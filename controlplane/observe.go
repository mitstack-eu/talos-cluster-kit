package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Problems names every way the plane falls short of what it builds, judged from what Hetzner holds.
func (p Plane) Problems(ctx context.Context, st State) []string {
	missing := st.Duplicates()
	if len(missing) > 0 {
		return missing
	}
	network, networkOK := st.Network()
	firewall, firewallOK := st.Firewall()
	if !networkOK {
		missing = append(missing, "no network carries this subscription's labels")
	}
	if !firewallOK {
		missing = append(missing, "no firewall carries this subscription's labels")
	}
	if builder, ok := st.Server(RoleBuilder); ok {
		missing = append(missing, p.buildDetail(ctx, builder, clusterImage))
	}
	if snap, ok := st.Snapshot(); !ok {
		missing = append(missing, "no snapshot of the cluster's image carries this subscription's labels")
	} else if snap.Status != SnapshotAvailable {
		missing = append(missing, fmt.Sprintf("snapshot %d of the cluster's image is %s", snap.ID, snap.Status))
	}
	if serving, ok := st.Server(RoleServing); ok {
		missing = append(missing, fmt.Sprintf("serving server %d still holds the hosting set", serving.ID))
	}
	if builder, ok := st.Server(RoleWorkerBuilder); ok {
		missing = append(missing, p.buildDetail(ctx, builder, workerImage))
	}
	if snap, ok := st.WorkerSnapshot(); ok && snap.Status != SnapshotAvailable {
		missing = append(missing, fmt.Sprintf("snapshot %d of the worker image is %s", snap.ID, snap.Status))
	}
	if firewallOK && firewall.RuleCount > 0 {
		missing = append(missing, fmt.Sprintf("firewall %d holds %d rules, and the cluster's firewall holds none", firewall.ID, firewall.RuleCount))
	}
	if !networkOK || !firewallOK {
		return missing
	}
	gateways, err := p.gatewayProblems(ctx, st, network, firewall)
	if err != nil {
		return append(missing, "the gateways cannot be judged: "+err.Error())
	}
	missing = append(missing, gateways...)
	missing = append(missing, st.nodeProblems(network, firewall)...)
	missing = append(missing, p.loadBalancerProblems(st, network)...)
	return missing
}

// buildDetail says where a builder stands, judged against BuildDeadline as builderStep judges it.
func (p Plane) buildDetail(ctx context.Context, b Server, img imageBuild) string {
	created := b.Created.UTC().Format(time.RFC3339)
	if b.Status != ServerRunning {
		return fmt.Sprintf("%s %d, created at %s, is %s", img.builder, b.ID, created, b.Status)
	}
	state := "is building " + img.what + " or waiting for the gateway's mirror to complete"
	if p.overdue(b) {
		state = fmt.Sprintf("did not power itself off within %s", BuildDeadline)
	}
	return fmt.Sprintf("%s %d, created at %s, %s; %s", img.builder, b.ID, created, state, p.readBuilderMetrics(ctx, b).summary())
}

// gatewayProblems names each location without a gateway as the plane builds it, a default route through none that
// serves, and an upgrade's replacement still to run or waiting on a gateway. A gateway down after its power-back is no
// problem here, since no step is left for it, and Unhealthy names it.
func (p Plane) gatewayProblems(ctx context.Context, st State, network Network, firewall Firewall) ([]string, error) {
	locations := Locations
	if st.SingleGateway() {
		locations = []string{GatewayLocation}
	}
	var missing []string
	for _, loc := range locations {
		gw, ok := st.GatewayIn(loc)
		if !ok {
			missing = append(missing, "no gateway in "+loc+" carries this subscription's labels")
			continue
		}
		down, _, err := p.downAfterPowerOn(ctx, gw)
		if err != nil {
			return nil, err
		}
		if gw.Status != ServerRunning && !down {
			missing = append(missing, fmt.Sprintf("gateway %d in %s is %s", gw.ID, loc, gw.Status))
		}
		if gw.PrivateIPs[network.ID] != GatewayAddresses[loc] {
			missing = append(missing, fmt.Sprintf("gateway %d is not on network %d at %s", gw.ID, network.ID, GatewayAddresses[loc]))
		}
		if gw.PublicIPv4 == "" {
			missing = append(missing, fmt.Sprintf("gateway %d has no public address", gw.ID))
		}
		if !slices.Contains(gw.FirewallIDs, firewall.ID) {
			missing = append(missing, fmt.Sprintf("firewall %d is not applied to gateway %d", firewall.ID, gw.ID))
		}
		if gw.Labels[KeyIssuerBucket] == "" {
			missing = append(missing, fmt.Sprintf("gateway %d has no %s label, so the block cannot read the issuer its nodes sign for", gw.ID, KeyIssuerBucket))
		}
	}
	route, err := p.routeProblem(ctx, st, network)
	if err != nil || route != "" {
		return append(missing, route), err
	}
	replacing, err := p.replacementProblem(ctx, st, network)
	if replacing != "" {
		missing = append(missing, replacing)
	}
	return missing, err
}

