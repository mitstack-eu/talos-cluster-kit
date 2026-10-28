package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Refusal refuses, before any change, a plane it cannot act on unambiguously.
func (p Plane) Refusal(st State) error {
	if dups := st.Duplicates(); len(dups) > 0 {
		return refuse("ambiguous-cluster",
			"the cluster is ambiguous, so nothing was changed: %s; delete the extra ones from the Hetzner project and retry", strings.Join(dups, "; "))
	}
	if _, ok := st.Network(); !ok && st.HasServers() {
		return refuse("network-gone",
			"servers carry this subscription's labels, but no network does, so nothing was changed; a new network would not hold their addresses, so unsubscribe and subscribe again to rebuild the cluster")
	}
	return nil
}

// EnsureNetwork creates the network when none carries the labels, which needs no wait, so it comes before any step.
func (p Plane) EnsureNetwork(ctx context.Context, st State) (Network, error) {
	if n, ok := st.Network(); ok {
		return n, nil
	}
	n, err := p.API.CreateNetwork(ctx, NetworkSpec{
		Name:    p.Name("network"),
		IPRange: NetworkIPRange,
		Labels:  p.Labels("network"),
		Subnet:  Subnet{NetworkZone: networkZone, IPRange: subnetIPRange},
		Routes:  []Route{{Destination: defaultRoute, Gateway: GatewayAddress}},
	})
	if err != nil {
		return Network{}, fmt.Errorf("creating the cluster network: %w", err)
	}
	return n, nil
}

// NextStep is the first step the plane lacks, each in the order it depends on the one before, and none while only
// Hetzner or the nodes can finish the work in flight.
func (p Plane) NextStep(ctx context.Context, st State, network Network) (Step, bool, error) {
	firewall, ok := st.Firewall()
	if !ok {
		return Step{"create the firewall", func(ctx context.Context) error { return p.createFirewall(ctx) }}, true, nil
	}
	for _, lb := range []struct {
		want    LoadBalancerSpec
		address string
	}{
		{p.apiLoadBalancer(network.ID), apiAddress},
		{p.ingressLoadBalancer(network.ID), ingressAddress},
	} {
		if s, ok := p.loadBalancerStep(st, lb.want, lb.address); ok {
			return s, true, nil
		}
	}
	return p.nodeStep(ctx, st, site{network: network, firewall: firewall})
}

type site struct {
	network    Network
	firewall   Firewall
	apiAddress string
}

// nodeStep builds the worker image beside the cluster's own, and deletes the worker builder of a cluster refused.
func (p Plane) nodeStep(ctx context.Context, st State, s site) (Step, bool, error) {
	step, ok, err := p.controlPlaneStep(ctx, st, s)
	var refused *Refusal
	if worker, has := st.Server(RoleWorkerBuilder); has && errors.As(err, &refused) {
		return Step{"delete the worker builder, whose disk holds the worker's join keys, beside a cluster that is refused", func(ctx context.Context) error {
			if derr := p.API.DeleteServer(ctx, worker.ID); derr != nil {
				return errors.Join(err, fmt.Errorf("deleting the worker builder %d: %w", worker.ID, derr))
			}
			return err
		}}, true, nil
	}
	if err != nil || ok {
		return step, ok, err
	}
	return p.workerImageStep(ctx, st, s)
}

