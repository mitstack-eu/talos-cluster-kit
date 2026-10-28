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
	missing = append(missing, st.gatewayProblems(network, firewall)...)
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

func (st State) gatewayProblems(network Network, firewall Firewall) []string {
	gw, ok := st.Server(RoleGateway)
	if !ok {
		return []string{"no gateway carries this subscription's labels"}
	}
	var missing []string
	if gw.Status != ServerRunning {
		missing = append(missing, fmt.Sprintf("gateway %d is %s", gw.ID, gw.Status))
	}
	if gw.PrivateIPs[network.ID] != GatewayAddress {
		missing = append(missing, fmt.Sprintf("gateway %d is not on network %d at %s", gw.ID, network.ID, GatewayAddress))
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
	return missing
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
	for _, lb := range []struct {
		role   Role
		public bool
	}{{RoleAPI, false}, {RoleIngress, true}} {
		found, ok := st.LoadBalancerIn(lb.role, GatewayLocation)
		if !ok {
			missing = append(missing, "no "+string(lb.role)+" load balancer in "+GatewayLocation+" carries this subscription's labels")
			continue
		}
		missing = append(missing, p.reachProblems(found, network, lb.public)...)
	}
	return missing
}

func (p Plane) reachProblems(lb LoadBalancer, network Network, public bool) []string {
	var missing []string
	if lb.PrivateIPs[network.ID] == "" {
		missing = append(missing, fmt.Sprintf("load balancer %d is not on network %d", lb.ID, network.ID))
	}
	if lb.PublicInterface != public {
		missing = append(missing, fmt.Sprintf("load balancer %d's public interface is %s, want %s", lb.ID, onOff(lb.PublicInterface), onOff(public)))
	}
	if !slices.Contains(lb.TargetSelectors, p.RoleSelector(RoleNode)) {
		missing = append(missing, fmt.Sprintf("load balancer %d does not target the subscription's nodes by label", lb.ID))
	}
	return missing
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// EnrolmentDigest is the digest a node's enrolment is judged against, which the gateway carries as a label from the
// install on, or a reason it cannot be read; both are empty before the install.
func (st State) EnrolmentDigest() (digest, gap string) {
	gw, ok := st.Server(RoleGateway)
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
	gw, _ := st.Server(RoleGateway)
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
	detail := fmt.Sprintf("nodes %s are on network %d behind gateway %d, and an API and an ingress load balancer in %s reach them",
		strings.Join(nodes, ", "), network.ID, gw.ID, GatewayLocation)
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