func (p Plane) routeProblem(ctx context.Context, st State, network Network) (string, error) {
	route, ok := defaultRouteOf(network)
	if !ok {
		return fmt.Sprintf("network %d has no default route", network.ID), nil
	}
	gw, ok := st.gatewayAt(route.Gateway)
	if !ok {
		return fmt.Sprintf("network %d's default route goes to %s, where no gateway of this subscription is", network.ID, route.Gateway), nil
	}
	if failing, err := p.readFailing(ctx, gw, network); err != nil || !failing {
		return "", err
	}
	return fmt.Sprintf("network %d's default route goes to gateway %d in %s, which does not serve", network.ID, gw.ID, gw.Location), nil
}

// replacementProblem names the gateway an upgrade replaces next, or the gateway built from this release's list that no
// report has read serving yet, naming the newest report's receivedAt.
func (p Plane) replacementProblem(ctx context.Context, st State, network Network) (string, error) {
	if st.SingleGateway() {
		return "", nil
	}
	r, err := p.replacement(st, network)
	if err != nil || !r.active {
		return "", err
	}
	waiting, reading, ok, err := p.waitingOn(ctx, r, network)
	switch {
	case err != nil:
		return "", err
	case ok:
		return fmt.Sprintf("gateway %d in %s, built from this release's list, has not been read serving since its creation at %s, so the upgrade waits on it: %s",
			waiting.ID, waiting.Location, waiting.Created.UTC().Format(time.RFC3339), reading.Detail), nil
	case len(r.stale) > 0:
		next, err := p.nextToReplace(ctx, r, network)
		return fmt.Sprintf("gateway %d in %s was built from an earlier list, and the upgrade replaces it next", next.ID, next.Location), err
	}
	return fmt.Sprintf("every gateway serves from this release's list, and network %d still carries the upgrade's mark", network.ID), nil
}

func (st State) nodeProblems(network Network, firewall Firewall) []string {
	var missing []string
	for _, loc := range Locations {
		node, ok := st.NodeIn(loc)
		if !ok {
			missing = append(missing, "no node in "+loc+" carries this subscription's labels")
			continue
		}
		if node.PrivateIPs[network.ID] == "" {
			missing = append(missing, fmt.Sprintf("node %d has no address on network %d", node.ID, network.ID))
		}
		if node.PublicIPv4 != "" {
			missing = append(missing, fmt.Sprintf("node %d holds the public address %s, and only the gateway may", node.ID, node.PublicIPv4))
		}
		if !slices.Contains(node.FirewallIDs, firewall.ID) {
			missing = append(missing, fmt.Sprintf("firewall %d is not applied to node %d", firewall.ID, node.ID))
		}
	}
	return missing
}

func (p Plane) loadBalancerProblems(st State, network Network) []string {
	var missing []string
	if p.AppsOnWorkers && len(p.WorkerAddresses) == 0 {
		missing = append(missing, "apps run on workers, but the plane names no worker address for the ingress load balancer to target")
	}
	for _, want := range []LoadBalancerSpec{p.apiLoadBalancer(network.ID), p.ingressLoadBalancer(network.ID)} {
		role := want.Labels[KeyRole]
		found, ok := st.LoadBalancerIn(Role(role), GatewayLocation)
		if !ok {
			missing = append(missing, "no "+role+" load balancer in "+GatewayLocation+" carries this subscription's labels")
			continue
		}
		missing = append(missing, reachProblems(found, want, network)...)
	}
	return missing
}

