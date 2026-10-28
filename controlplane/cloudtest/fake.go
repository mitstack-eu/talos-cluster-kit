// Package cloudtest holds a Hetzner Cloud project in memory behind controlplane.API, for the kit's tests and a block's.
package cloudtest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

// UserDataLimit is the most user data Hetzner's create-server reference accepts: "limited to 32KiB".
const UserDataLimit = 32 * 1024

// Fake holds every resource in memory and records each create, as a project would, answering each call as Hetzner did
// live. Its fields are for a test to read and set between calls.
type Fake struct {
	mu            sync.Mutex
	nextID        int64
	Servers       map[int64]*controlplane.Server
	UserData      map[int64]string
	Networks      map[int64]*controlplane.Network
	Firewalls     map[int64]*controlplane.Firewall
	LoadBalancers map[int64]*controlplane.LoadBalancer
	Snapshots     map[int64]*controlplane.Snapshot
	ServerSpecs   []controlplane.ServerSpec
	LBSpecs       []controlplane.LoadBalancerSpec
	NetworkSpecs  []controlplane.NetworkSpec
	// NodesPerCreate holds how many nodes each server create made.
	NodesPerCreate []int
	now            func() time.Time
	// started and imaging hold when each server was powered on and each snapshot was started.
	started, imaging map[int64]time.Time
	// waits counts the calls the adapter waits on a Hetzner action for, and writes every call that changes something.
	waits, writes int
	// InstantBuild powers each builder off as it is powered on, and images each snapshot as it starts.
	InstantBuild bool
	// FailNext, when set, fails the next server list with it.
	FailNext error
	// FailDelete, when set, fails the next server delete with it.
	FailDelete error
	// FailCreate, when set, fails the next server create of that role with it.
	FailCreate map[controlplane.Role]error
	// FailLBStep, when set, fails the next load balancer call of that name with it.
	FailLBStep map[string]error
	// FailAttach, when set, fails the next attach of a server of that role with it once the attach has landed.
	FailAttach map[controlplane.Role]error
	// SoldOut holds the server types no location can order.
	SoldOut []string
	// Metrics answers each server's metrics read; FailMetrics, when set, fails every read, and MetricsReads records each one.
	Metrics      map[int64]controlplane.Metrics
	FailMetrics  error
	MetricsReads []MetricsRead
}

type MetricsRead struct {
	ID         int64
	Start, End time.Time
}

func New(now func() time.Time) *Fake {
	return &Fake{
		nextID: 1000, now: now,
		Servers: map[int64]*controlplane.Server{}, UserData: map[int64]string{}, Networks: map[int64]*controlplane.Network{},
		Firewalls: map[int64]*controlplane.Firewall{}, LoadBalancers: map[int64]*controlplane.LoadBalancer{},
		Snapshots: map[int64]*controlplane.Snapshot{}, started: map[int64]time.Time{}, imaging: map[int64]time.Time{},
		FailCreate: map[controlplane.Role]error{}, FailLBStep: map[string]error{}, FailAttach: map[controlplane.Role]error{},
		Metrics: map[int64]controlplane.Metrics{},
	}
}

// NextID is the id the project gives its next resource.
func (f *Fake) NextID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id()
}

func (f *Fake) id() int64 {
	f.nextID++
	return f.nextID
}

// write counts a call that changes the project, and one the adapter waits on.
func (f *Fake) write(waited bool) {
	f.writes++
	if waited {
		f.waits++
	}
}

// Writes counts every call that changed the project.
func (f *Fake) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// Waits counts the calls whose Hetzner action the adapter would wait on.
func (f *Fake) Waits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits
}

// held reports whether a server or load balancer holds ip on the network.
func (f *Fake) held(networkID int64, ip string) bool {
	for _, s := range f.Servers {
		if s.PrivateIPs[networkID] == ip {
			return true
		}
	}
	for _, lb := range f.LoadBalancers {
		if lb.PrivateIPs[networkID] == ip {
			return true
		}
	}
	return false
}

