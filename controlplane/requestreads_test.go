package controlplane_test

// Within one request a plane lists each kind of resource once, lists again after any write, and keeps nothing past it.

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
)

// requestReadsKey holds one request's reads in a test's context, as a block's framework holds them for its request.
type requestReadsKey struct{}

func inRequest() context.Context {
	return context.WithValue(context.Background(), requestReadsKey{}, controlplane.NewRequestReads())
}

func requestReadsOf(ctx context.Context) *controlplane.RequestReads {
	reads, _ := ctx.Value(requestReadsKey{}).(*controlplane.RequestReads)
	return reads
}

func readingOncePerRequest(h *harness) {
	h.plane.API = controlplane.ReadOncePerRequest(h.cloud, requestReadsOf)
}

func lists(n int) cloudtest.Reads {
	return cloudtest.Reads{"ListServers": n, "ListLoadBalancers": n, "ListNetworks": n, "ListFirewalls": n, "ListSnapshots": n}
}

type seeded struct{ network, firewall, server, loadBalancer, snapshot int64 }

func (h *harness) seed() seeded {
	s := seeded{h.cloud.NextID(), h.cloud.NextID(), h.cloud.NextID(), h.cloud.NextID(), h.cloud.NextID()}
	h.cloud.Networks[s.network] = &controlplane.Network{
		ID: s.network, Name: "network", IPRange: controlplane.NetworkIPRange, Labels: h.plane.Labels("network"),
		VSwitchSubnets: map[int64]string{},
	}
	h.cloud.Firewalls[s.firewall] = &controlplane.Firewall{ID: s.firewall, Name: "firewall", Labels: h.plane.Labels("firewall")}
	h.cloud.Servers[s.server] = &controlplane.Server{
		ID: s.server, Name: "gateway", Location: "fsn1", Status: controlplane.ServerOff,
		Labels: h.plane.Labels(controlplane.RoleGateway), PrivateIPs: map[int64]string{s.network: "10.0.0.2"},
	}
	h.cloud.LoadBalancers[s.loadBalancer] = &controlplane.LoadBalancer{
		ID: s.loadBalancer, Name: "api", Location: "fsn1", Labels: h.plane.Labels(controlplane.RoleAPI),
		PrivateIPs: map[int64]string{s.network: "10.0.0.3"},
	}
	h.cloud.Snapshots[s.snapshot] = &controlplane.Snapshot{
		ID: s.snapshot, Status: controlplane.SnapshotAvailable, Labels: h.plane.Labels(controlplane.RoleImage),
	}
	return s
}

func (h *harness) readIn(ctx context.Context) controlplane.State {
	h.t.Helper()
	st, err := h.plane.Read(ctx)
	require.NoError(h.t, err)
	return st
}

func (h *harness) cloudWorkerSpec() controlplane.ServerSpec {
	return controlplane.ServerSpec{
		Name: "cloud-worker", ServerType: controlplane.DefaultServerType, Location: "fsn1",
		Labels: h.plane.Labels(controlplane.RoleCloudWorker),
	}
}

