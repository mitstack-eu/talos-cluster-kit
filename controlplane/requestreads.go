package controlplane

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// RequestReads holds what each plane read within one request, so the request lists each kind of resource once; its
// zero value is ready to use.
type RequestReads struct {
	mu   sync.Mutex
	kept map[string]State
	// drops counts each start and end of a write, so a list that a write overlapped is not kept either.
	drops uint64
	// writing counts the writes still running, during which nothing is kept, since a list may miss what they change.
	writing int
}

func NewRequestReads() *RequestReads {
	return &RequestReads{kept: map[string]State{}}
}

func (r *RequestReads) read(ctx context.Context, p Plane) (State, error) {
	r.mu.Lock()
	kept, ok := r.kept[p.Selector()]
	drops := r.drops
	r.mu.Unlock()
	if ok {
		return kept.clone(), nil
	}
	st, err := p.list(ctx)
	if err != nil {
		return st, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drops == drops && r.writing == 0 {
		if r.kept == nil {
			r.kept = map[string]State{}
		}
		r.kept[p.Selector()] = st.clone()
	}
	return st, nil
}

func (r *RequestReads) startWrite() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writing++
	r.drop()
}

func (r *RequestReads) endWrite() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writing--
	r.drop()
}

// drop needs r.mu held.
func (r *RequestReads) drop() {
	clear(r.kept)
	r.drops++
}

type hiddenReadsKey struct{}

// WithoutRequestReads is ctx with its request's reads hidden from every plane read, for work that outlives the request
// it began in, though a write made in it still has the request's next read list again.
func WithoutRequestReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, hiddenReadsKey{}, true)
}

// ReadOncePerRequest wraps api so Plane.Read answers from the RequestReads readsOf finds in a call's context, and every
// write through it, refused or not, has the request's reads list afresh while it runs and after; a context with none
// reads as api does.
func ReadOncePerRequest(api API, readsOf func(context.Context) *RequestReads) API {
	return &readOncePerRequest{api: api, readsOf: readsOf}
}

// readOncePerRequest names every API method and embeds none, so a method added to API fails to compile until the
// wrapper names it, deferring a call to what writing returns when it writes.
type readOncePerRequest struct {
	api     API
	readsOf func(context.Context) *RequestReads
}

func (w *readOncePerRequest) read(ctx context.Context, p Plane) (State, error) {
	if hidden, _ := ctx.Value(hiddenReadsKey{}).(bool); hidden {
		return p.list(ctx)
	}
	reads := w.readsOf(ctx)
	if reads == nil {
		return p.list(ctx)
	}
	return reads.read(ctx, p)
}

// writing marks a write as running in the request's reads and returns what marks it ended.
func (w *readOncePerRequest) writing(ctx context.Context) func() {
	reads := w.readsOf(ctx)
	if reads == nil {
		return func() {}
	}
	reads.startWrite()
	return reads.endWrite
}

func (w *readOncePerRequest) ListServers(ctx context.Context, labelSelector string) ([]Server, error) {
	return w.api.ListServers(ctx, labelSelector)
}

func (w *readOncePerRequest) CreateServers(ctx context.Context, specs ...ServerSpec) ([]Server, error) {
	defer w.writing(ctx)()
	return w.api.CreateServers(ctx, specs...)
}

func (w *readOncePerRequest) AvailableServerTypes(ctx context.Context, location string) ([]string, error) {
	return w.api.AvailableServerTypes(ctx, location)
}

func (w *readOncePerRequest) ServerStarted(ctx context.Context, id int64) (bool, error) {
	return w.api.ServerStarted(ctx, id)
}

func (w *readOncePerRequest) AttachServerToNetwork(ctx context.Context, serverID, networkID int64, ip string) error {
	defer w.writing(ctx)()
	return w.api.AttachServerToNetwork(ctx, serverID, networkID, ip)
}

func (w *readOncePerRequest) PowerOnServer(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.PowerOnServer(ctx, id)
}

func (w *readOncePerRequest) PowerOffServer(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.PowerOffServer(ctx, id)
}

func (w *readOncePerRequest) DeleteServer(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.DeleteServer(ctx, id)
}

func (w *readOncePerRequest) ListNetworks(ctx context.Context, labelSelector string) ([]Network, error) {
	return w.api.ListNetworks(ctx, labelSelector)
}

func (w *readOncePerRequest) CreateNetwork(ctx context.Context, spec NetworkSpec) (Network, error) {
	defer w.writing(ctx)()
	return w.api.CreateNetwork(ctx, spec)
}

func (w *readOncePerRequest) SetNetworkLabels(ctx context.Context, id int64, labels map[string]string) error {
	defer w.writing(ctx)()
	return w.api.SetNetworkLabels(ctx, id, labels)
}

func (w *readOncePerRequest) AddRoute(ctx context.Context, networkID int64, r Route) error {
	defer w.writing(ctx)()
	return w.api.AddRoute(ctx, networkID, r)
}

func (w *readOncePerRequest) DeleteRoute(ctx context.Context, networkID int64, r Route) error {
	defer w.writing(ctx)()
	return w.api.DeleteRoute(ctx, networkID, r)
}

func (w *readOncePerRequest) SetServerLabels(ctx context.Context, id int64, labels map[string]string) error {
	defer w.writing(ctx)()
	return w.api.SetServerLabels(ctx, id, labels)
}

func (w *readOncePerRequest) LastPowerAction(ctx context.Context, id int64) (string, time.Time, error) {
	return w.api.LastPowerAction(ctx, id)
}

func (w *readOncePerRequest) AddVSwitchSubnet(ctx context.Context, networkID, vswitchID int64, ipRange string) error {
	defer w.writing(ctx)()
	return w.api.AddVSwitchSubnet(ctx, networkID, vswitchID, ipRange)
}

