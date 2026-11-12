package controlplane_test

// A confirmed upgrade replaces a named control-plane node: the others read serving first, then the node is powered off,
// its etcd member and Node object go through the cluster repository, and only then its server is deleted and built anew.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cluster stands in for what the plane reaches only through its block: the removal files of the cluster repository,
// and what each node report reads of etcd, the Node objects, OpenBao and the removal Jobs Argo CD applied.
type cluster struct {
	h       *harness
	held    map[string][]byte
	commits []string
	// members and objects hold each hostname with an etcd member and a Node object, and jobs each removal Job's health.
	members, objects map[string]bool
	jobs             map[string]string
	// unhealthy names the nodes whose etcd member reads unhealthy, and sealed those whose OpenBao member reads sealed.
	unhealthy, sealed []string
	// failJobs is how many removal Jobs fail before one removes its member, and overwrote names each removal written
	// while Argo CD still held a Job of its name.
	failJobs  int
	overwrote []string
	// joinTakes is how long a node runs before it holds an etcd member and a Node object.
	joinTakes time.Duration
	synced    time.Time
}

func (c *cluster) Held(context.Context) ([]string, error) {
	return slices.Sorted(maps.Keys(c.held)), nil
}

func (c *cluster) Commit(_ context.Context, path string, content []byte) error {
	if content == nil {
		delete(c.held, path)
		c.commits = append(c.commits, "delete "+path)
		return nil
	}
	if c.jobs[strings.TrimSuffix(strings.TrimPrefix(path, "cluster/"), ".yaml")] != "" {
		c.overwrote = append(c.overwrote, path)
	}
	c.held[path] = content
	c.commits = append(c.commits, "write "+path)
	return nil
}

// sync lets a report interval pass in the cluster: each node running for joinTakes joins, Argo CD prunes each Job whose
// file is gone, and each Job not yet run removes its node's member and Node object, or fails.
func (c *cluster) sync(received time.Time) {
	if !received.After(c.synced) {
		return
	}
	c.synced = received
	nodes := c.h.byRole(controlplane.RoleNode)
	for _, node := range nodes {
		if node.Status == controlplane.ServerRunning && !received.Before(node.Created.Add(c.joinTakes)) {
			c.members[node.Name], c.objects[node.Name] = true, true
		}
	}
	for job := range c.jobs {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(c.held)), func(path string) bool { return strings.Contains(path, job) }) {
			delete(c.jobs, job)
		}
	}
	for hostname := range maps.Clone(c.objects) {
		job := etcd.JobName(hostname)
		if _, ran := c.jobs[job]; ran || c.held[etcd.RemovalPath(hostname)] == nil {
			continue
		}
		if c.failJobs > 0 {
			c.failJobs--
			c.jobs[job] = "Degraded"
			continue
		}
		delete(c.members, hostname)
		delete(c.objects, hostname)
		c.jobs[job] = "Healthy"
	}
}

// body is the node report: each Node object and etcd member, an OpenBao member on each node, and the cluster
// Application's removal Jobs.
func (c *cluster) body() []byte {
	running := map[string]bool{}
	for _, node := range c.h.byRole(controlplane.RoleNode) {
		running[node.Name] = node.Status == controlplane.ServerRunning
	}
	var nodes, members, pods, openBao, jobs []map[string]any
	for i, hostname := range slices.Sorted(maps.Keys(c.objects)) {
		member := "openbao-" + string(rune('0'+i))
		nodes = append(nodes, map[string]any{"name": hostname, "ready": running[hostname]})
		pods = append(pods, map[string]any{"namespace": "openbao", "name": member, "node": hostname})
		openBao = append(openBao, map[string]any{"name": member, "initialized": true, "sealed": !running[hostname] || slices.Contains(c.sealed, hostname)})
	}
	for _, hostname := range slices.Sorted(maps.Keys(c.members)) {
		members = append(members, map[string]any{"hostname": hostname, "healthy": running[hostname] && !slices.Contains(c.unhealthy, hostname)})
	}
	for _, job := range slices.Sorted(maps.Keys(c.jobs)) {
		jobs = append(jobs, map[string]any{"group": "batch", "kind": "Job", "namespace": "mitstack-node", "name": job, "health": c.jobs[job]})
	}
	data, err := json.Marshal(map[string]any{
		"images": []report.ImageRef{}, "nodes": nodes, "etcd": map[string]any{"members": members}, "pods": pods,
		"openbao": map[string]any{"members": openBao},
		"argocd":  map[string]any{"applications": []map[string]any{{"name": "cluster", "resources": append([]map[string]any{}, jobs...)}}},
	})
	require.NoError(c.h.t, err)
	return data
}

