package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// State is every Hetzner resource that carries a subscription's labels, sorted by role.
type State struct {
	Networks      []Network
	Firewalls     []Firewall
	Servers       map[Role][]Server
	LoadBalancers map[Role][]LoadBalancer
	// Snapshots are the cluster's own images, and WorkerSnapshots the worker images.
	Snapshots       []Snapshot
	WorkerSnapshots []Snapshot
}

// Read lists every resource of the plane's subscription.
func (p Plane) Read(ctx context.Context) (State, error) {
	sel := p.Selector()
	st := State{Servers: map[Role][]Server{}, LoadBalancers: map[Role][]LoadBalancer{}}
	servers, err := p.API.ListServers(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("listing the subscription's servers: %w", err)
	}
	for _, s := range servers {
		st.Servers[Role(s.Labels[KeyRole])] = append(st.Servers[Role(s.Labels[KeyRole])], s)
	}
	lbs, err := p.API.ListLoadBalancers(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("listing the subscription's load balancers: %w", err)
	}
	for _, lb := range lbs {
		st.LoadBalancers[Role(lb.Labels[KeyRole])] = append(st.LoadBalancers[Role(lb.Labels[KeyRole])], lb)
	}
	if st.Networks, err = p.API.ListNetworks(ctx, sel); err != nil {
		return st, fmt.Errorf("listing the subscription's networks: %w", err)
	}
	if st.Firewalls, err = p.API.ListFirewalls(ctx, sel); err != nil {
		return st, fmt.Errorf("listing the subscription's firewalls: %w", err)
	}
	snapshots, err := p.API.ListSnapshots(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("listing the subscription's snapshots: %w", err)
	}
	for _, snap := range snapshots {
		if Role(snap.Labels[KeyRole]) == RoleWorkerImage {
			st.WorkerSnapshots = append(st.WorkerSnapshots, snap)
		} else {
			st.Snapshots = append(st.Snapshots, snap)
		}
	}
	return st, nil
}

func (st State) Network() (Network, bool) { return onlyOne(st.Networks) }

func (st State) Firewall() (Firewall, bool) { return onlyOne(st.Firewalls) }

func (st State) Server(r Role) (Server, bool) { return onlyOne(st.Servers[r]) }

func (st State) Snapshot() (Snapshot, bool) { return onlyOne(st.Snapshots) }

func (st State) WorkerSnapshot() (Snapshot, bool) { return onlyOne(st.WorkerSnapshots) }

func (st State) NodeIn(loc string) (Server, bool) {
	return onlyOne(ServersIn(st.Servers[RoleNode], loc))
}

func (st State) LoadBalancerIn(r Role, loc string) (LoadBalancer, bool) {
	return onlyOne(LoadBalancersIn(st.LoadBalancers[r], loc))
}

// ServersIn reads a location from Hetzner's own record of the server, never from its name or a label.
func ServersIn(servers []Server, loc string) []Server {
	return slices.DeleteFunc(slices.Clone(servers), func(s Server) bool { return s.Location != loc })
}

func LoadBalancersIn(lbs []LoadBalancer, loc string) []LoadBalancer {
	return slices.DeleteFunc(slices.Clone(lbs), func(lb LoadBalancer) bool { return lb.Location != loc })
}

func onlyOne[T any](items []T) (T, bool) {
	if len(items) != 1 {
		var none T
		return none, false
	}
	return items[0], true
}

// HasServers reports whether any server carries the subscription's labels.
func (st State) HasServers() bool {
	for _, servers := range st.Servers {
		if len(servers) > 0 {
			return true
		}
	}
	return false
}

func (st State) Empty() bool {
	count := len(st.Networks) + len(st.Firewalls) + len(st.Snapshots) + len(st.WorkerSnapshots)
	for _, s := range st.Servers {
		count += len(s)
	}
	for _, lb := range st.LoadBalancers {
		count += len(lb)
	}
	return count == 0
}

// Duplicates names each role more than one resource carries; the plane touches nothing while any does.
func (st State) Duplicates() []string {
	var out []string
	add := func(what string, ids []int64) {
		if len(ids) > 1 {
			out = append(out, DuplicatesDetail(what, ids))
		}
	}
	add("network", IDs(st.Networks, func(n Network) int64 { return n.ID }))
	add("firewall", IDs(st.Firewalls, func(f Firewall) int64 { return f.ID }))
	add("snapshot", IDs(st.Snapshots, func(s Snapshot) int64 { return s.ID }))
	add("worker image snapshot", IDs(st.WorkerSnapshots, func(s Snapshot) int64 { return s.ID }))
	for _, r := range []Role{RoleGateway, RoleBuilder, RoleServing, RoleWorkerBuilder} {
		add(string(r)+" server", IDs(st.Servers[r], ServerID))
	}
	for _, loc := range Locations {
		add(loc+" node server", IDs(ServersIn(st.Servers[RoleNode], loc), ServerID))
	}
	for _, r := range []Role{RoleAPI, RoleIngress} {
		add(string(r)+" load balancer", IDs(st.LoadBalancers[r], LoadBalancerID))
	}
	return out
}

// DuplicatesDetail says how many resources of one kind carry the subscription's labels, and which.
func DuplicatesDetail(what string, ids []int64) string {
	named := make([]string, len(ids))
	for i, id := range ids {
		named[i] = strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("%d %ss carry this subscription's labels: %s", len(ids), what, strings.Join(named, ", "))
}

func ServerID(s Server) int64 { return s.ID }

func LoadBalancerID(lb LoadBalancer) int64 { return lb.ID }

func IDs[T any](items []T, id func(T) int64) []int64 {
	out := make([]int64, len(items))
	for i, item := range items {
		out[i] = id(item)
	}
	return out
}
