package controlplane_test

// A confirmed upgrade replaces a named control-plane node: the others read serving first, then the node is powered off,
// its etcd member and Node object go through the cluster repository, and only then its server is deleted and built anew.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cluster stands in for what the plane reaches only through its block: the cluster repository with the Argo CD that
// syncs it, and what each node report reads of etcd, the Node objects, OpenBao and the objects Argo CD applied.
type cluster struct {
	h       *harness
	argo    *testutil.Cluster
	commits []string
	// members and objects hold each hostname with an etcd member and a Node object, and jobs each removal Job's health.
	members, objects map[string]bool
	jobs             map[string]string
	// unhealthy names the nodes whose etcd member reads unhealthy, and sealed those whose OpenBao member reads sealed.
	unhealthy, sealed []string
	// failJobs is how many removal Jobs fail before one removes its member, halfway has a failing Job remove the etcd
	// member first, and overwrote names each removal written while Argo CD still held a Job of its name.
	failJobs  int
	halfway   bool
	overwrote []string
	// joinTakes is how long a node runs before it holds an etcd member and a Node object.
	joinTakes time.Duration
	// reported is the last report's body, which reads the cluster as it stood when that report was received at synced.
	reported []byte
	synced   time.Time
	// unread is what a node report leaves out of each Application: its "resources", or the "health" of each of them.
	unread string
}

func removalJob(name string) testutil.ObjectRef {
	return testutil.ObjectRef{Group: "batch", Kind: "Job", Namespace: "mitstack-node", Name: name}
}

func (c *cluster) Held(context.Context) ([]string, error) {
	return slices.DeleteFunc(c.argo.Files(), func(path string) bool { return !etcd.IsRemovalPath(path) }), nil
}

func (c *cluster) holds(path string) bool { return slices.Contains(c.argo.Files(), path) }

func (c *cluster) Commit(_ context.Context, path string, content []byte) error {
	verb := "delete "
	if content != nil {
		verb = "write "
		for _, doc := range testutil.Documents(c.h.t, content) {
			if doc["kind"] == "Job" && c.argo.Object(removalJob(testutil.Field(c.h.t, doc, "metadata", "name").(string))) != nil {
				c.overwrote = append(c.overwrote, path)
			}
		}
	}
	c.argo.Commit(map[string][]byte{path: content})
	c.commits = append(c.commits, verb+path)
	return nil
}

func (c *cluster) sync(received time.Time) {
	c.synced = received
	for _, node := range c.h.byRole(controlplane.RoleNode) {
		if node.Status == controlplane.ServerRunning && !received.Before(node.Created.Add(c.joinTakes)) {
			c.members[node.Name], c.objects[node.Name] = true, true
		}
	}
	c.argo.Sync()
	for job := range c.jobs {
		if c.argo.Object(removalJob(job)) == nil {
			delete(c.jobs, job)
		}
	}
	for hostname := range maps.Clone(c.objects) {
		job := etcd.JobName(hostname)
		if _, ran := c.jobs[job]; ran || c.argo.Object(removalJob(job)) == nil {
			continue
		}
		if c.failJobs > 0 {
			c.failJobs--
			c.jobs[job] = "Degraded"
			if c.halfway {
				delete(c.members, hostname)
			}
			continue
		}
		delete(c.members, hostname)
		delete(c.objects, hostname)
		c.jobs[job] = "Healthy"
	}
}