// everyWrite is each call that changes the project, made on the seeded resources; Hetzner may refuse any of them.
var everyWrite = []struct {
	name  string
	write func(ctx context.Context, api controlplane.API, s seeded) error
}{
	{"CreateServers", func(ctx context.Context, api controlplane.API, _ seeded) error {
		_, err := api.CreateServers(ctx, controlplane.ServerSpec{Name: "extra", Location: "fsn1"})
		return err
	}},
	{"AttachServerToNetwork", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AttachServerToNetwork(ctx, s.server, s.network, "")
	}},
	{"PowerOnServer", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.PowerOnServer(ctx, s.server)
	}},
	{"PowerOffServer", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.PowerOffServer(ctx, s.server)
	}},
	{"DeleteServer", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DeleteServer(ctx, s.server)
	}},
	{"CreateNetwork", func(ctx context.Context, api controlplane.API, _ seeded) error {
		_, err := api.CreateNetwork(ctx, controlplane.NetworkSpec{Name: "extra", IPRange: controlplane.NetworkIPRange})
		return err
	}},
	{"SetNetworkLabels", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.SetNetworkLabels(ctx, s.network, map[string]string{"extra": "true"})
	}},
	{"AddRoute", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AddRoute(ctx, s.network, controlplane.Route{Destination: "10.9.0.0/16", Gateway: "10.0.0.2"})
	}},
	{"DeleteRoute", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DeleteRoute(ctx, s.network, controlplane.Route{Destination: "10.9.0.0/16", Gateway: "10.0.0.2"})
	}},
	{"SetServerLabels", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.SetServerLabels(ctx, s.server, map[string]string{"extra": "true"})
	}},
	{"AddVSwitchSubnet", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AddVSwitchSubnet(ctx, s.network, 1, "10.0.1.0/24")
	}},
	{"DeleteNetwork", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DeleteNetwork(ctx, s.network)
	}},
	{"CreateFirewall", func(ctx context.Context, api controlplane.API, _ seeded) error {
		_, err := api.CreateFirewall(ctx, controlplane.FirewallSpec{Name: "extra"})
		return err
	}},
	{"DeleteFirewall", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DeleteFirewall(ctx, s.firewall)
	}},
	{"CreateLoadBalancer", func(ctx context.Context, api controlplane.API, s seeded) error {
		_, err := api.CreateLoadBalancer(ctx, controlplane.LoadBalancerSpec{Name: "extra", Location: "fsn1", NetworkID: s.network})
		return err
	}},
	{"AttachLoadBalancerToNetwork", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AttachLoadBalancerToNetwork(ctx, s.loadBalancer, s.network, "10.0.0.4")
	}},
	{"DisableLoadBalancerPublicInterface", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DisableLoadBalancerPublicInterface(ctx, s.loadBalancer)
	}},
	{"AddLoadBalancerService", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AddLoadBalancerService(ctx, s.loadBalancer, controlplane.Service{ListenPort: 6443, DestinationPort: 6443})
	}},
	{"AddLoadBalancerTarget", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AddLoadBalancerTarget(ctx, s.loadBalancer, "extra=true")
	}},
	{"AddLoadBalancerIPTarget", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.AddLoadBalancerIPTarget(ctx, s.loadBalancer, "10.0.1.2")
	}},
	{"RemoveLoadBalancerTarget", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.RemoveLoadBalancerTarget(ctx, s.loadBalancer, "extra=true")
	}},
	{"DeleteLoadBalancer", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DeleteLoadBalancer(ctx, s.loadBalancer)
	}},
	{"StartSnapshot", func(ctx context.Context, api controlplane.API, s seeded) error {
		_, err := api.StartSnapshot(ctx, s.server, controlplane.SnapshotSpec{Description: "extra"})
		return err
	}},
	{"DeleteSnapshot", func(ctx context.Context, api controlplane.API, s seeded) error {
		return api.DeleteSnapshot(ctx, s.snapshot)
	}},
}

func TestPlaneRead_ReadsAgainAfterAWrite(t *testing.T) {
	t.Run("a server a step creates shows in the request's next read", func(t *testing.T) {
		h := newHarness(t, readingOncePerRequest)
		h.seed()
		ctx := inRequest()
		before := h.cloud.Reads()

		h.readIn(ctx)
		created, err := h.plane.API.CreateServers(ctx, h.cloudWorkerSpec())
		require.NoError(t, err)
		st := h.readIn(ctx)

		assert.Equal(t, controlplane.IDs(created, controlplane.ServerID),
			controlplane.IDs(st.Servers[controlplane.RoleCloudWorker], controlplane.ServerID), "the Cloud workers the read after the create lists")
		assert.Equal(t, lists(2), h.cloud.Reads().Since(before), "the reads of two plane reads with a create between them")
	})
	t.Run("a create Hetzner refuses still has the next read list again", func(t *testing.T) {
		h := newHarness(t, readingOncePerRequest)
		h.seed()
		ctx := inRequest()
		h.readIn(ctx)
		h.cloud.FailCreate[controlplane.RoleCloudWorker] = errors.New("server limit reached (resource_limit_exceeded)")
		before := h.cloud.Reads()

		_, err := h.plane.API.CreateServers(ctx, h.cloudWorkerSpec())
		require.Error(t, err)
		h.readIn(ctx)

		assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of a plane read after a refused create")
	})
	t.Run("a write from work outside the request still has the request's next read list again", func(t *testing.T) {
		h := newHarness(t, readingOncePerRequest)
		h.seed()
		ctx := inRequest()
		h.readIn(ctx)

		created, err := h.plane.API.CreateServers(controlplane.WithoutRequestReads(ctx), h.cloudWorkerSpec())
		require.NoError(t, err)
		st := h.readIn(ctx)

		assert.Equal(t, controlplane.IDs(created, controlplane.ServerID),
			controlplane.IDs(st.Servers[controlplane.RoleCloudWorker], controlplane.ServerID), "the Cloud workers the request reads next")
	})
	for _, tc := range everyWrite {
		t.Run(tc.name+" has the request's next read list again", func(t *testing.T) {
			h := newHarness(t, readingOncePerRequest)
			s := h.seed()
			ctx := inRequest()
			h.readIn(ctx)
			before := h.cloud.Reads()

			_ = tc.write(ctx, h.plane.API, s)
			h.readIn(ctx)

			assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of a plane read after %s", tc.name)
		})
		t.Run(tc.name+" that Hetzner refuses has the request's next read list again", func(t *testing.T) {
			h := newHarness(t)
			s := h.seed()
			h.plane.API = controlplane.ReadOncePerRequest(refusesEveryWrite{h.cloud}, requestReadsOf)
			ctx := inRequest()
			h.readIn(ctx)
			before := h.cloud.Reads()

			err := tc.write(ctx, h.plane.API, s)
			h.readIn(ctx)

			require.ErrorIs(t, err, errRefused, "the error of %s", tc.name)
			assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of a plane read after a refused %s", tc.name)
		})
	}
}