// firstFree is the lowest address no one holds above the router's, which Hetzner gives a server joining with no address.
func (f *Fake) firstFree(networkID int64) string {
	for host := 2; ; host++ {
		if ip := fmt.Sprintf("10.0.0.%d", host); !f.held(networkID, ip) {
			return ip
		}
	}
}

// matches reads a selector of comma-separated key=value terms, the only form the plane sends.
func matches(labels map[string]string, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		key, value, _ := strings.Cut(term, "=")
		if labels[key] != value {
			return false
		}
	}
	return true
}

func selected[T any](items map[int64]*T, labels func(*T) map[string]string, selector string) []T {
	var out []T
	for _, id := range slices.Sorted(maps.Keys(items)) {
		if matches(labels(items[id]), selector) {
			out = append(out, *items[id])
		}
	}
	return out
}

func role(labels map[string]string) controlplane.Role {
	return controlplane.Role(labels[controlplane.KeyRole])
}

// builds reports whether a server builds an image, powering itself off once it is done.
func builds(labels map[string]string) bool {
	return role(labels) == controlplane.RoleBuilder || role(labels) == controlplane.RoleWorkerBuilder
}

func (f *Fake) ListServers(_ context.Context, sel string) ([]controlplane.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.FailNext; err != nil {
		f.FailNext = nil
		return nil, err
	}
	return selected(f.Servers, func(s *controlplane.Server) map[string]string { return s.Labels }, sel), nil
}

func (f *Fake) CreateServers(_ context.Context, specs ...controlplane.ServerSpec) ([]controlplane.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(true)
	nodes := 0
	for _, spec := range specs {
		if role(spec.Labels) == controlplane.RoleNode {
			nodes++
		}
	}
	f.NodesPerCreate = append(f.NodesPerCreate, nodes)
	var created []controlplane.Server
	for _, spec := range specs {
		if err := f.FailCreate[role(spec.Labels)]; err != nil {
			delete(f.FailCreate, role(spec.Labels))
			return created, err
		}
		if len(spec.UserData) > UserDataLimit {
			return created, fmt.Errorf("creating server %s: invalid input in field 'user_data' (invalid_input)", spec.Name)
		}
		s := &controlplane.Server{
			ID: f.id(), Name: spec.Name, Location: spec.Location, ServerType: spec.ServerType, Status: controlplane.ServerRunning,
			Labels: maps.Clone(spec.Labels), PrivateIPs: map[int64]string{}, FirewallIDs: slices.Clone(spec.FirewallIDs),
			ImageID: spec.ImageID, Created: f.now(),
		}
		if spec.Stopped {
			s.Status = controlplane.ServerOff
		} else {
			f.started[s.ID] = f.now()
		}
		if spec.PublicIPv4 {
			s.PublicIPv4 = fmt.Sprintf("203.0.113.%d", s.ID%250)
		}
		for _, n := range spec.NetworkIDs {
			s.PrivateIPs[n] = f.firstFree(n)
		}
		f.Servers[s.ID], f.UserData[s.ID] = s, spec.UserData
		f.ServerSpecs = append(f.ServerSpecs, spec)
		created = append(created, *s)
	}
	return created, nil
}

func (f *Fake) AvailableServerTypes(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc([]string{"cx23", "cx33", "cx43", "cx53", "cpx22", "cpx52", "cpx62"},
		func(name string) bool { return slices.Contains(f.SoldOut, name) }), nil
}

func (f *Fake) ServerStarted(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.started[id]
	return ok, nil
}

// AttachServerToNetwork answers as Hetzner did live, refusing a server already on the network and an address held.
func (f *Fake) AttachServerToNetwork(_ context.Context, serverID, networkID int64, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.Servers[serverID]
	switch {
	case s.PrivateIPs[networkID] != "":
		return fmt.Errorf("attaching server %d to network %d: server already attached (server_already_attached)", serverID, networkID)
	case ip == "":
		ip = f.firstFree(networkID)
	case f.held(networkID, ip):
		return fmt.Errorf("attaching server %d to network %d at %s: IP not available (ip_not_available)", serverID, networkID, ip)
	}
	f.write(true)
	s.PrivateIPs[networkID] = ip
	if err := f.FailAttach[role(s.Labels)]; err != nil {
		delete(f.FailAttach, role(s.Labels))
		return err
	}
	return nil
}