func (w *readOncePerRequest) DeleteNetwork(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.DeleteNetwork(ctx, id)
}

func (w *readOncePerRequest) ListFirewalls(ctx context.Context, labelSelector string) ([]Firewall, error) {
	return w.api.ListFirewalls(ctx, labelSelector)
}

func (w *readOncePerRequest) CreateFirewall(ctx context.Context, spec FirewallSpec) (Firewall, error) {
	defer w.writing(ctx)()
	return w.api.CreateFirewall(ctx, spec)
}

func (w *readOncePerRequest) DeleteFirewall(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.DeleteFirewall(ctx, id)
}

func (w *readOncePerRequest) ListLoadBalancers(ctx context.Context, labelSelector string) ([]LoadBalancer, error) {
	return w.api.ListLoadBalancers(ctx, labelSelector)
}

func (w *readOncePerRequest) CreateLoadBalancer(ctx context.Context, spec LoadBalancerSpec) (LoadBalancer, error) {
	defer w.writing(ctx)()
	return w.api.CreateLoadBalancer(ctx, spec)
}

func (w *readOncePerRequest) AttachLoadBalancerToNetwork(ctx context.Context, id, networkID int64, ip string) error {
	defer w.writing(ctx)()
	return w.api.AttachLoadBalancerToNetwork(ctx, id, networkID, ip)
}

func (w *readOncePerRequest) DisableLoadBalancerPublicInterface(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.DisableLoadBalancerPublicInterface(ctx, id)
}

func (w *readOncePerRequest) AddLoadBalancerService(ctx context.Context, id int64, service Service) error {
	defer w.writing(ctx)()
	return w.api.AddLoadBalancerService(ctx, id, service)
}

func (w *readOncePerRequest) AddLoadBalancerTarget(ctx context.Context, id int64, labelSelector string) error {
	defer w.writing(ctx)()
	return w.api.AddLoadBalancerTarget(ctx, id, labelSelector)
}

func (w *readOncePerRequest) AddLoadBalancerIPTarget(ctx context.Context, id int64, ip string) error {
	defer w.writing(ctx)()
	return w.api.AddLoadBalancerIPTarget(ctx, id, ip)
}

func (w *readOncePerRequest) RemoveLoadBalancerTarget(ctx context.Context, id int64, labelSelector string) error {
	defer w.writing(ctx)()
	return w.api.RemoveLoadBalancerTarget(ctx, id, labelSelector)
}

func (w *readOncePerRequest) DeleteLoadBalancer(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.DeleteLoadBalancer(ctx, id)
}

func (w *readOncePerRequest) ListSnapshots(ctx context.Context, labelSelector string) ([]Snapshot, error) {
	return w.api.ListSnapshots(ctx, labelSelector)
}

func (w *readOncePerRequest) StartSnapshot(ctx context.Context, serverID int64, spec SnapshotSpec) (Snapshot, error) {
	defer w.writing(ctx)()
	return w.api.StartSnapshot(ctx, serverID, spec)
}

func (w *readOncePerRequest) DeleteSnapshot(ctx context.Context, id int64) error {
	defer w.writing(ctx)()
	return w.api.DeleteSnapshot(ctx, id)
}

func (w *readOncePerRequest) ServerMetrics(ctx context.Context, id int64, start, end time.Time) (Metrics, error) {
	return w.api.ServerMetrics(ctx, id, start, end)
}

// clone copies every slice and map the state holds, so a caller that changes its state changes no other read's.
func (st State) clone() State {
	st.Networks = cloneEach(st.Networks, Network.clone)
	st.Firewalls = cloneEach(st.Firewalls, Firewall.clone)
	st.Servers = cloneByRole(st.Servers, Server.clone)
	st.LoadBalancers = cloneByRole(st.LoadBalancers, LoadBalancer.clone)
	st.Snapshots = cloneEach(st.Snapshots, Snapshot.clone)
	st.WorkerSnapshots = cloneEach(st.WorkerSnapshots, Snapshot.clone)
	return st
}

func cloneEach[T any](items []T, clone func(T) T) []T {
	if items == nil {
		return nil
	}
	out := make([]T, len(items))
	for i, item := range items {
		out[i] = clone(item)
	}
	return out
}

func cloneByRole[T any](byRole map[Role][]T, clone func(T) T) map[Role][]T {
	out := make(map[Role][]T, len(byRole))
	for r, items := range byRole {
		out[r] = cloneEach(items, clone)
	}
	return out
}

func (s Server) clone() Server {
	s.Labels, s.PrivateIPs, s.FirewallIDs = maps.Clone(s.Labels), maps.Clone(s.PrivateIPs), slices.Clone(s.FirewallIDs)
	return s
}

func (n Network) clone() Network {
	n.Labels, n.Routes, n.VSwitchSubnets = maps.Clone(n.Labels), slices.Clone(n.Routes), maps.Clone(n.VSwitchSubnets)
	return n
}

func (f Firewall) clone() Firewall {
	f.Labels = maps.Clone(f.Labels)
	return f
}

func (lb LoadBalancer) clone() LoadBalancer {
	lb.Labels, lb.PrivateIPs = maps.Clone(lb.Labels), maps.Clone(lb.PrivateIPs)
	lb.Services, lb.TargetSelectors, lb.TargetIPs = slices.Clone(lb.Services), slices.Clone(lb.TargetSelectors), slices.Clone(lb.TargetIPs)
	return lb
}

func (s Snapshot) clone() Snapshot {
	s.Labels = maps.Clone(s.Labels)
	return s
}