var errRefused = errors.New("the request is not allowed (forbidden)")

// refusesEveryWrite answers each read from the fake and refuses each write before the fake sees it.
type refusesEveryWrite struct{ *cloudtest.Fake }

func (refusesEveryWrite) CreateServers(context.Context, ...controlplane.ServerSpec) ([]controlplane.Server, error) {
	return nil, errRefused
}
func (refusesEveryWrite) AttachServerToNetwork(context.Context, int64, int64, string) error {
	return errRefused
}
func (refusesEveryWrite) PowerOnServer(context.Context, int64) error  { return errRefused }
func (refusesEveryWrite) PowerOffServer(context.Context, int64) error { return errRefused }
func (refusesEveryWrite) DeleteServer(context.Context, int64) error   { return errRefused }
func (refusesEveryWrite) CreateNetwork(context.Context, controlplane.NetworkSpec) (controlplane.Network, error) {
	return controlplane.Network{}, errRefused
}
func (refusesEveryWrite) SetNetworkLabels(context.Context, int64, map[string]string) error {
	return errRefused
}
func (refusesEveryWrite) AddRoute(context.Context, int64, controlplane.Route) error {
	return errRefused
}
func (refusesEveryWrite) DeleteRoute(context.Context, int64, controlplane.Route) error {
	return errRefused
}
func (refusesEveryWrite) SetServerLabels(context.Context, int64, map[string]string) error {
	return errRefused
}
func (refusesEveryWrite) AddVSwitchSubnet(context.Context, int64, int64, string) error {
	return errRefused
}
func (refusesEveryWrite) DeleteNetwork(context.Context, int64) error { return errRefused }
func (refusesEveryWrite) CreateFirewall(context.Context, controlplane.FirewallSpec) (controlplane.Firewall, error) {
	return controlplane.Firewall{}, errRefused
}
func (refusesEveryWrite) DeleteFirewall(context.Context, int64) error { return errRefused }
func (refusesEveryWrite) CreateLoadBalancer(context.Context, controlplane.LoadBalancerSpec) (controlplane.LoadBalancer, error) {
	return controlplane.LoadBalancer{}, errRefused
}
func (refusesEveryWrite) AttachLoadBalancerToNetwork(context.Context, int64, int64, string) error {
	return errRefused
}
func (refusesEveryWrite) DisableLoadBalancerPublicInterface(context.Context, int64) error {
	return errRefused
}
func (refusesEveryWrite) AddLoadBalancerService(context.Context, int64, controlplane.Service) error {
	return errRefused
}
func (refusesEveryWrite) AddLoadBalancerTarget(context.Context, int64, string) error {
	return errRefused
}
func (refusesEveryWrite) AddLoadBalancerIPTarget(context.Context, int64, string) error {
	return errRefused
}
func (refusesEveryWrite) RemoveLoadBalancerTarget(context.Context, int64, string) error {
	return errRefused
}
func (refusesEveryWrite) DeleteLoadBalancer(context.Context, int64) error { return errRefused }
func (refusesEveryWrite) StartSnapshot(context.Context, int64, controlplane.SnapshotSpec) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, errRefused
}
func (refusesEveryWrite) DeleteSnapshot(context.Context, int64) error { return errRefused }