// withCluster builds the cluster with gateways that serve, a cluster repository and node reports every five minutes.
func withCluster(t *testing.T, opts ...func(*harness)) (*harness, *cluster) {
	t.Helper()
	h := newHarness(t, append([]func(*harness){withReports}, opts...)...)
	c := &cluster{h: h, held: map[string][]byte{}, members: map[string]bool{}, objects: map[string]bool{}, jobs: map[string]string{}}
	h.plane.Removals = c
	h.plane.ReadReport = func(_ context.Context, since time.Time) (*report.Read, error) {
		received := h.reportReceived()
		if !received.After(since) {
			return nil, nil
		}
		c.sync(received)
		return &report.Read{Body: c.body(), At: received}, nil
	}
	h.built()
	return h, c
}

// joinsSlowly makes each node created from now on take longer than two report intervals to join the nodes that stand.
func (c *cluster) joinsSlowly() {
	for _, node := range c.h.byRole(controlplane.RoleNode) {
		c.members[node.Name], c.objects[node.Name] = true, true
	}
	c.joinTakes = 2*reportEvery + time.Minute
}

func (h *harness) nodeIn(loc string) controlplane.Server {
	h.t.Helper()
	node, ok := h.read().NodeIn(loc)
	require.True(h.t, ok, "the cluster has no single node in %s", loc)
	return node
}

func (h *harness) replacing() bool {
	return slices.ContainsFunc(slices.Collect(maps.Keys(h.network().Labels)), func(key string) bool {
		return strings.HasPrefix(key, controlplane.KeyNodeReplace)
	})
}

// replace asks an upgrade to replace the nodes in locs, then calls the plane as a block's re-drives do, naming none,
// with a minute after each call that changed nothing, until until reads true, a call fails or no replacement is left.
func (h *harness) replace(c *cluster, until func() bool, locs ...string) error {
	h.t.Helper()
	h.plane.ReplaceNodes, h.plane.Upgrading = locs, len(locs) > 0
	err := h.ensure()
	h.plane.ReplaceNodes, h.plane.Upgrading = nil, false
	for calls := 0; err == nil; calls++ {
		require.Less(h.t, calls, 400, "the replacement did not finish within 400 calls")
		if until != nil && until() {
			return nil
		}
		before := h.cloud.Writes() + len(c.commits)
		if err = h.ensure(); err != nil || h.cloud.Writes()+len(c.commits) > before {
			continue
		}
		if !h.replacing() {
			return nil
		}
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}
	return err
}

func (h *harness) eventsNaming(part string) []string {
	return slices.DeleteFunc(slices.Clone(h.events), func(event string) bool { return !strings.Contains(event, part) })
}

func (h *harness) problems() string {
	return strings.Join(h.plane.Problems(context.Background(), h.read()), "; ")
}

func TestPlane_AConfirmedUpgradeReplacesTheNamedNodeFromTheKeptSnapshotOnceItsEtcdMemberAndNodeObjectAreGone(t *testing.T) {
	h, c := withCluster(t)
	old, network := h.nodeIn("fsn1"), h.network()
	kept := map[string]int64{"nbg1": h.nodeIn("nbg1").ID, "hel1": h.nodeIn("hel1").ID}
	snapshot, ok := h.read().Snapshot()
	require.True(t, ok, "the cluster keeps no single snapshot")
	path := etcd.RemovalPath(old.Name)
	var removedBeforeDelete bool
	watch := func() bool {
		if _, stands := h.cloud.Servers[old.ID]; stands && !c.members[old.Name] && !c.objects[old.Name] {
			removedBeforeDelete = true
		}
		return false
	}

	require.NoError(t, h.replace(c, watch, "fsn1"))

	successor := h.nodeIn("fsn1")
	assert.NotEqual(t, old.ID, successor.ID, "the node in fsn1 after its replacement")
	assert.Equal(t, old.Name, successor.Name, "the successor's name")
	assert.Equal(t, snapshot.ID, successor.ImageID, "the image the successor booted from")
	assert.NotEmpty(t, successor.PrivateIPs[network.ID], "the successor's address on the cluster's network %d", network.ID)
	assert.Equal(t, network.ID, h.network().ID, "the cluster's network after the replacement")
	assert.Equal(t, kept, map[string]int64{"nbg1": h.nodeIn("nbg1").ID, "hel1": h.nodeIn("hel1").ID}, "the nodes the upgrade did not name")
	assert.True(t, removedBeforeDelete, "the old server stood until its etcd member and Node object were gone")
	assert.Equal(t, []string{"write " + path, "delete " + path}, c.commits, "the removal's commits to the cluster repository")
	assert.False(t, h.replacing(), "the network still carries a replacement's mark: %v", h.network().Labels)
	assert.Len(t, h.eventsNaming("powered off node"), 1, "the events of a power-off: %v", h.events)
}

