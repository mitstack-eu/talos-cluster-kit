// Package controlplane builds and deletes a Talos cluster's control plane on Hetzner Cloud: three nodes booted from
// the cluster's own snapshot, a gateway in each location, two load balancers, a firewall and a network, all found by
// their labels.
package controlplane

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// Plane is one subscription's control plane as a block names it. It keeps no state between calls, so each call
// takes the cluster on from wherever an earlier one left it.
type Plane struct {
	API API
	// Block is the block's id, which every resource carries as its mitstack.dev/block label.
	Block        string
	Subscription string
	AppName      string
	// ServerTypes are the control-plane nodes' and builders' types by preference, so one sold-out type does not stop
	// a node: each server takes the first its location can order when it is created.
	ServerTypes []string
	// Boundary separates the parts of the gateway's user data.
	Boundary string
	// PlatformURL is where every node enrols and reports.
	PlatformURL string
	// ReportProofScript signs each node report, as report.Inputs.ProofScript.
	ReportProofScript string
	// RepoURL is the cluster repository Argo CD syncs.
	RepoURL string
	// Now is the clock the builder's deadline is judged against; nil reads time.Now.
	Now func() time.Time
	// Installed receives the cluster the install rendered, its keys included, and the gateway CA, in the one call they
	// exist, so the block can install its workers and keep the CA as its own secret. An error rolls the install back.
	Installed func(ctx context.Context, network Network, cluster nodeconfig.Cluster, ca gateway.CA) error
	// GatewayCA is the CA the block keeps, which signs each replacement gateway's certificate; nil when it keeps none.
	GatewayCA *gateway.CA
	// Upgrading starts replacing each gateway built from an earlier release's pinned list, one location at a time.
	Upgrading bool
	// ReadGateway reads the gateway at address from the newest node report received after since; nil reads none.
	ReadGateway func(ctx context.Context, address string, since time.Time) (GatewayReading, error)
	// ReadReport reads the newest node report received after since, nil when none was; a nil ReadReport reads none.
	ReadReport func(ctx context.Context, since time.Time) (*report.Read, error)
	// Guard is block-kit's redundancy guard, which judges every removal of a gateway or a Cloud worker; nil removes none.
	Guard redundant.Guard
	// Notify receives each event a step records: a certificate issued, a route moved, a gateway deleted or powered on.
	Notify func(ctx context.Context, detail string)
	// AppsOnWorkers keeps every pod without a toleration off the control-plane nodes, and the ingress load balancer's
	// traffic with them, so it targets each of WorkerAddresses and no control-plane node.
	AppsOnWorkers bool
	// WorkerAddresses are the private addresses of the workers outside Hetzner Cloud, each inside a vSwitch subnet of
	// the network.
	WorkerAddresses []string
	// Hosting makes the cluster host the platform; nil builds one that hosts none.
	Hosting *Hosting
	// ReplaceNodes are the locations whose control-plane node a confirmed upgrade replaces, which the upgrade's own call
	// records on the network, so the calls after it carry the replacement on and name none.
	ReplaceNodes []string
	// Removals is the cluster repository a replaced node's member removal is written to; nil refuses an upgrade that
	// names a node.
	Removals *Removals
	Records  []Record
	Signers  []components.Signer
}

// Record adds to the network's labels what an upgrade asks of a part beside the plane, inside the upgrade's one write.
type Record func(ctx context.Context, p Plane, st State, labels map[string]string) (replaces string, err error)

// Refusal is a request the plane will not carry out, which no retry of the same call fixes. Class is
// machine-readable, and the block answers it as its own framework's refusal.
type Refusal struct {
	Class, Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(class, format string, args ...any) error {
	return &Refusal{Class: class, Message: fmt.Sprintf(format, args...)}
}

// Step is one change the plane lacks, which waits on Hetzner at most once, or twice for a route move's delete and add.
type Step struct {
	What string
	Run  func(ctx context.Context) error
}

// Take logs how long the step ran, so a live run shows each call's share of the verb timeout.
func (s Step) Take(ctx context.Context, block, subID string) error {
	start := time.Now()
	err := s.Run(ctx)
	outcome := "took"
	if err != nil {
		outcome = "failed after"
	}
	log.Printf("%s: subscription %s: the step to %s %s %s", block, subID, s.What, outcome, time.Since(start).Round(time.Millisecond))
	return err
}

func (p Plane) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

const (
	DefaultServerType = "cx33"
	gatewayImage      = "ubuntu-24.04"
	// builderImage is Hetzner's Ubuntu image with Docker installed, so the builder runs imager without a package install.
	builderImage     = "docker-ce"
	loadBalancerType = "lb11"
	// NetworkZone is the Hetzner network zone every location of the cluster sits in, which names each node's region.
	NetworkZone    = "eu-central"
	NetworkIPRange = "10.0.0.0/16"
	subnetIPRange  = "10.0.0.0/24"
	// routerAddress is the first address of the network, which Hetzner's own router answers on.
	routerAddress = "10.0.0.1"
	// GatewayAddress is the first location's gateway, which the builder's mirror and the etcd bootstrap name.
	GatewayAddress = "10.0.0.2"
	defaultRoute   = "0.0.0.0/0"

	APIServerPort = 6443
	httpPort      = 80
	httpsPort     = 443
	// The public load balancer forwards to the NodePorts the edge listens on, which ADR 0076 fixes.
	httpNodePort  = 30080
	httpsNodePort = 30081

	// BuildDeadline bounds the builder from its creation to powering itself off once its image and the mirror are done.
	BuildDeadline = 60 * time.Minute
)

// GatewayServerTypes are the gateway's types by preference, so one sold-out type does not stop a cluster.
var GatewayServerTypes = []string{"cx23", "cpx22"}

// Locations hold a node and a gateway each; the first also holds the builder and both load balancers.
var Locations = []string{"fsn1", "nbg1", "hel1"}

// GatewayLocation is where the first gateway, the builder and both load balancers run.
var GatewayLocation = Locations[0]

// GatewayAddresses fix each location's gateway, since a replacement takes the address every node names, with the others
// high in the subnet, above any address Hetzner gives a server joining with none.
var GatewayAddresses = map[string]string{"fsn1": GatewayAddress, "nbg1": "10.0.0.248", "hel1": "10.0.0.249"}

func gatewayAddrs() []netip.Addr {
	out := make([]netip.Addr, 0, len(Locations))
	for _, loc := range Locations {
		out = append(out, netip.MustParseAddr(GatewayAddresses[loc]))
	}
	return out
}

func (p Plane) notify(ctx context.Context, format string, args ...any) {
	if p.Notify != nil {
		p.Notify(ctx, fmt.Sprintf(format, args...))
	}
}

// The load balancers join the network at fixed addresses before the gateway, so neither takes the gateway's.
const (
	apiAddress     = "10.0.0.3"
	ingressAddress = "10.0.0.4"
)

// The builders and the serving server join at fixed addresses high in the subnet, so the serving server knows the
// one client it serves, and none takes another's address or a node's, whatever order they attach in.
const (
	BuilderAddress       = "10.0.0.250"
	ServingAddress       = "10.0.0.251"
	WorkerBuilderAddress = "10.0.0.252"
)

// ServerTypes are x86, since the image is amd64, hold a control plane with Cilium and Argo CD, and are each one the
// platform's hetzner.server type allows.
var ServerTypes = []string{"cx23", "cpx22", "cx33", "cx43", "cx53", "cpx32", "cpx52", "cpx62"}