func TestPlaneRead_KeepsNoListAcrossRequests(t *testing.T) {
	h := newHarness(t, readingOncePerRequest)
	s := h.seed()
	first := inRequest()
	start := h.cloud.Reads()
	h.readIn(first)
	h.readIn(first)
	firstReads := h.cloud.Reads().Since(start)
	delete(h.cloud.LoadBalancers, s.loadBalancer)
	second := inRequest()
	before := h.cloud.Reads()

	h.readIn(second)
	st := h.readIn(second)

	assert.Equal(t, lists(1), firstReads, "the reads of two plane reads in the first request")
	assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of two plane reads in the second request")
	assert.Empty(t, st.LoadBalancers[controlplane.RoleAPI], "the API load balancer deleted outside mitstack between the requests")
}

// snapshotsFailOnce refuses its first snapshot list, the last of the five lists a plane read makes.
type snapshotsFailOnce struct {
	*cloudtest.Fake
	refused bool
}

func (f *snapshotsFailOnce) ListSnapshots(ctx context.Context, sel string) ([]controlplane.Snapshot, error) {
	snapshots, err := f.Fake.ListSnapshots(ctx, sel)
	if !f.refused {
		f.refused = true
		return nil, errors.New("listing snapshots: rate limit exceeded (rate_limit_exceeded)")
	}
	return snapshots, err
}

func TestPlaneRead_KeepsNothingFromAFailedRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(h *harness)
	}{
		{"the first list, of the servers, fails", func(h *harness) {
			h.cloud.FailNext = errors.New("listing servers: rate limit exceeded (rate_limit_exceeded)")
		}},
		{"the last list, of the snapshots, fails after the other four answered", func(h *harness) {
			h.plane.API = controlplane.ReadOncePerRequest(&snapshotsFailOnce{Fake: h.cloud}, requestReadsOf)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, readingOncePerRequest)
			h.seed()
			tc.fail(h)
			ctx := inRequest()

			_, failed := h.plane.Read(ctx)
			before := h.cloud.Reads()
			h.readIn(ctx)
			retried := h.cloud.Reads().Since(before)
			h.readIn(ctx)

			require.Error(t, failed, "the first plane read")
			assert.Equal(t, lists(1), retried, "the reads of the plane read after the failed one")
			assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of two plane reads after the failed one")
		})
	}
}

func TestPlaneRead_ListsTheCloudAsItIsOutsideTheRequest(t *testing.T) {
	h := newHarness(t, readingOncePerRequest)
	h.seed()
	ctx := inRequest()
	h.readIn(ctx)
	created, err := h.cloud.CreateServers(context.Background(), h.cloudWorkerSpec())
	require.NoError(t, err)

	kept := h.readIn(ctx)
	now := h.readIn(controlplane.WithoutRequestReads(ctx))

	assert.Empty(t, kept.Servers[controlplane.RoleCloudWorker], "the Cloud workers the request read before the create")
	assert.Equal(t, controlplane.IDs(created, controlplane.ServerID),
		controlplane.IDs(now.Servers[controlplane.RoleCloudWorker], controlplane.ServerID), "the Cloud workers a read outside the request lists")
}

func TestPlaneRead_ListsEachTimeWithNoRequestReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*harness)
		ctx  context.Context
	}{
		{"an API reading once per request, in a context holding no request's reads", []func(*harness){readingOncePerRequest}, context.Background()},
		{"the API the block hands the plane, in a request", nil, inRequest()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.seed()
			before := h.cloud.Reads()

			h.readIn(tc.ctx)
			h.readIn(tc.ctx)

			assert.Equal(t, lists(2), h.cloud.Reads().Since(before), "the reads of two plane reads")
		})
	}
}

// serversHeldOnce holds its first server list, after listing, until release closes, so a test can write meanwhile.
type serversHeldOnce struct {
	*cloudtest.Fake
	listed, release chan struct{}
	once            sync.Once
}

func (f *serversHeldOnce) ListServers(ctx context.Context, sel string) ([]controlplane.Server, error) {
	servers, err := f.Fake.ListServers(ctx, sel)
	f.once.Do(func() {
		close(f.listed)
		<-f.release
	})
	return servers, err
}

func relabelled(h *harness) map[string]string {
	labels := maps.Clone(h.plane.Labels(controlplane.RoleGateway))
	labels["relabelled"] = "true"
	return labels
}