func (p Plane) controlPlaneStep(ctx context.Context, st State, s site) (Step, bool, error) {
	gw, hasGateway := st.Server(RoleGateway)
	hasNode := len(st.Servers[RoleNode]) > 0
	builder, hasBuilder := st.Server(RoleBuilder)
	serving, hasServing := st.Server(RoleServing)
	snap, hasSnapshot := st.Snapshot()
	imaged := hasSnapshot && snap.Status == SnapshotAvailable
	switch {
	case hasNode && !hasGateway:
		return Step{}, false, refuse("gateway-lost",
			"the cluster's nodes are running, but no gateway carries this subscription's labels, so no node can pull an image and nothing was changed; the gateway's certificate authority is gone with it, so unsubscribe and subscribe again to rebuild the cluster")
	case hasServing && (!hasBuilder || hasSnapshot || p.overdue(builder)):
		return Step{"delete the serving server, whose file holds the hosting set's keys, once the builder is done with it", func(ctx context.Context) error {
			return p.API.DeleteServer(ctx, serving.ID)
		}}, true, nil
	case hasBuilder && (hasNode || imaged):
		return Step{"delete the builder, whose disk holds every key of the cluster, beside its snapshot", func(ctx context.Context) error {
			return p.API.DeleteServer(ctx, builder.ID)
		}}, true, nil
	case hasNode:
		return p.missingNodeStep(st, s, snap)
	case !hasGateway:
		return p.installStep(st, s), true, nil
	case gw.PrivateIPs[s.network.ID] == "":
		return Step{"attach the gateway to the network at its fixed address before its first boot", func(ctx context.Context) error {
			return p.API.AttachServerToNetwork(ctx, gw.ID, s.network.ID, GatewayAddress)
		}}, true, nil
	case hasBuilder && builder.PrivateIPs[s.network.ID] == "":
		return Step{"attach the builder to the network at its fixed address", func(ctx context.Context) error {
			return p.API.AttachServerToNetwork(ctx, builder.ID, s.network.ID, BuilderAddress)
		}}, true, nil
	case hasServing && serving.PrivateIPs[s.network.ID] == "":
		return Step{"attach the serving server to the network at its fixed address", func(ctx context.Context) error {
			return p.API.AttachServerToNetwork(ctx, serving.ID, s.network.ID, ServingAddress)
		}}, true, nil
	case gw.Status == ServerOff:
		return Step{"power on the gateway", func(ctx context.Context) error { return p.API.PowerOnServer(ctx, gw.ID) }}, true, nil
	case imaged:
		return p.missingNodeStep(st, s, snap)
	case hasSnapshot:
		return Step{}, false, nil
	case hasServing && serving.Status == ServerOff:
		return Step{"power on the serving server before the builder fetches from it", func(ctx context.Context) error {
			return p.API.PowerOnServer(ctx, serving.ID)
		}}, true, nil
	case hasBuilder:
		return p.builderStep(ctx, builder, clusterImage)
	default:
		return Step{}, false, refuse("cluster-secrets-lost",
			"the gateway exists but neither the builder nor the cluster's snapshot does, so the cluster's keys, which only they held, are gone; unsubscribe and subscribe again to rebuild the cluster")
	}
}

// workerImageStep builds the worker image once the gateway the worker builder routes through runs, and deletes the
// builder, whose disk holds the worker's join keys, beside the snapshot.
func (p Plane) workerImageStep(ctx context.Context, st State, s site) (Step, bool, error) {
	builder, hasBuilder := st.Server(RoleWorkerBuilder)
	snap, hasSnapshot := st.WorkerSnapshot()
	gw, hasGateway := st.Server(RoleGateway)
	switch {
	case !hasBuilder:
		return Step{}, false, nil
	case hasSnapshot && snap.Status == SnapshotAvailable:
		return Step{"delete the worker builder beside its snapshot", func(ctx context.Context) error {
			return p.API.DeleteServer(ctx, builder.ID)
		}}, true, nil
	case hasSnapshot:
		return Step{}, false, nil
	case builder.PrivateIPs[s.network.ID] == "":
		return Step{"attach the worker builder to the network at its fixed address", func(ctx context.Context) error {
			return p.API.AttachServerToNetwork(ctx, builder.ID, s.network.ID, WorkerBuilderAddress)
		}}, true, nil
	case !hasGateway || gw.Status != ServerRunning:
		return Step{}, false, nil
	default:
		return p.builderStep(ctx, builder, workerImage)
	}
}

// installStep first deletes what is left of a cluster whose gateway is gone, since the keys it holds match no gateway.
func (p Plane) installStep(st State, s site) Step {
	var servers []Server
	for _, r := range []Role{RoleBuilder, RoleServing, RoleWorkerBuilder} {
		servers = append(servers, st.Servers[r]...)
	}
	snapshots := append(slices.Clone(st.Snapshots), st.WorkerSnapshots...)
	if len(servers) == 0 && len(snapshots) == 0 {
		api, _ := st.LoadBalancerIn(RoleAPI, GatewayLocation)
		s.apiAddress = api.PrivateIPs[s.network.ID]
		return Step{"create the gateway and the builders", func(ctx context.Context) error { return p.install(ctx, s) }}
	}
	return Step{"delete the builders and snapshots of a cluster with no gateway", func(ctx context.Context) error {
		for _, server := range servers {
			if err := p.API.DeleteServer(ctx, server.ID); err != nil {
				return fmt.Errorf("deleting the %s %d of a cluster with no gateway: %w", server.Labels[KeyRole], server.ID, err)
			}
		}
		for _, snap := range snapshots {
			if err := p.API.DeleteSnapshot(ctx, snap.ID); err != nil {
				return fmt.Errorf("deleting the snapshot %d of a cluster with no gateway: %w", snap.ID, err)
			}
		}
		return nil
	}}
}