func (f *Fake) PowerOnServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(true)
	s := f.Servers[id]
	s.Status, f.started[id] = controlplane.ServerRunning, f.now()
	if f.InstantBuild && builds(s.Labels) {
		s.Status = controlplane.ServerOff
	}
	return nil
}

func (f *Fake) ServerMetrics(_ context.Context, id int64, start, end time.Time) (controlplane.Metrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MetricsReads = append(f.MetricsReads, MetricsRead{ID: id, Start: start, End: end})
	if f.FailMetrics != nil {
		return controlplane.Metrics{}, f.FailMetrics
	}
	return f.Metrics[id], nil
}

func (f *Fake) DeleteServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.FailDelete; err != nil {
		f.FailDelete = nil
		return err
	}
	f.write(true)
	delete(f.Servers, id)
	return nil
}

func (f *Fake) ListNetworks(_ context.Context, sel string) ([]controlplane.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.Networks, func(n *controlplane.Network) map[string]string { return n.Labels }, sel), nil
}

func (f *Fake) CreateNetwork(_ context.Context, spec controlplane.NetworkSpec) (controlplane.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(false)
	n := &controlplane.Network{
		ID: f.id(), Name: spec.Name, IPRange: spec.IPRange, Labels: maps.Clone(spec.Labels), Routes: spec.Routes,
		VSwitchSubnets: map[int64]string{},
	}
	f.Networks[n.ID] = n
	f.NetworkSpecs = append(f.NetworkSpecs, spec)
	return *n, nil
}

func (f *Fake) SetNetworkLabels(_ context.Context, id int64, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.Networks[id]
	if !ok {
		return fmt.Errorf("labelling network %d: not_found", id)
	}
	f.write(false)
	n.Labels = maps.Clone(labels)
	return nil
}

// AddVSwitchSubnet takes a vSwitch already coupled at ipRange as coupled, and refuses another range for it or a range
// another vSwitch holds, as Hetzner does.
func (f *Fake) AddVSwitchSubnet(_ context.Context, networkID, vswitchID int64, ipRange string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.Networks[networkID]
	if !ok {
		return fmt.Errorf("coupling vSwitch %d to network %d: not_found", vswitchID, networkID)
	}
	if n.VSwitchSubnets[vswitchID] == ipRange {
		if !n.ExposeRoutesToVSwitch {
			f.write(false)
			n.ExposeRoutesToVSwitch = true
		}
		return nil
	}
	if _, ok := n.VSwitchSubnets[vswitchID]; ok || slices.Contains(slices.Collect(maps.Values(n.VSwitchSubnets)), ipRange) {
		return fmt.Errorf("coupling vSwitch %d to network %d at %s: vswitch_id_already_used or ip_range overlaps (invalid_input)", vswitchID, networkID, ipRange)
	}
	f.write(true)
	n.VSwitchSubnets[vswitchID], n.ExposeRoutesToVSwitch = ipRange, true
	return nil
}

func (f *Fake) DeleteNetwork(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Networks, id)
	return nil
}

func (f *Fake) ListFirewalls(_ context.Context, sel string) ([]controlplane.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.Firewalls, func(fw *controlplane.Firewall) map[string]string { return fw.Labels }, sel), nil
}

func (f *Fake) CreateFirewall(_ context.Context, spec controlplane.FirewallSpec) (controlplane.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(true)
	fw := &controlplane.Firewall{ID: f.id(), Name: spec.Name, Labels: maps.Clone(spec.Labels)}
	f.Firewalls[fw.ID] = fw
	return *fw, nil
}

func (f *Fake) DeleteFirewall(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Firewalls, id)
	return nil
}

func (f *Fake) ListLoadBalancers(_ context.Context, sel string) ([]controlplane.LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.LoadBalancers, func(lb *controlplane.LoadBalancer) map[string]string { return lb.Labels }, sel), nil
}