func TestPlaneRead_KeepsNothingFromAListAWriteOverlapped(t *testing.T) {
	h := newHarness(t)
	s := h.seed()
	held := &serversHeldOnce{Fake: h.cloud, listed: make(chan struct{}), release: make(chan struct{})}
	h.plane.API = controlplane.ReadOncePerRequest(held, requestReadsOf)
	ctx := inRequest()
	labels := relabelled(h)

	overlapped := make(chan error, 1)
	go func() {
		_, err := h.plane.Read(ctx)
		overlapped <- err
	}()
	<-held.listed
	wrote := h.plane.API.SetServerLabels(ctx, s.server, labels)
	close(held.release)
	require.NoError(t, wrote, "the write made while the read listed")
	require.NoError(t, <-overlapped, "the read the write overlapped")
	before := h.cloud.Reads()
	st := h.readIn(ctx)

	gateway, ok := st.Server(controlplane.RoleGateway)
	require.True(t, ok, "the gateway the read after the write lists")
	assert.Equal(t, labels, gateway.Labels, "the gateway's labels the read after the write lists")
	assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of the plane read after the write")
}

// createHeld holds a server create until release closes, as Hetzner holds one until its action ends, with the server
// already made when madeFirst is set.
type createHeld struct {
	controlplane.API
	madeFirst        bool
	entered, release chan struct{}
}

func (c *createHeld) CreateServers(ctx context.Context, specs ...controlplane.ServerSpec) ([]controlplane.Server, error) {
	if !c.madeFirst {
		close(c.entered)
		<-c.release
		return c.API.CreateServers(ctx, specs...)
	}
	servers, err := c.API.CreateServers(ctx, specs...)
	close(c.entered)
	<-c.release
	return servers, err
}

func TestPlaneRead_ListsWhileAWriteRuns(t *testing.T) {
	h := newHarness(t)
	h.seed()
	held := &createHeld{API: h.cloud, madeFirst: true, entered: make(chan struct{}), release: make(chan struct{})}
	h.plane.API = controlplane.ReadOncePerRequest(held, requestReadsOf)
	ctx := inRequest()
	h.readIn(ctx)

	creating := make(chan error, 1)
	go func() {
		_, err := h.plane.API.CreateServers(ctx, h.cloudWorkerSpec())
		creating <- err
	}()
	<-held.entered
	before := h.cloud.Reads()
	during := h.readIn(ctx)
	h.readIn(ctx)
	duringReads := h.cloud.Reads().Since(before)
	close(held.release)

	require.NoError(t, <-creating, "the create the reads ran beside")
	assert.Len(t, during.Servers[controlplane.RoleCloudWorker], 1, "the Cloud workers a read lists while the create that made one still runs")
	assert.Equal(t, lists(2), duringReads, "the reads of two plane reads while a create runs")
}

func TestPlaneRead_KeepsNothingFromAListThatBeganWhileAWriteRan(t *testing.T) {
	h := newHarness(t)
	h.seed()
	listing := &serversHeldOnce{Fake: h.cloud, listed: make(chan struct{}), release: make(chan struct{})}
	held := &createHeld{API: listing, entered: make(chan struct{}), release: make(chan struct{})}
	h.plane.API = controlplane.ReadOncePerRequest(held, requestReadsOf)
	ctx := inRequest()

	creating := make(chan error, 1)
	go func() {
		_, err := h.plane.API.CreateServers(ctx, h.cloudWorkerSpec())
		creating <- err
	}()
	<-held.entered
	reading := make(chan error, 1)
	go func() {
		_, err := h.plane.Read(ctx)
		reading <- err
	}()
	<-listing.listed
	close(held.release)
	require.NoError(t, <-creating, "the create that ended while the read listed")
	close(listing.release)
	require.NoError(t, <-reading, "the read that began while the create ran")
	before := h.cloud.Reads()
	st := h.readIn(ctx)

	assert.Len(t, st.Servers[controlplane.RoleCloudWorker], 1, "the Cloud workers the read after the create lists")
	assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of the plane read after the create")
}

func TestPlaneRead_KeepsReadsInAZeroRequestReads(t *testing.T) {
	h := newHarness(t, readingOncePerRequest)
	h.seed()
	ctx := context.WithValue(context.Background(), requestReadsKey{}, &controlplane.RequestReads{})
	before := h.cloud.Reads()

	h.readIn(ctx)
	h.readIn(ctx)

	assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of two plane reads in a request holding a zero RequestReads")
}