func (c *cluster) body() []byte {
	running := map[string]bool{}
	for _, node := range c.h.byRole(controlplane.RoleNode) {
		running[node.Name] = node.Status == controlplane.ServerRunning
	}
	var nodes, members, pods, openBao []map[string]any
	for i, hostname := range slices.Sorted(maps.Keys(c.objects)) {
		member := "openbao-" + string(rune('0'+i))
		nodes = append(nodes, map[string]any{"name": hostname, "ready": running[hostname]})
		pods = append(pods, map[string]any{"namespace": "openbao", "name": member, "node": hostname})
		openBao = append(openBao, map[string]any{"name": member, "initialized": true, "sealed": !running[hostname] || slices.Contains(c.sealed, hostname)})
	}
	for _, hostname := range slices.Sorted(maps.Keys(c.members)) {
		members = append(members, map[string]any{"hostname": hostname, "healthy": running[hostname] && !slices.Contains(c.unhealthy, hostname)})
	}
	var body map[string]any
	require.NoError(c.h.t, json.Unmarshal(c.argo.Report(), &body))
	for _, app := range testutil.Field(c.h.t, body, "argocd", "applications").([]any) {
		app := app.(map[string]any)
		if c.unread == "resources" {
			delete(app, "resources")
			continue
		}
		for _, r := range app["resources"].([]any) {
			if r := r.(map[string]any); c.unread != "health" {
				r["health"] = c.health(r)
				if r["health"] == "Degraded" {
					r["message"] = "Job has reached the specified backoff limit"
				}
			}
		}
	}
	maps.Copy(body, map[string]any{
		"images": []report.ImageRef{}, "nodes": nodes, "etcd": map[string]any{"members": members}, "pods": pods,
		"openbao": map[string]any{"members": openBao},
	})
	data, err := json.Marshal(body)
	require.NoError(c.h.t, err)
	return data
}

// health is what Argo CD keeps of an applied object: a Job's own, and none for a kind it has no health check for.
func (c *cluster) health(resource map[string]any) any {
	if resource["kind"] != "Job" {
		return nil
	}
	return cmp.Or(c.jobs[resource["name"].(string)], "Progressing")
}