// imageBuild names what a builder builds: the snapshot's role, what a message calls it and the class of its failure.
type imageBuild struct {
	role                   Role
	builder, what, failure string
}

var (
	clusterImage = imageBuild{RoleImage, "builder", "the cluster's image", "image-build-failed"}
	workerImage  = imageBuild{RoleWorkerImage, "worker builder", "the worker image", "worker-image-build-failed"}
)

func (p Plane) overdue(builder Server) bool {
	return builder.Status == ServerRunning && p.now().Sub(builder.Created) >= BuildDeadline
}

// builderStep powers on a builder that has never run, snapshots one that ran and powered itself off, and deletes one
// still running past BuildDeadline, since its script leaves a failed build running and only the deadline tells.
func (p Plane) builderStep(ctx context.Context, builder Server, img imageBuild) (Step, bool, error) {
	switch builder.Status {
	case ServerRunning:
		if !p.overdue(builder) {
			return Step{}, false, nil
		}
		return Step{"delete the " + img.builder + " that did not finish", func(ctx context.Context) error {
			metrics := p.readBuilderMetrics(ctx, builder)
			if err := p.API.DeleteServer(ctx, builder.ID); err != nil {
				return fmt.Errorf("deleting the %s %d that did not finish: %w", img.builder, builder.ID, err)
			}
			return refuse(img.failure,
				"the %s did not power itself off within %s of its creation, so %s was not built or the gateway's mirror never completed, and the %s is deleted; %s; unsubscribe and subscribe again to rebuild the cluster",
				img.builder, BuildDeadline, img.what, img.builder, metrics.stall())
		}}, true, nil
	case ServerOff:
		started, err := p.API.ServerStarted(ctx, builder.ID)
		if err != nil {
			return Step{}, false, err
		}
		if !started {
			return Step{"power on the " + img.builder, func(ctx context.Context) error { return p.API.PowerOnServer(ctx, builder.ID) }}, true, nil
		}
		return Step{"start the snapshot of the " + img.builder + "'s disk", func(ctx context.Context) error {
			_, err := p.API.StartSnapshot(ctx, builder.ID, SnapshotSpec{Description: p.Name(img.role), Labels: p.Labels(img.role)})
			return err
		}}, true, nil
	default:
		return Step{}, false, nil
	}
}

// missingNodeStep creates the node of the first location that has none, one node a call, once the snapshot is available.
func (p Plane) missingNodeStep(st State, s site, snap Snapshot) (Step, bool, error) {
	if snap.Status != SnapshotAvailable {
		return Step{}, false, nil
	}
	for _, loc := range Locations {
		if _, ok := st.NodeIn(loc); !ok {
			return Step{"create the node in " + loc + " from the snapshot", func(ctx context.Context) error {
				return p.createNode(ctx, s, snap, loc)
			}}, true, nil
		}
	}
	return Step{}, false, nil
}

// createNode boots a node from the cluster's own snapshot, with no user data and no public address, named for its location.
func (p Plane) createNode(ctx context.Context, s site, snap Snapshot, loc string) error {
	_, err := p.API.CreateServers(ctx, ServerSpec{
		Name:        p.LocatedName(RoleNode, loc),
		ServerType:  p.ServerType,
		Location:    loc,
		ImageID:     snap.ID,
		Labels:      p.Labels(RoleNode),
		NetworkIDs:  []int64{s.network.ID},
		FirewallIDs: []int64{s.firewall.ID},
	})
	if err != nil {
		return fmt.Errorf("creating the cluster node in %s from snapshot %d: %w", loc, snap.ID, err)
	}
	return nil
}