func TestPlaneRead_ServesReadsAndWritesRunningTogether(t *testing.T) {
	const readsTogether = 8
	h := newHarness(t, readingOncePerRequest)
	s := h.seed()
	ctx := inRequest()
	labels := relabelled(h)

	var wg sync.WaitGroup
	for range readsTogether {
		wg.Go(func() {
			st, err := h.plane.Read(ctx)
			assert.NoError(t, err)
			_, ok := st.Server(controlplane.RoleGateway)
			assert.True(t, ok, "the gateway a read running beside the others lists")
		})
	}
	wg.Go(func() { assert.NoError(t, h.plane.API.SetServerLabels(ctx, s.server, labels)) })
	wg.Wait()
	st := h.readIn(ctx)

	gateway, ok := st.Server(controlplane.RoleGateway)
	require.True(t, ok, "the gateway the read after the reads and the write lists")
	assert.Equal(t, labels, gateway.Labels, "the gateway's labels the read after the reads and the write lists")
}

func TestPlaneRead_ServesManyWritesBesideTheReadsAndKeepsAgainOnceTheyEnd(t *testing.T) {
	const together = 16
	h := newHarness(t, readingOncePerRequest)
	s := h.seed()
	ctx := inRequest()
	labels := relabelled(h)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range together {
		wg.Go(func() {
			<-start
			_, err := h.plane.Read(ctx)
			assert.NoError(t, err)
		})
		wg.Go(func() {
			<-start
			assert.NoError(t, h.plane.API.SetServerLabels(ctx, s.server, labels))
		})
	}
	close(start)
	wg.Wait()
	require.NoError(t, h.plane.API.SetServerLabels(ctx, s.server, labels))
	before := h.cloud.Reads()
	h.readIn(ctx)
	st := h.readIn(ctx)

	assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of two plane reads once every write has ended")
	gateway, ok := st.Server(controlplane.RoleGateway)
	require.True(t, ok, "the gateway the read after every write lists")
	assert.Equal(t, labels, gateway.Labels, "the gateway's labels the read after every write lists")
}

// seedEveryField seeds one resource of each kind, a worker image among them, with every slice and map it has filled.
func (h *harness) seedEveryField() {
	s := h.seed()
	network := h.cloud.Networks[s.network]
	network.Routes = []controlplane.Route{{Destination: "10.1.0.0/16", Gateway: "10.0.0.2"}}
	network.VSwitchSubnets[s.network+100] = "10.0.1.0/24"
	h.cloud.Servers[s.server].FirewallIDs = []int64{s.firewall}
	lb := h.cloud.LoadBalancers[s.loadBalancer]
	lb.Services = []controlplane.Service{{ListenPort: 6443, DestinationPort: 6443}}
	lb.TargetSelectors = []string{h.plane.RoleSelector(controlplane.RoleNode)}
	lb.TargetIPs = []string{"10.0.0.4"}
	worker := h.cloud.NextID()
	h.cloud.Snapshots[worker] = &controlplane.Snapshot{
		ID: worker, Status: controlplane.SnapshotAvailable, Labels: h.plane.Labels(controlplane.RoleWorkerImage),
	}
}

func deepCopy[T any](v T) T {
	return copyValue(reflect.ValueOf(v)).Interface().(T)
}

func copyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out.Field(i).Set(copyValue(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(copyValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out.SetMapIndex(iter.Key(), copyValue(iter.Value()))
		}
		return out
	}
	return v
}

// changeEvery changes in place every value st's slices hold and adds an entry to every map, so whatever another state
// shares with st changes too, and it names each slice or map field it found holding something.
func changeEvery(st controlplane.State) map[string]bool {
	reached := map[string]bool{}
	change(reflect.ValueOf(&st).Elem(), reached)
	return reached
}

func change(v reflect.Value, reached map[string]bool) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			if k := field.Type.Kind(); (k == reflect.Slice || k == reflect.Map) && v.Field(i).Len() > 0 {
				reached[v.Type().Name()+"."+field.Name] = true
			}
			change(v.Field(i), reached)
		}
	case reflect.Slice:
		for i := range v.Len() {
			change(v.Index(i), reached)
		}
	case reflect.Map:
		for iter := v.MapRange(); iter.Next(); {
			change(iter.Value(), reached)
		}
		v.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem())
	case reflect.String:
		if v.CanSet() {
			v.SetString(v.String() + " changed")
		}
	case reflect.Int, reflect.Int64:
		if v.CanSet() {
			v.SetInt(v.Int() + 1)
		}
	case reflect.Bool:
		if v.CanSet() {
			v.SetBool(!v.Bool())
		}
	}
}