func TestPlane_RefusesANodeReplacementThroughTheRedundancyGuardNamingTheMemberThatDoesNotServe(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *cluster, other string)
		want string
	}{
		{"another etcd member reads unhealthy", func(c *cluster, other string) { c.unhealthy = []string{other} }, "etcd members"},
		{"the OpenBao member on another node reads sealed", func(c *cluster, other string) { c.sealed = []string{other} }, "OpenBao members"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := withCluster(t)
			old, other := h.nodeIn("fsn1"), h.nodeIn("nbg1")
			tc.set(c, other.Name)

			err := h.replace(c, nil, "fsn1")

			require.Error(t, err, "the replacement of the node in fsn1 while %s", tc.name)
			assert.Equal(t, redundant.ClassBelowMinimum, refusalClass(t, err), "the refusal's class")
			assert.Contains(t, err.Error(), tc.want, "the set the refusal names")
			named := other.Name
			if tc.want == "OpenBao members" {
				named = "openbao-" + string(rune('0'+slices.Index(slices.Sorted(maps.Keys(c.objects)), other.Name)))
			}
			assert.Contains(t, err.Error(), `"`+named+`" read not serving`, "the member the refusal names")
			assert.Equal(t, controlplane.ServerRunning, h.nodeIn("fsn1").Status, "the node the refused replacement named")
			assert.Equal(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after the refusal")
			assert.Empty(t, c.commits, "the commits of a refused replacement")
			assert.False(t, h.replacing(), "the network still carries the refused replacement's mark: %v", h.network().Labels)

			c.unhealthy, c.sealed = nil, nil
			require.NoError(t, h.replace(c, nil), "a later call that names no node, once every member serves")
			assert.Equal(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after that call")
		})
	}
}

func TestPlane_AReplacementStoppedAfterItsNodePoweredOffResumesAtTheMemberRemovalOnACallThatNamesNoNode(t *testing.T) {
	h, c := withCluster(t)
	old, restarted := h.nodeIn("fsn1"), h.plane
	poweredOff := func() bool { return h.cloud.Servers[old.ID].Status == controlplane.ServerOff }
	require.NoError(t, h.replace(c, poweredOff, "fsn1"))
	require.Empty(t, c.commits, "precondition: the replacement stopped before its member removal")
	h.plane = restarted

	require.NoError(t, h.replace(c, nil))

	assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 once the resumed replacement ended")
	assert.Len(t, h.eventsNaming("powered off node"), 1, "the resumed replacement powered a node off again: %v", h.events)
	assert.Len(t, h.eventsNaming("began replacing"), 1, "the resumed replacement began again: %v", h.events)
	assert.Len(t, h.byRole(controlplane.RoleNode), len(controlplane.Locations), "the cluster's nodes")
}

func TestPlane_DeletesAFinishedRemovalJobBeforeItAppliesOne(t *testing.T) {
	t.Run("a Job that failed and left its member is pruned before it is applied again", func(t *testing.T) {
		h, c := withCluster(t)
		old := h.nodeIn("fsn1")
		path := etcd.RemovalPath(old.Name)
		c.failJobs = 1

		require.NoError(t, h.replace(c, nil, "fsn1"))

		assert.Equal(t, []string{"write " + path, "delete " + path, "write " + path, "delete " + path}, c.commits, "the commits to the cluster repository")
		assert.Empty(t, c.overwrote, "the removals written while Argo CD still held a Job of their name")
		assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after its replacement")
	})

	t.Run("an earlier replacement's removal is taken out before another node's is written", func(t *testing.T) {
		h, c := withCluster(t)
		finished := h.nodeIn("hel1").Name
		earlier := etcd.RemovalPath(finished)
		c.held[earlier], c.jobs[etcd.JobName(finished)] = []byte("kind: Job\n"), "Healthy"
		path := etcd.RemovalPath(h.nodeIn("fsn1").Name)

		require.NoError(t, h.replace(c, nil, "fsn1"))

		assert.Equal(t, []string{"delete " + earlier, "write " + path, "delete " + path}, c.commits, "the commits to the cluster repository")
		assert.Empty(t, c.overwrote, "the removals written while Argo CD still held a Job of their name")
		assert.Len(t, h.byRole(controlplane.RoleNode), len(controlplane.Locations), "the cluster's nodes, hel1's among them")
	})
}

func TestPlane_AReplacedNodeTakesTheListsNextTypeWhenItsLocationCannotOrderTheFirstAndTheEventNamesIt(t *testing.T) {
	h, c := withCluster(t, func(h *harness) { h.plane.ServerTypes = []string{"cx23", "cpx22"} })
	require.Equal(t, "cx23", h.nodeIn("fsn1").ServerType, "precondition: the node's type before the first sold out")
	h.cloud.SoldOutIn = map[string][]string{"fsn1": {"cx23"}}

	require.NoError(t, h.replace(c, nil, "fsn1"))

	assert.Equal(t, "cpx22", h.nodeIn("fsn1").ServerType, "the successor's server type in fsn1")
	named := h.eventsNaming("cpx22")
	require.Len(t, named, 1, "the events naming the type taken: %v", h.events)
	assert.Contains(t, named[0], "fsn1", "the event naming the type taken")
	assert.Contains(t, named[0], "cx23", "the event names the type Hetzner could not order")
}