func withCluster(t *testing.T, opts ...func(*harness)) (*harness, *cluster) {
	t.Helper()
	h := newHarness(t, append([]func(*harness){withReports}, opts...)...)
	c := &cluster{h: h, argo: testutil.BuildCluster(t), members: map[string]bool{}, objects: map[string]bool{}, jobs: map[string]string{}}
	c.argo.Commit(map[string][]byte{"cluster/namespace.yaml": []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: mitstack-node\n")})
	c.argo.Sync()
	h.plane.Removals = &controlplane.Removals{Held: c.Held, Commit: c.Commit}
	h.plane.ReadReport = func(_ context.Context, since time.Time) (*report.Read, error) {
		received := h.reportReceived()
		if !received.After(since) {
			return nil, nil
		}
		if received.After(c.synced) {
			c.sync(received)
			c.reported = c.body()
		}
		return &report.Read{Body: c.reported, At: received}, nil
	}
	h.built()
	return h, c
}

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

func (h *harness) upgradingGateways() bool {
	_, marked := h.network().Labels[controlplane.KeyGatewayUpgrade]
	return marked
}

// upgradeCall is the upgrade's own call, the one call that names the nodes it replaces.
func (h *harness) upgradeCall(locs ...string) error {
	h.plane.ReplaceNodes, h.plane.Upgrading = locs, true
	defer func() { h.plane.ReplaceNodes, h.plane.Upgrading = nil, false }()
	return h.ensure()
}

// replace re-drives the plane as a block does, naming no node after the upgrade's own call, until a call fails or no
// replacement of a node or of the gateways is left.
func (h *harness) replace(c *cluster, until func() bool, locs ...string) error {
	h.t.Helper()
	call := h.ensure
	if len(locs) > 0 {
		call = func() error { return h.upgradeCall(locs...) }
	}
	err := call()
	for calls := 0; err == nil; calls++ {
		require.Less(h.t, calls, 400, "the replacement did not finish within 400 calls")
		if until != nil && until() {
			return nil
		}
		before := h.cloud.Writes() + len(c.commits)
		if err = h.ensure(); err != nil || h.cloud.Writes()+len(c.commits) > before {
			continue
		}
		if !h.replacing() && !h.upgradingGateways() {
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

func TestPlane_PowersTheNodeBackOnAndWithdrawsWhenASecondMemberStopsServingBeforeItsMemberIsRemoved(t *testing.T) {
	h, c := withCluster(t)
	old, other := h.nodeIn("fsn1"), h.nodeIn("nbg1")
	require.NoError(t, h.replace(c, func() bool { return h.cloud.Servers[old.ID].Status == controlplane.ServerOff }, "fsn1"))
	c.unhealthy = []string{other.Name}

	err := h.replace(c, nil)

	require.Error(t, err, "the replacement once the etcd member of %s read unhealthy with the node in fsn1 off", other.Name)
	assert.Equal(t, redundant.ClassBelowMinimum, refusalClass(t, err), "the refusal's class")
	assert.Contains(t, err.Error(), `"`+other.Name+`" read not serving`, "the member the refusal names")
	assert.Equal(t, controlplane.ServerRunning, h.nodeIn("fsn1").Status, "the node in fsn1 after the refusal")
	assert.Equal(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after the refusal")
	assert.Empty(t, c.commits, "the commits of a replacement refused before its member's removal")
	assert.False(t, h.replacing(), "the network still carries the refused replacement's mark: %v", h.network().Labels)
	assert.Len(t, h.eventsNaming("back on"), 1, "the events of the power-on: %v", h.events)
}

func TestPlane_PowersTheNodeBackOnAndWithdrawsWhenItsRemovalJobFailedAndLeftItsEtcdMember(t *testing.T) {
	h, c := withCluster(t)
	old := h.nodeIn("fsn1")
	path := etcd.RemovalPath(old.Name)
	c.failJobs = 1

	err := h.replace(c, nil, "fsn1")

	require.Error(t, err, "the replacement of the node in fsn1, whose removal Job failed with its etcd member still listed")
	assert.Equal(t, "node-removal-failed", refusalClass(t, err), "the refusal's class")
	assert.Contains(t, err.Error(), "Job has reached the specified backoff limit", "the message Argo CD gives of the failed Job")
	assert.Equal(t, controlplane.ServerRunning, h.nodeIn("fsn1").Status, "the node in fsn1 after the refusal")
	assert.Equal(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after the refusal")
	assert.Equal(t, []string{"write " + path, "delete " + path}, c.commits, "the commits to the cluster repository")
	assert.False(t, h.replacing(), "the network still carries the refused replacement's mark: %v", h.network().Labels)

	require.NoError(t, h.replace(c, nil, "fsn1"), "a later upgrade that names the node again")
	assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after the later upgrade")
	assert.Empty(t, c.overwrote, "the removals written while Argo CD still held a Job of their name")
}

func TestPlane_RefusesToCarryAReplacementOnWhenALaterCallPassesNoRepositoryNoReportReadOrNoGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unset func(h *harness)
		class string
	}{
		{"no cluster repository", func(h *harness) { h.plane.Removals = nil }, "node-removal-unwritable"},
		{"no node report read", func(h *harness) { h.plane.ReadReport = nil }, "node-replacement-unjudged"},
		{"no redundancy guard", func(h *harness) { h.plane.Guard = nil }, "node-replacement-unjudged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := withCluster(t)
			old := h.nodeIn("fsn1")
			require.NoError(t, h.replace(c, func() bool { return h.cloud.Servers[old.ID].Status == controlplane.ServerOff }, "fsn1"))
			tc.unset(h)

			err := h.replace(c, nil)

			require.Error(t, err, "the call that passes %s with the node in fsn1 off", tc.name)
			assert.Equal(t, tc.class, refusalClass(t, err), "the refusal's class")
			assert.True(t, h.replacing(), "the record of the replacement under way")
			assert.Empty(t, c.commits, "the commits of the refused call")
		})
	}
}

func TestPlane_TakesAHeldRemovalOutBeforeItCreatesASuccessorForANodeWhoseServerWentMeanwhile(t *testing.T) {
	h, c := withCluster(t)
	old := h.nodeIn("fsn1")
	path := etcd.RemovalPath(old.Name)
	require.NoError(t, h.replace(c, func() bool { return len(c.commits) == 1 }, "fsn1"))
	require.NoError(t, h.cloud.DeleteServer(context.Background(), old.ID), "deleting the old server behind the plane's back")
	var heldBesideSuccessor bool
	watch := func() bool {
		if _, stands := h.read().NodeIn("fsn1"); stands && c.holds(path) {
			heldBesideSuccessor = true
		}
		return false
	}

	require.NoError(t, h.replace(c, watch))

	assert.False(t, heldBesideSuccessor, "the removal of %s was still held once a successor of that name stood", old.Name)
	assert.Equal(t, []string{"write " + path, "delete " + path}, c.commits, "the commits to the cluster repository")
	assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 once the replacement ended")
	assert.False(t, h.replacing(), "the network still carries the replacement's mark: %v", h.network().Labels)
}

func TestPlane_DeletesAFinishedRemovalJobBeforeItAppliesOne(t *testing.T) {
	t.Run("a Job that removed the etcd member and then failed is pruned before it is applied again", func(t *testing.T) {
		h, c := withCluster(t)
		old := h.nodeIn("fsn1")
		path := etcd.RemovalPath(old.Name)
		c.failJobs, c.halfway = 1, true

		require.NoError(t, h.replace(c, nil, "fsn1"))

		assert.Equal(t, []string{"write " + path, "delete " + path, "write " + path, "delete " + path}, c.commits, "the commits to the cluster repository")
		assert.Len(t, h.eventsNaming("whose Job failed"), 1, "the events naming the failed Job: %v", h.events)
		assert.Empty(t, h.eventsNaming("back on"), "the events of a power-on, for a node whose etcd member is gone")
		assert.Empty(t, c.overwrote, "the removals written while Argo CD still held a Job of their name")
		assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after its replacement")
	})

	t.Run("an earlier replacement's removal is taken out before another node's is written", func(t *testing.T) {
		h, c := withCluster(t)
		finished := h.nodeIn("hel1").Name
		earlier := etcd.RemovalPath(finished)
		job := etcd.JobName(finished)
		c.argo.Commit(map[string][]byte{earlier: []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: " + job + "\n  namespace: mitstack-node\n")})
		c.argo.Sync()
		c.jobs[job] = "Healthy"
		path := etcd.RemovalPath(h.nodeIn("fsn1").Name)

		require.NoError(t, h.replace(c, nil, "fsn1"))

		assert.Equal(t, []string{"delete " + earlier, "write " + path, "delete " + path}, c.commits, "the commits to the cluster repository")
		assert.Empty(t, c.overwrote, "the removals written while Argo CD still held a Job of their name")
		assert.Len(t, h.byRole(controlplane.RoleNode), len(controlplane.Locations), "the cluster's nodes, hel1's among them")
	})
}

func TestPlane_RefusesANodeReplacementWhileItsNodeRunsWhenTheNodeReportCarriesNoHealthOfWhatArgoCDApplied(t *testing.T) {
	for _, unread := range []string{"resources", "health"} {
		t.Run("a report without the "+unread+" of the cluster's own Application", func(t *testing.T) {
			h, c := withCluster(t)
			old := h.nodeIn("fsn1")
			c.unread = unread

			err := h.replace(c, nil, "fsn1")

			require.Error(t, err, "the replacement of the node in fsn1, whose failed removal Job no report could show")
			assert.Equal(t, "node-replacement-unjudged", refusalClass(t, err), "the refusal's class")
			assert.Contains(t, err.Error(), "Application cluster", "the Application the refusal names")
			assert.Equal(t, controlplane.ServerRunning, h.nodeIn("fsn1").Status, "the node the refused replacement named")
			assert.Equal(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 after the refusal")
			assert.Empty(t, c.commits, "the commits of a refused replacement")
			assert.False(t, h.replacing(), "the network still carries the refused replacement's mark: %v", h.network().Labels)
		})
	}
}

func TestPlane_NamesARemovalJobWhoseHealthNoNodeReportCarriesAndActsOnItsFailureOnceAReportDoes(t *testing.T) {
	for _, unread := range []string{"resources", "health"} {
		t.Run("a report without the "+unread+" of the cluster's own Application", func(t *testing.T) {
			h, c := withCluster(t)
			old := h.nodeIn("fsn1")
			job := etcd.JobName(old.Name)
			c.failJobs = 1
			require.NoError(t, h.replace(c, func() bool { return len(c.commits) == 1 }, "fsn1"))
			c.unread = unread

			require.NoError(t, h.replace(c, func() bool { return c.jobs[job] != "" }))

			assert.Contains(t, h.problems(), "removal Job "+job+" cannot be read", "the plane's problems while no report carries the failed Job's health")
			assert.Len(t, c.commits, 1, "the commits while no report carried the failed Job's health: %v", c.commits)

			c.unread = ""
			err := h.replace(c, nil)
			require.Error(t, err, "the replacement once a report carried the failed Job's health")
			assert.Equal(t, "node-removal-failed", refusalClass(t, err), "the refusal's class")
			assert.Equal(t, controlplane.ServerRunning, h.nodeIn("fsn1").Status, "the node in fsn1 once a report carried the Job's health")
		})
	}
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
		{"the block reads no node report to judge the other members by", func(h *harness) { h.plane.ReadReport = nil }, false, "fsn1", "node-replacement-unjudged"},
		{"the block passes no redundancy guard", func(h *harness) { h.plane.Guard = nil }, false, "fsn1", "node-replacement-unjudged"},
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

func (h *harness) standingOf(ids []int64) []int64 {
	return slices.DeleteFunc(h.gatewayIDs(), func(id int64) bool { return !slices.Contains(ids, id) })
}

// touched reports whether the replacement of node has begun: its removal is labelled, or it is off or gone.
func (h *harness) touched(node controlplane.Server) bool {
	now, stands := h.cloud.Servers[node.ID]
	return !stands || now.Status != controlplane.ServerRunning || now.Labels[controlplane.KeyRemovalBegan] != ""
}

func (h *harness) deleteFirewall() {
	h.t.Helper()
	firewall, ok := h.read().Firewall()
	require.True(h.t, ok, "the cluster has no single firewall")
	require.NoError(h.t, h.cloud.DeleteFirewall(context.Background(), firewall.ID))
}

func (h *harness) deleteIngressLoadBalancer() {
	h.t.Helper()
	require.NoError(h.t, h.cloud.DeleteLoadBalancer(context.Background(), h.loadBalancer(controlplane.RoleIngress).ID))
}

func TestPlane_AnUpgradeNamingTheNodeInFsn1WhileANewGatewayListIsDueReplacesEveryGatewayAndThenTheNodeWithNoSecondUpgrade(t *testing.T) {
	h, c := withCluster(t)
	old := h.nodeIn("fsn1")
	path := etcd.RemovalPath(old.Name)
	kept := map[string]int64{"nbg1": h.nodeIn("nbg1").ID, "hel1": h.nodeIn("hel1").ID}
	earlier := h.gatewayIDs()
	h.relabelGateways("an-earlier-list")
	asked, from := h.now, len(h.events)
	var began bool
	var standingAtBegin []int64
	watch := func() bool {
		if !began && h.touched(old) {
			began, standingAtBegin = true, h.standingOf(earlier)
		}
		return false
	}

	require.NoError(t, h.upgradeCall("fsn1"))
	recorded, waiting := maps.Clone(h.network().Labels), h.problems()
	require.NoError(t, h.replace(c, watch))

	assert.Contains(t, recorded, controlplane.KeyNodeReplace+"fsn1", "the network's labels once the upgrade's own call ended")
	assert.Contains(t, waiting, fmt.Sprintf("node %d in fsn1 still runs for the replacement the upgrade asked for at %s: it begins once the upgrade has replaced every gateway",
		old.ID, asked.Format(time.RFC3339)), "the plane's problems once the upgrade's own call ended")
	assert.Len(t, h.deletedGateways(from), len(controlplane.Locations), "the gateways the upgrade deleted: %v", h.events[from:])
	assert.Empty(t, h.standingOf(earlier), "the gateways of the earlier list once no step is left")
	assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 once no step is left")
	assert.Equal(t, kept, map[string]int64{"nbg1": h.nodeIn("nbg1").ID, "hel1": h.nodeIn("hel1").ID}, "the nodes the upgrade did not name")
	assert.Equal(t, []string{"write " + path, "delete " + path}, c.commits, "the removal's commits to the cluster repository")
	assert.True(t, began, "the replacement of the node in fsn1 never began")
	assert.Empty(t, standingAtBegin, "the gateways of the earlier list that stood when the node in fsn1's replacement began")
	assert.Empty(t, h.problems(), "the plane's problems once no step is left")
}

func TestPlane_AnUpgradeNamingANodeWhileAnEarlierStepIsDueRecordsItOnItsOwnCallAndReplacesItOnceThatStepIsDone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		due     func(h *harness)
		missing string
	}{
		{"the firewall is gone", (*harness).deleteFirewall, "no firewall carries this subscription's labels"},
		{"the ingress load balancer is gone", (*harness).deleteIngressLoadBalancer, "no ingress load balancer in fsn1 carries this subscription's labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := withCluster(t)
			old := h.nodeIn("fsn1")
			tc.due(h)
			require.Contains(t, h.problems(), tc.missing, "precondition: the plane's problems once %s", tc.name)

			require.NoError(t, h.upgradeCall("fsn1"))
			recorded, standing := h.replacing(), h.problems()
			require.NoError(t, h.replace(c, nil))

			assert.True(t, recorded, "the record of the node in fsn1 once the upgrade's own call ended while %s", tc.name)
			assert.Contains(t, standing, tc.missing, "the upgrade's own call took the step that %s calls for", tc.name)
			assert.NotContains(t, h.problems(), tc.missing, "the plane's problems once no step is left")
			assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 once no step is left")
		})
	}
}

func TestPlane_NamesARecordedReplacementsOwnNextStepWhileALoadBalancerStepIsDue(t *testing.T) {
	h, _ := withCluster(t)
	old, asked := h.nodeIn("fsn1"), h.now
	h.deleteIngressLoadBalancer()

	require.NoError(t, h.upgradeCall("fsn1"))

	assert.Contains(t, h.problems(), fmt.Sprintf("node %d in fsn1 still runs for the replacement the upgrade asked for at %s: its next step is to begin replacing the node in fsn1",
		old.ID, asked.Format(time.RFC3339)), "the plane's problems once the upgrade's own call ended while the ingress load balancer is gone")
}

func TestPlane_RefusesAnUpgradeNamingANodeItCannotReplaceBeforeAnyFirewallLoadBalancerOrGatewayChange(t *testing.T) {
	soldOut := func(h *harness) { h.cloud.SoldOutIn = map[string][]string{"fsn1": h.plane.ServerTypes} }
	for _, due := range []struct {
		name string
		set  func(h *harness)
	}{
		{"the firewall is gone", (*harness).deleteFirewall},
		{"the ingress load balancer is gone", (*harness).deleteIngressLoadBalancer},
		{"a new gateway list is due", func(h *harness) { h.relabelGateways("an-earlier-list") }},
	} {
		for _, refused := range []struct {
			name, loc, class string
			set              func(h *harness)
		}{
			{"names a location the cluster runs no node in", "ash", "node-replacement-unknown", func(*harness) {}},
			{"names a node whose location can order none of its server types", "fsn1", "server-unavailable", soldOut},
		} {
			t.Run("an upgrade that "+refused.name+" while "+due.name, func(t *testing.T) {
				h, c := withCluster(t)
				due.set(h)
				refused.set(h)
				writes, labels := h.cloud.Writes(), h.network().Labels

				err := h.upgradeCall(refused.loc)

				require.Error(t, err, "the upgrade that %s while %s", refused.name, due.name)
				assert.Equal(t, refused.class, refusalClass(t, err), "the refusal's class")
				assert.Equal(t, writes, h.cloud.Writes(), "the writes of the refused upgrade")
				assert.Equal(t, labels, h.network().Labels, "the network's labels after the refused upgrade")
				assert.Empty(t, c.commits, "the commits of the refused upgrade")
			})
		}
	}
}

func TestPlane_AnUpgradeNamingTwoLocationsWhileANewGatewayListIsDueRecordsBothInItsOneWriteAndReplacesThemOneAtATime(t *testing.T) {
	h, c := withCluster(t)
	c.joinsSlowly()
	before, earlier := h.byRole(controlplane.RoleNode), h.gatewayIDs()
	h.relabelGateways("an-earlier-list")
	writes := h.cloud.Writes()
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

	require.NoError(t, h.upgradeCall("fsn1", "nbg1"))
	labels, written := h.network().Labels, h.cloud.Writes()-writes
	require.NoError(t, h.replace(c, watch))

	assert.Equal(t, 1, written, "the writes of the upgrade's own call")
	for _, key := range []string{controlplane.KeyGatewayUpgrade, controlplane.KeyNodeReplace + "fsn1", controlplane.KeyNodeReplace + "nbg1"} {
		assert.Contains(t, labels, key, "the network's labels once the upgrade's own call ended")
	}
	assert.Equal(t, 1, mostDown, "the most locations without a running node that holds an etcd member at once")
	assert.Empty(t, h.standingOf(earlier), "the gateways of the earlier list once no step is left")
	after := h.byRole(controlplane.RoleNode)
	require.Len(t, after, len(controlplane.Locations), "the cluster's nodes")
	for _, node := range after {
		replaced := !slices.ContainsFunc(before, func(b controlplane.Server) bool { return b.ID == node.ID })
		assert.Equal(t, node.Location != "hel1", replaced, "whether the node in %s was replaced", node.Location)
	}
}

func TestPlane_KeepsAnUpgradesRecordThroughAGatewayStepThatFailsPartWayAndCarriesTheGatewaysAndThenTheNodeOn(t *testing.T) {
	h, c := withCluster(t)
	old, earlier := h.nodeIn("fsn1"), h.gatewayIDs()
	h.relabelGateways("an-earlier-list")
	require.NoError(t, h.upgradeCall("fsn1"))
	h.cloud.FailRoute["add"] = errors.New("server error (server_error)")

	err := h.ensure()
	labels := h.network().Labels
	require.NoError(t, h.replace(c, nil))

	require.Error(t, err, "the call after the upgrade's own, whose route add fails")
	assert.Contains(t, err.Error(), "the default route via "+controlplane.GatewayAddress+" is deleted", "the step that failed part way")
	assert.Contains(t, labels, controlplane.KeyNodeReplace+"fsn1", "the network's labels after the failed call")
	assert.Contains(t, labels, controlplane.KeyGatewayUpgrade, "the network's labels after the failed call")
	assert.Empty(t, h.standingOf(earlier), "the gateways of the earlier list once no step is left")
	assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 once no step is left")
}

func TestPlane_ALaterCallRecordsNoNodeAnewAndCarriesTheUpgradesRecordOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(h *harness) error
	}{
		{"a second upgrade naming the node in fsn1 again", func(h *harness) error { return h.upgradeCall("fsn1") }},
		{"a subscribe call naming the node in nbg1", func(h *harness) error {
			h.plane.ReplaceNodes = []string{"nbg1"}
			defer func() { h.plane.ReplaceNodes = nil }()
			return h.ensure()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := withCluster(t)
			old, nbg1 := h.nodeIn("fsn1"), h.nodeIn("nbg1")
			h.relabelGateways("an-earlier-list")
			require.NoError(t, h.upgradeCall("fsn1"))
			recorded := h.network().Labels[controlplane.KeyNodeReplace+"fsn1"]
			h.now = h.now.Add(time.Minute)

			require.NoError(t, tc.call(h))
			labels := h.network().Labels
			require.NoError(t, h.replace(c, nil))

			require.NotEmpty(t, recorded, "the record of the node in fsn1 once the upgrade's own call ended")
			assert.Equal(t, recorded, labels[controlplane.KeyNodeReplace+"fsn1"], "the record of the node in fsn1 after %s", tc.name)
			assert.NotContains(t, labels, controlplane.KeyNodeReplace+"nbg1", "the network's labels after %s", tc.name)
			assert.NotEqual(t, old.ID, h.nodeIn("fsn1").ID, "the node in fsn1 once no step is left")
			assert.Equal(t, nbg1.ID, h.nodeIn("nbg1").ID, "the node in nbg1 once no step is left")
		})
	}
}