func (f *Fake) CreateLoadBalancer(_ context.Context, spec controlplane.LoadBalancerSpec) (controlplane.LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(true)
	lb := &controlplane.LoadBalancer{
		ID: f.id(), Name: spec.Name, Location: spec.Location, Labels: maps.Clone(spec.Labels), PublicInterface: spec.PublicInterface,
		PrivateIPs: map[int64]string{}, Services: slices.Clone(spec.Services),
	}
	if spec.NetworkID != 0 {
		lb.PrivateIPs[spec.NetworkID] = f.firstFree(spec.NetworkID)
	}
	if spec.TargetSelector != "" {
		lb.TargetSelectors = []string{spec.TargetSelector}
	}
	if spec.PublicInterface {
		lb.PublicIPv4 = fmt.Sprintf("198.51.100.%d", lb.ID%250)
	}
	f.LoadBalancers[lb.ID] = lb
	f.LBSpecs = append(f.LBSpecs, spec)
	return *lb, nil
}

// lbStep fails a load balancer call FailLBStep names, once, and otherwise returns the load balancer it acts on.
func (f *Fake) lbStep(step string, id int64) (*controlplane.LoadBalancer, error) {
	if err := f.FailLBStep[step]; err != nil {
		delete(f.FailLBStep, step)
		return nil, err
	}
	lb, ok := f.LoadBalancers[id]
	if !ok {
		return nil, fmt.Errorf("not_found: load balancer %d", id)
	}
	f.write(true)
	return lb, nil
}

// AttachLoadBalancerToNetwork assigns the requested address once the attach action finishes, and refuses a second attach as Hetzner does.
func (f *Fake) AttachLoadBalancerToNetwork(_ context.Context, id, networkID int64, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("attach", id)
	if err != nil {
		return err
	}
	if len(lb.PrivateIPs) > 0 {
		return fmt.Errorf("load_balancer_already_attached: load balancer %d", id)
	}
	if f.held(networkID, ip) {
		return fmt.Errorf("attaching load balancer %d to network %d at %s: IP not available (ip_not_available)", id, networkID, ip)
	}
	lb.PrivateIPs[networkID] = ip
	return nil
}

func (f *Fake) DisableLoadBalancerPublicInterface(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("disable-public", id)
	if err != nil {
		return err
	}
	if len(lb.PrivateIPs) == 0 {
		return fmt.Errorf("load_balancer_not_attached_to_network: load balancer %d", id)
	}
	lb.PublicInterface = false
	return nil
}

func (f *Fake) AddLoadBalancerService(_ context.Context, id int64, service controlplane.Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("add-service", id)
	if err != nil {
		return err
	}
	for _, s := range lb.Services {
		if s.ListenPort == service.ListenPort {
			return fmt.Errorf("source_port_already_used: load balancer %d listens on %d", id, s.ListenPort)
		}
	}
	lb.Services = append(lb.Services, service)
	return nil
}

func (f *Fake) AddLoadBalancerTarget(_ context.Context, id int64, labelSelector string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("add-target", id)
	if err != nil {
		return err
	}
	if len(lb.PrivateIPs) == 0 {
		return fmt.Errorf("load_balancer_not_attached_to_network: load balancer %d", id)
	}
	if slices.Contains(lb.TargetSelectors, labelSelector) {
		return fmt.Errorf("target_already_defined: load balancer %d", id)
	}
	lb.TargetSelectors = append(lb.TargetSelectors, labelSelector)
	return nil
}

func (f *Fake) DeleteLoadBalancer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.LoadBalancers, id)
	return nil
}

func (f *Fake) ListSnapshots(_ context.Context, sel string) ([]controlplane.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.Snapshots, func(s *controlplane.Snapshot) map[string]string { return s.Labels }, sel), nil
}

func (f *Fake) StartSnapshot(_ context.Context, serverID int64, spec controlplane.SnapshotSpec) (controlplane.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Servers[serverID] == nil || f.Servers[serverID].Status != controlplane.ServerOff {
		return controlplane.Snapshot{}, fmt.Errorf("server %d is not powered off", serverID)
	}
	f.write(false)
	snap := &controlplane.Snapshot{ID: f.id(), Description: spec.Description, Status: "creating", Labels: maps.Clone(spec.Labels)}
	if f.InstantBuild {
		snap.Status = controlplane.SnapshotAvailable
	}
	f.Snapshots[snap.ID], f.imaging[snap.ID] = snap, f.now()
	return *snap, nil
}