func TestPlane_RefusesANodeReplacementBeforeAnyChangeToTheNode(t *testing.T) {
	soldOut := func(h *harness) { h.cloud.SoldOutIn = map[string][]string{"fsn1": h.plane.ServerTypes} }
	for _, tc := range []struct {
		name string
		set  func(h *harness)
		// late sets the case only once the upgrade's own call has ended.
		late  bool
		loc   string
		class string
	}{
		{"its location can order no successor", soldOut, false, "fsn1", "server-unavailable"},
		{"its location can order no successor any more once the upgrade's call ended", soldOut, true, "fsn1", "server-unavailable"},
		{"the upgrade names a location the cluster runs no node in", func(*harness) {}, false, "ash", "node-replacement-unknown"},
		{"the block passes no cluster repository to write the member removal to", func(h *harness) { h.plane.Removals = nil }, false, "fsn1", "node-removal-unwritable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := withCluster(t)
			old := h.nodeIn("fsn1")
			var until func() bool
			if tc.late {
				until = func() bool { tc.set(h); return false }
			} else {
				tc.set(h)
			}

			err := h.replace(c, until, tc.loc)

			require.Error(t, err, "the replacement while %s", tc.name)
			assert.Equal(t, tc.class, refusalClass(t, err), "the refusal's class")
			assert.Equal(t, old, h.nodeIn("fsn1"), "the node in fsn1 after the refusal")
			assert.Empty(t, c.commits, "the commits of a refused replacement")
			assert.False(t, h.replacing(), "the network still carries the refused replacement's mark: %v", h.network().Labels)
		})
	}
}

func TestPlane_NamesAReplacementUnderWayInItsProblemsUntilANodeReportReadsItsSuccessorServing(t *testing.T) {
	h, c := withCluster(t)
	c.joinsSlowly()
	old := h.nodeIn("fsn1")
	require.NotContains(t, h.problems(), "fsn1", "precondition: the plane's problems before the replacement")

	require.NoError(t, h.replace(c, func() bool { return h.cloud.Servers[old.ID].Status == controlplane.ServerOff }, "fsn1"))
	assert.Contains(t, h.problems(), fmt.Sprintf("node %d in fsn1 is powered off", old.ID), "the plane's problems while the old node is off")

	require.NoError(t, h.replace(c, func() bool { node, ok := h.read().NodeIn("fsn1"); return ok && node.ID != old.ID }))
	successor := h.nodeIn("fsn1")
	assert.Contains(t, h.problems(), fmt.Sprintf("node %d in fsn1 has not been read serving", successor.ID), "the plane's problems once the successor exists")
	assert.True(t, h.replacing(), "the replacement's mark before any report reads the successor")

	require.NoError(t, h.replace(c, nil))
	assert.GreaterOrEqual(t, h.now.Sub(successor.Created), c.joinTakes, "how long after the successor's creation its replacement ended")
	assert.NotContains(t, h.problems(), "fsn1", "the plane's problems once a report reads the successor serving")
}

func TestPlane_ReplacesTheNodesAnUpgradeNamesOneAtATimeAndNoneOnACallThatIsNoUpgrade(t *testing.T) {
	h, c := withCluster(t)
	c.joinsSlowly()
	before := h.byRole(controlplane.RoleNode)
	h.plane.ReplaceNodes = []string{"fsn1"}
	require.NoError(t, h.converge())
	require.Equal(t, before, h.byRole(controlplane.RoleNode), "a call that is no upgrade changed a node")
	var mostDown int
	watch := func() bool {
		down := len(controlplane.Locations)
		for _, node := range h.byRole(controlplane.RoleNode) {
			if node.Status == controlplane.ServerRunning && c.members[node.Name] {
				down--
			}
		}
		mostDown = max(mostDown, down)
		return false
	}

	require.NoError(t, h.replace(c, watch, "fsn1", "nbg1"))

	assert.Equal(t, 1, mostDown, "the most locations without a running node that holds an etcd member at once")
	after := h.byRole(controlplane.RoleNode)
	require.Len(t, after, len(controlplane.Locations), "the cluster's nodes")
	for _, node := range after {
		replaced := !slices.ContainsFunc(before, func(b controlplane.Server) bool { return b.ID == node.ID })
		assert.Equal(t, node.Location != "hel1", replaced, "whether the node in %s was replaced", node.Location)
	}
}