// sliceAndMapFields names every slice or map field of t and of each struct t holds, as changeEvery names them.
func sliceAndMapFields(t reflect.Type) map[string]bool {
	fields := map[string]bool{}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			if k := field.Type.Kind(); k == reflect.Slice || k == reflect.Map {
				fields[t.Name()+"."+field.Name] = true
			}
			walk(field.Type)
		}
	}
	walk(t)
	return fields
}

func TestPlaneRead_HandsEachReadItsOwnState(t *testing.T) {
	h := newHarness(t, readingOncePerRequest)
	h.seedEveryField()
	ctx := inRequest()
	first := h.readIn(ctx)
	want := deepCopy(first)

	reached := changeEvery(first)
	second := h.readIn(ctx)
	secondAsRead := deepCopy(second)
	changeEvery(second)
	last := h.readIn(ctx)

	assert.Equal(t, sliceAndMapFields(reflect.TypeFor[controlplane.State]()), reached,
		"the slices and maps the seed filled, each of which the test changes")
	assert.Equal(t, want, secondAsRead, "the read after the first read's state changed")
	assert.Equal(t, want, last, "the read after the second read's state changed")
}

func TestPlaneRead_KeepsEachPlaneApart(t *testing.T) {
	h := newHarness(t, readingOncePerRequest)
	h.seed()
	other := h.plane
	other.Subscription = "other-subscription"
	ctx := inRequest()
	h.readIn(ctx)
	before := h.cloud.Reads()

	st, err := other.Read(ctx)
	require.NoError(t, err)
	h.readIn(ctx)

	_, ok := st.Server(controlplane.RoleGateway)
	assert.False(t, ok, "a gateway in the other plane, which holds none")
	assert.Equal(t, lists(1), h.cloud.Reads().Since(before), "the reads of the other plane's first read and this plane's second")
}

// What Hetzner sells does not follow the project's own writes, so a write leaves the request's listings kept.
func TestReadOncePerRequest_ListsEachLocationsServerTypesOnceARequest(t *testing.T) {
	h := newHarness(t, readingOncePerRequest)
	h.seed()
	ctx := inRequest()
	before := h.cloud.Reads()
	listed := func() int { return h.cloud.Reads().Since(before)["AvailableServerTypes"] }
	list := func(ctx context.Context, loc string) []controlplane.ServerType {
		types, err := h.plane.API.AvailableServerTypes(ctx, loc)
		require.NoError(t, err)
		return types
	}

	first := list(ctx, "fsn1")
	first[0].Name = "changed by its caller"
	assert.Equal(t, list(inRequest(), "fsn1"), list(ctx, "fsn1"), "a listing kept for the request, as another request lists it")
	list(ctx, "nbg1")
	_, err := h.plane.API.CreateServers(ctx, h.cloudWorkerSpec())
	require.NoError(t, err)
	list(ctx, "fsn1")
	assert.Equal(t, 3, listed(), "the listings of fsn1 in two requests and of nbg1, with a create between")
	list(controlplane.WithoutRequestReads(ctx), "fsn1")
	list(context.Background(), "fsn1")
	assert.Equal(t, 5, listed(), "the listings once fsn1 is read outside the request and with no request")
}

var planeReads = []string{
	"ListServers", "AvailableServerTypes", "ServerStarted", "ListNetworks", "LastPowerAction", "ListFirewalls",
	"ListLoadBalancers", "ListSnapshots", "ServerMetrics", "RunningAction", "GetServer",
}

func TestReadOncePerRequest_SortsEachAPIMethodIntoAReadOrAWrite(t *testing.T) {
	var methods, sorted []string
	api := reflect.TypeFor[controlplane.API]()
	for i := range api.NumMethod() {
		methods = append(methods, api.Method(i).Name)
	}
	sorted = append(sorted, planeReads...)
	for _, w := range everyWrite {
		sorted = append(sorted, w.name)
	}

	assert.ElementsMatch(t, methods, sorted, "the API's methods against the reads and the writes these tests name")
}