func reachProblems(lb LoadBalancer, want LoadBalancerSpec, network Network) []string {
	var missing []string
	if lb.PrivateIPs[network.ID] == "" {
		missing = append(missing, fmt.Sprintf("load balancer %d is not on network %d", lb.ID, network.ID))
	}
	if lb.PublicInterface != want.PublicInterface {
		missing = append(missing, fmt.Sprintf("load balancer %d's public interface is %s, want %s", lb.ID, onOff(lb.PublicInterface), onOff(want.PublicInterface)))
	}
	if want.TargetSelector != "" && !slices.Contains(lb.TargetSelectors, want.TargetSelector) {
		missing = append(missing, fmt.Sprintf("load balancer %d does not target the subscription's nodes by label", lb.ID))
	}
	for _, ip := range want.TargetIPs {
		if !slices.Contains(lb.TargetIPs, ip) {
			missing = append(missing, fmt.Sprintf("load balancer %d does not target the worker at %s", lb.ID, ip))
		}
	}
	for _, sel := range lb.TargetSelectors {
		if sel != want.TargetSelector {
			missing = append(missing, fmt.Sprintf("load balancer %d still targets the servers %s selects", lb.ID, sel))
		}
	}
	return missing
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// EnrolmentDigest is the digest a node's enrolment is judged against, which every gateway carries as a label from the
// install on, or a reason it cannot be read; both are empty before the install.
func (st State) EnrolmentDigest() (digest, gap string) {
	gw, ok := st.SharedGateway()
	switch {
	case !ok:
		return "", ""
	case gw.Labels[KeyEnrolmentSHA256] == "":
		return "", fmt.Sprintf("gateway %d has no %s label, so the cluster carries no enrolment outputs", gw.ID, KeyEnrolmentSHA256)
	}
	return gw.Labels[KeyEnrolmentSHA256], ""
}

// Outputs are what a present plane gives its block's provided type, with the detail that says what stands.
func (p Plane) Outputs(st State) (map[string]string, string) {
	network, _ := st.Network()
	gw, _ := st.SharedGateway()
	endpoint, _ := st.LoadBalancerIn(RoleAPI, GatewayLocation)
	ingress, _ := st.LoadBalancerIn(RoleIngress, GatewayLocation)
	var nodes []string
	for _, loc := range Locations {
		node, _ := st.NodeIn(loc)
		nodes = append(nodes, fmt.Sprintf("%d in %s at %s", node.ID, loc, node.PrivateIPs[network.ID]))
	}
	byLocation, _ := json.Marshal(map[string]string{GatewayLocation: ingress.PublicIPv4})
	outputs := map[string]string{
		"enrolmentTokenSha256": gw.Labels[KeyEnrolmentSHA256],
		"privateEndpoint":      "https://" + endpoint.PrivateIPs[network.ID] + ":" + strconv.Itoa(APIServerPort),
		"publicIpByLocation":   string(byLocation),
		"subscriptionId":       p.Subscription,
		"issuerBucket":         gw.Labels[KeyIssuerBucket],
		"issuerUrl":            issuerURL(gw.Labels[KeyIssuerBucket], p.Subscription).String(),
	}
	reach := "an API and an ingress load balancer in " + GatewayLocation + " reach them"
	if p.AppsOnWorkers {
		reach = "an API load balancer in " + GatewayLocation + " reaches them, and its ingress load balancer the workers at " +
			strings.Join(p.WorkerAddresses, ", ")
	}
	var gateways []string
	for _, g := range st.Servers[RoleGateway] {
		gateways = append(gateways, fmt.Sprintf("%d in %s", g.ID, g.Location))
	}
	route, _ := defaultRouteOf(network)
	detail := fmt.Sprintf("nodes %s are on network %d behind gateways %s, its default route via %s, and %s",
		strings.Join(nodes, ", "), network.ID, strings.Join(gateways, ", "), route.Gateway, reach)
	return outputs, detail
}

// Delete deletes servers and load balancers before the firewall and network they use, and the snapshot that holds
// every key of the cluster with them.
func (p Plane) Delete(ctx context.Context) error {
	st, err := p.Read(ctx)
	if err != nil {
		return err
	}
	for _, servers := range st.Servers {
		for _, s := range servers {
			if err := p.API.DeleteServer(ctx, s.ID); err != nil {
				return fmt.Errorf("deleting the cluster server %d: %w", s.ID, err)
			}
		}
	}
	for _, lbs := range st.LoadBalancers {
		for _, lb := range lbs {
			if err := p.API.DeleteLoadBalancer(ctx, lb.ID); err != nil {
				return fmt.Errorf("deleting the cluster load balancer %d: %w", lb.ID, err)
			}
		}
	}
	for _, snap := range append(st.Snapshots, st.WorkerSnapshots...) {
		if err := p.API.DeleteSnapshot(ctx, snap.ID); err != nil {
			return fmt.Errorf("deleting the cluster snapshot %d: %w", snap.ID, err)
		}
	}
	for _, fw := range st.Firewalls {
		if err := p.API.DeleteFirewall(ctx, fw.ID); err != nil {
			return fmt.Errorf("deleting the cluster firewall %d: %w", fw.ID, err)
		}
	}
	for _, n := range st.Networks {
		if err := p.API.DeleteNetwork(ctx, n.ID); err != nil {
			return fmt.Errorf("deleting the cluster network %d: %w", n.ID, err)
		}
	}
	return nil
}