func (p Plane) createFirewall(ctx context.Context) error {
	if _, err := p.API.CreateFirewall(ctx, FirewallSpec{Name: p.Name("firewall"), Labels: p.Labels("firewall")}); err != nil {
		return fmt.Errorf("creating the cluster firewall: %w", err)
	}
	return nil
}

// apiLoadBalancer carries the Kubernetes API on the private network alone, with no public interface, to every node.
func (p Plane) apiLoadBalancer(networkID int64) LoadBalancerSpec {
	return LoadBalancerSpec{
		Name:           p.LocatedName(RoleAPI, GatewayLocation),
		Type:           loadBalancerType,
		Location:       GatewayLocation,
		Labels:         p.Labels(RoleAPI),
		NetworkID:      networkID,
		Services:       []Service{{ListenPort: APIServerPort, DestinationPort: APIServerPort}},
		TargetSelector: p.RoleSelector(RoleNode),
	}
}

// ingressLoadBalancer serves ports 80 and 443 on its public address to every node's edge NodePorts, with PROXY protocol.
func (p Plane) ingressLoadBalancer(networkID int64) LoadBalancerSpec {
	return LoadBalancerSpec{
		Name:            p.LocatedName(RoleIngress, GatewayLocation),
		Type:            loadBalancerType,
		Location:        GatewayLocation,
		Labels:          p.Labels(RoleIngress),
		NetworkID:       networkID,
		PublicInterface: true,
		Services: []Service{
			{ListenPort: httpPort, DestinationPort: httpNodePort, ProxyProtocol: true},
			{ListenPort: httpsPort, DestinationPort: httpsNodePort, ProxyProtocol: true},
		},
		TargetSelector: p.RoleSelector(RoleNode),
	}
}

// loadBalancerStep is the next step the load balancer lacks. It is attached before its public interface is disabled or
// its private target added, since Hetzner refuses either on a load balancer not yet attached to a network.
func (p Plane) loadBalancerStep(st State, want LoadBalancerSpec, address string) (Step, bool) {
	what := want.Labels[KeyRole] + " load balancer in " + want.Location
	lb, ok := st.LoadBalancerIn(Role(want.Labels[KeyRole]), want.Location)
	switch {
	case !ok:
		return Step{"create the " + what, func(ctx context.Context) error { return p.createLoadBalancer(ctx, want) }}, true
	case lb.PrivateIPs[want.NetworkID] == "":
		return Step{"attach the " + what + " to the network", func(ctx context.Context) error {
			if err := p.API.AttachLoadBalancerToNetwork(ctx, lb.ID, want.NetworkID, address); err != nil {
				return fmt.Errorf("attaching the cluster's %s: %w", what, err)
			}
			return nil
		}}, true
	case lb.PublicInterface && !want.PublicInterface:
		return Step{"disable the " + what + "'s public interface", func(ctx context.Context) error {
			return p.API.DisableLoadBalancerPublicInterface(ctx, lb.ID)
		}}, true
	}
	for _, s := range want.Services {
		if !slices.ContainsFunc(lb.Services, func(have Service) bool { return have.ListenPort == s.ListenPort }) {
			return Step{fmt.Sprintf("add port %d to the %s", s.ListenPort, what), func(ctx context.Context) error {
				return p.API.AddLoadBalancerService(ctx, lb.ID, s)
			}}, true
		}
	}
	if !slices.Contains(lb.TargetSelectors, want.TargetSelector) {
		return Step{"target the nodes from the " + what, func(ctx context.Context) error {
			return p.API.AddLoadBalancerTarget(ctx, lb.ID, want.TargetSelector)
		}}, true
	}
	return Step{}, false
}

// createLoadBalancer creates one bare, with its public interface on, since Hetzner's create takes no address on the network.
func (p Plane) createLoadBalancer(ctx context.Context, want LoadBalancerSpec) error {
	_, err := p.API.CreateLoadBalancer(ctx, LoadBalancerSpec{
		Name: want.Name, Type: want.Type, Location: want.Location, Labels: want.Labels, PublicInterface: true,
	})
	if err != nil {
		return fmt.Errorf("creating the cluster's %s load balancer in %s: %w", want.Labels[KeyRole], want.Location, err)
	}
	return nil
}