func (f *Fake) DeleteSnapshot(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(false)
	delete(f.Snapshots, id)
	return nil
}

// Advance plays what Hetzner and the builders finish on their own: a builder powers itself off build after its
// power-on, unless build is zero, and a snapshot is available image after it started.
func (f *Fake) Advance(build, image time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, s := range f.Servers {
		if builds(s.Labels) && s.Status == controlplane.ServerRunning && build > 0 && !f.now().Before(f.started[id].Add(build)) {
			s.Status = controlplane.ServerOff
		}
	}
	for id, snap := range f.Snapshots {
		if !f.now().Before(f.imaging[id].Add(image)) {
			snap.Status = controlplane.SnapshotAvailable
		}
	}
}

// Resources is every resource but the builders and the serving server, as text, so a later read shows any change to
// one made before.
func (f *Fake) Resources() map[int64]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]string{}
	for id, s := range f.Servers {
		if !builds(s.Labels) && role(s.Labels) != controlplane.RoleServing {
			out[id] = fmt.Sprintf("%+v", *s)
		}
	}
	for id, n := range f.Networks {
		out[id] = fmt.Sprintf("%+v", *n)
	}
	for id, fw := range f.Firewalls {
		out[id] = fmt.Sprintf("%+v", *fw)
	}
	for id, lb := range f.LoadBalancers {
		out[id] = fmt.Sprintf("%+v", *lb)
	}
	return out
}

// Selected returns the servers selector matches.
func (f *Fake) Selected(t testing.TB, selector string) []controlplane.Server {
	t.Helper()
	servers, err := f.ListServers(context.Background(), selector)
	require.NoError(t, err)
	return servers
}

// OnlyLoadBalancer returns the one load balancer selector matches, failing unless exactly one does.
func (f *Fake) OnlyLoadBalancer(t testing.TB, selector string) controlplane.LoadBalancer {
	t.Helper()
	lbs, err := f.ListLoadBalancers(context.Background(), selector)
	require.NoError(t, err)
	require.Len(t, lbs, 1, "load balancers matching %s", selector)
	return lbs[0]
}

// OnlyNetwork returns the one network selector matches, failing unless exactly one does.
func (f *Fake) OnlyNetwork(t testing.TB, selector string) controlplane.Network {
	t.Helper()
	networks, err := f.ListNetworks(context.Background(), selector)
	require.NoError(t, err)
	require.Len(t, networks, 1, "networks matching %s", selector)
	return networks[0]
}

// SpecFor is the first server spec created with role r.
func (f *Fake) SpecFor(t testing.TB, r controlplane.Role) controlplane.ServerSpec {
	t.Helper()
	for _, spec := range f.ServerSpecs {
		if role(spec.Labels) == r {
			return spec
		}
	}
	require.Failf(t, "no server created", "no server with role %s was created", r)
	return controlplane.ServerSpec{}
}

// PowerOff stands in for a server powering itself off, as the builder script's last step does.
func (f *Fake) PowerOff(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Servers[id].Status = controlplane.ServerOff
}

// GiveMetrics gives the server the series Hetzner would answer, a point a minute from a minute after its creation.
func (f *Fake) GiveMetrics(s controlplane.Server, series map[string][]float64) {
	m := controlplane.Metrics{Step: time.Minute, Series: map[string][]controlplane.MetricPoint{}}
	for name, values := range series {
		for i, v := range values {
			m.Series[name] = append(m.Series[name], controlplane.MetricPoint{At: s.Created.Add(time.Duration(i+1) * time.Minute), Value: v})
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Metrics[s.ID] = m
}

// Count is every resource the project holds.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Servers) + len(f.Networks) + len(f.Firewalls) + len(f.LoadBalancers) + len(f.Snapshots)
}
