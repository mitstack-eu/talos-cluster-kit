// Package nodeconfig renders the Talos machine configuration a cluster's disk image embeds.
package nodeconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"

	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
)

// Profile names the imager profile a cluster's image is built with, and so the platform its nodes run on.
type Profile string

const (
	// HCloud is a Hetzner Cloud server, which boots from a snapshot and names itself from Hetzner's metadata service.
	HCloud Profile = "hcloud"
	// Metal is a Hetzner dedicated server, whose image is written from the rescue system and which has no metadata service.
	Metal Profile = "metal"
)

// apiServerPort is the port every private load balancer carries the Kubernetes API on.
const apiServerPort = 6443

// SubscriptionLabel is the node label naming the subscription that owns the node.
const SubscriptionLabel = "mitstack.dev/subscription"

// Cluster is what every node of one cluster shares.
type Cluster struct {
	// Name is the Kubernetes cluster name.
	Name string
	// SubscriptionID labels every node and every log line.
	SubscriptionID string
	Profile        Profile
	Components     components.Set
	Secrets        *Secrets
	// Network is the cluster's own Hetzner private network, which etcd and the kubelet bind to. On the HCloud profile
	// the block gives it a route of 0.0.0.0/0 to a gateway, since a Cloud node's default route leads there.
	Network netip.Prefix
	// APIAddresses are the private load balancers that carry the Kubernetes API, one per location, the first of
	// them the control-plane endpoint, and the API server's certificate names them all.
	APIAddresses []netip.Addr
	// Gateways are the gateways' private addresses, one per location; every one serves the image mirror.
	Gateways []netip.Addr
	// GatewayCA is the PEM certificate the gateways' TLS certificates chain to.
	GatewayCA []byte
	// Logs is where the node-local Collector exports the node's logs; with none, the node runs no Collector.
	Logs components.LogExport
}

// Node is what one node's configuration adds to its cluster's.
type Node struct {
	// Hostname names a Metal node, which has no metadata service to name it, and stays empty on a Cloud node.
	Hostname string
}

// Render returns the multi-document machine configuration of a control-plane node, validated against Talos's own schema.
func Render(c Cluster, n Node) ([]byte, error) {
	if err := c.validate(n); err != nil {
		return nil, err
	}
	base, err := c.generate()
	if err != nil {
		return nil, err
	}
	extra, err := c.documents(n)
	if err != nil {
		return nil, err
	}
	cfg, err := container.New(append(c.adapt(base.Documents()), extra...)...)
	if err != nil {
		return nil, fmt.Errorf("assemble the configuration of cluster %s: %w", c.Name, err)
	}
	if _, err := cfg.Validate(imageMode{}); err != nil {
		return nil, fmt.Errorf("validate the configuration of cluster %s: %w", c.Name, err)
	}
	return cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
}

// generate returns Talos's own control-plane configuration for the cluster, before the kit adapts it.
func (c Cluster) generate() (machineryconfig.Provider, error) {
	sans := make([]string, 0, len(c.APIAddresses))
	for _, addr := range c.APIAddresses {
		sans = append(sans, addr.String())
	}
	input, err := generate.NewInput(c.Name, c.endpoint(), c.Components.Versions.Kubernetes,
		generate.WithVersionContract(versionContract),
		generate.WithSecretsBundle(c.Secrets.bundle),
		generate.WithClusterDiscovery(false),
		generate.WithAllowSchedulingOnControlPlanes(true),
		generate.WithAdditionalSubjectAltNames(sans),
	)
	if err != nil {
		return nil, fmt.Errorf("prepare the configuration of cluster %s: %w", c.Name, err)
	}
	cfg, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("generate the configuration of cluster %s: %w", c.Name, err)
	}
	return cfg, nil
}

func (c Cluster) endpoint() string {
	return "https://" + netip.AddrPortFrom(c.APIAddresses[0], apiServerPort).String()
}

var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

func (c Cluster) validate(n Node) error {
	var errs []error
	if c.Secrets == nil {
		errs = append(errs, errors.New("the cluster has no secrets"))
	}
	if !labelValueRe.MatchString(c.SubscriptionID) {
		errs = append(errs, fmt.Errorf("subscription id %q is not a Kubernetes label value", c.SubscriptionID))
	}
	if !c.Network.IsValid() {
		errs = append(errs, errors.New("the cluster has no private network"))
	}
	if len(c.APIAddresses) == 0 || len(c.Gateways) == 0 {
		errs = append(errs, errors.New("the cluster needs at least one API address and one gateway"))
	}
	if err := check.PEMCertificate(c.GatewayCA); err != nil {
		errs = append(errs, fmt.Errorf("gateway CA: %w", err))
	}
	if err := c.validateNode(n); err != nil {
		errs = append(errs, err)
	}
	if errs != nil {
		return fmt.Errorf("cluster %q: %w", c.Name, errors.Join(errs...))
	}
	return nil
}

func (c Cluster) validateNode(n Node) error {
	switch c.Profile {
	case HCloud:
		if n.Hostname != "" {
			return fmt.Errorf("a Cloud node takes its name from Hetzner's metadata service, so hostname %q is refused", n.Hostname)
		}
	case Metal:
		if err := check.DNS1123Label(n.Hostname); err != nil {
			return fmt.Errorf("a dedicated node needs its own hostname: %w", err)
		}
	default:
		return fmt.Errorf("profile %q is neither %q nor %q", c.Profile, HCloud, Metal)
	}
	return nil
}

// imageMode validates a configuration that boots from a disk image, which needs no install section.
type imageMode struct{}

func (imageMode) String() string        { return "cloud" }
func (imageMode) RequiresInstall() bool { return false }
func (imageMode) InContainer() bool     { return false }

// replacedKinds are the generated documents the kit drops, because documents() writes its own in their place
// or because the cluster runs without them: Flannel and Sidero's discovery service.
var replacedKinds = []string{"KubeFlannelCNIConfig", "DiscoveryServiceConfig", "ResolverConfig", "HostnameConfig"}

func keepGenerated(doc config.Document) bool {
	return !slices.Contains(replacedKinds, doc.Kind())
}
