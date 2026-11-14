package pools_test

// A confirmed upgrade replaces a named Cloud worker: the other workers read serving first, its drain moves its pods, it
// is powered off, its Node object goes through the cluster repository, and only then its server is deleted and a new
// one boots from the worker image in its location.

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	robot      = "robot-3041839"
	drainPath  = "cluster/mitstack-drain-fsn1-1.yaml"
	removePath = "cluster/mitstack-remove-node-fsn1-1.yaml"
	budget     = "drain: pod shop/web-0 waits on a disruption budget in namespace shop: The disruption budget web needs 1 healthy pods and has 1 currently"
)

// pool2 is the pool of the site withWorkers builds, a Cloud worker in each of fsn1 and nbg1.
var pool2 = cloudPool(2, "fsn1", "nbg1")

// workers stands in for what the pool reaches only through its block: the cluster repository with the Argo CD that
// syncs it, and what each node report reads of the Node objects, the pods on them and the Jobs Argo CD applied.
type workers struct {
	s       *site
	argo    *testutil.Cluster
	files   map[string][]byte
	commits []string
	events  []string
	// objects names each Node object, down those that read not Ready, and pods each app pod's node.
	objects, down map[string]bool
	pods          map[string]string
	// held names the pods a disruption budget allows no eviction of, attempts how often a drain Job runs before it
	// fails, failed each Job's failed attempts, jobs each finished Job's health and failure each removal's message.
	held     map[string]bool
	attempts int
	failed   map[string]int
	jobs     map[string]string
	failure  string
	paused   bool
	// removedWhileRunning reports whether a Node object went while its server ran, which a kubelet would undo.
	removedWhileRunning bool
}

// withWorkers is a site with a Cloud worker in each of fsn1 and nbg1, each running an app pod, and a Robot worker.
func withWorkers(t *testing.T) (*site, *workers) {
	t.Helper()
	s := newSite(t)
	s.reach(pool2)
	w := &workers{s: s, argo: testutil.BuildCluster(t), files: map[string][]byte{}, objects: map[string]bool{robot: true},
		down: map[string]bool{}, pods: map[string]string{}, held: map[string]bool{}, attempts: 6, failed: map[string]int{}, jobs: map[string]string{}}
	w.argo.Commit(map[string][]byte{"cluster/namespace.yaml": []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: mitstack-node\n")})
	w.argo.Sync()
	w.pods["shop/web-0"], w.pods["shop/web-1"] = w.worker("fsn1-1").Name, w.worker("nbg1-1").Name
	w.attach(&s.plane)
	return s, w
}

// attach gives plane what a block passes it on each call.
func (w *workers) attach(plane *controlplane.Plane) {
	plane.Removals = &controlplane.Removals{Held: w.Held, Commit: w.Commit}
	plane.ReadReport = w.read
	plane.Notify = func(_ context.Context, detail string) { w.events = append(w.events, detail) }
}

// Held lists the removals main holds as a block does, by both kinds of removal path.
func (w *workers) Held(context.Context) ([]string, error) {
	return slices.DeleteFunc(w.argo.Files(), func(path string) bool { return !pools.IsRemovalPath(path) && !etcd.IsRemovalPath(path) }), nil
}

func (w *workers) Commit(_ context.Context, path string, content []byte) error {
	verb := "delete "
	if content != nil {
		verb = "write "
		w.files[path] = content
	}
	w.argo.Commit(map[string][]byte{path: content})
	w.commits = append(w.commits, verb+path)
	return nil
}

// read is the node report a node would send now, once Argo CD has synced and each Job it applied has run.
func (w *workers) read(context.Context, time.Time) (*report.Read, error) {
	for _, srv := range w.s.members() {
		if srv.Status == controlplane.ServerRunning {
			w.objects[srv.Name] = true
		}
	}
	if !w.paused {
		w.argo.Sync()
	}
	for path, content := range w.files {
		job := testutil.Kind(testutil.Documents(w.s.t, content), "Job")[0]
		name := testutil.Field(w.s.t, job, "metadata", "name").(string)
		if w.argo.Object(testutil.ObjectRef{Group: "batch", Kind: "Job", Namespace: "mitstack-node", Name: name}) == nil {
			delete(w.files, path)
			delete(w.jobs, name)
			delete(w.failed, name)
		} else if w.jobs[name] == "" {
			env := testutil.ContainerEnv(w.s.t, testutil.Field(w.s.t, job, "spec", "template", "spec", "containers", "0"))
			w.run(name, env["STEP"], env["NODE"])
		}
	}
	return &report.Read{Body: w.body(), At: w.s.tick()}, nil
}

func (w *workers) run(job, step, node string) {
	if step == pools.RemoveNodeStep && w.failure != "" {
		w.failed[job]++
		w.jobs[job] = "Degraded"
		return
	}
	if step == pools.RemoveNodeStep {
		for _, srv := range w.s.members() {
			w.removedWhileRunning = w.removedWhileRunning || srv.Name == node && srv.Status == controlplane.ServerRunning
		}
		delete(w.objects, node)
		w.jobs[job] = "Healthy"
		return
	}
	for pod, on := range w.pods {
		if on == node && w.held[pod] {
			if w.failed[job]++; w.failed[job] >= w.attempts {
				w.jobs[job] = "Degraded"
			}
			return
		}
	}
	for pod, on := range w.pods {
		if on == node {
			w.pods[pod] = w.readyWorker(node)
		}
	}
	w.jobs[job] = "Healthy"
}

func (w *workers) ready(node string) bool {
	if w.down[node] {
		return false
	}
	return node == robot || slices.ContainsFunc(w.s.members(), func(srv controlplane.Server) bool {
		return srv.Name == node && srv.Status == controlplane.ServerRunning
	})
}

func (w *workers) readyWorker(but string) string {
	for _, node := range slices.Sorted(maps.Keys(w.objects)) {
		if node != but && w.ready(node) {
			return node
		}
	}
	require.Fail(w.s.t, "no Ready worker to move a pod to")
	return ""
}

func (w *workers) body() []byte {
	nodes := []map[string]any{{"name": "node", "ready": true}}
	for _, node := range slices.Sorted(maps.Keys(w.objects)) {
		nodes = append(nodes, map[string]any{"name": node, "ready": w.ready(node)})
	}
	var pods []map[string]any
	for _, pod := range slices.Sorted(maps.Keys(w.pods)) {
		ns, name, _ := strings.Cut(pod, "/")
		pods = append(pods, map[string]any{"namespace": ns, "name": name, "node": w.pods[pod], "phase": "Running", "containers": []any{}})
	}
	for _, job := range slices.Sorted(maps.Keys(w.failed)) {
		pods = append(pods, map[string]any{"namespace": "mitstack-node", "name": job + "-x7k2q", "node": "node", "phase": "Failed",
			"containers": []any{map[string]any{"name": "worker", "state": "terminated", "reason": "Error", "message": cmp.Or(w.failure, budget)}}})
	}
	var body map[string]any
	require.NoError(w.s.t, json.Unmarshal(w.argo.Report(), &body))
	for _, app := range testutil.Field(w.s.t, body, "argocd", "applications").([]any) {
		for _, r := range app.(map[string]any)["resources"].([]any) {
			if r := r.(map[string]any); r["kind"] == "Job" {
				r["health"] = cmp.Or(w.jobs[r["name"].(string)], "Progressing")
			} else {
				r["health"] = nil
			}
		}
	}
	maps.Copy(body, map[string]any{"nodes": nodes, "pods": pods, "openbao": map[string]any{"members": []any{}}})
	data, err := json.Marshal(body)
	require.NoError(w.s.t, err)
	return data
}

func (w *workers) worker(key string) controlplane.Server {
	w.s.t.Helper()
	for _, srv := range w.s.members() {
		if srv.Labels[pools.KeyMember] == key {
			return srv
		}
	}
	require.Failf(w.s.t, "no such worker", "the pool holds no member %s", key)
	return controlplane.Server{}
}

func (w *workers) network() controlplane.Network {
	return w.s.cloud.OnlyNetwork(w.s.t, w.s.plane.RoleSelector("network"))
}

// label sets the network's label key to value, and takes it off for an empty value.
func (w *workers) label(key, value string) {
	network := w.network()
	labels := maps.Clone(network.Labels)
	if labels[key] = value; value == "" {
		delete(labels, key)
	}
	require.NoError(w.s.t, w.s.cloud.SetNetworkLabels(context.Background(), network.ID, labels))
}

func (w *workers) recorded(key string) bool {
	_, ok := w.network().Labels[pools.KeyWorkerReplace+key]
	return ok
}

func (w *workers) replacing() bool {
	return slices.ContainsFunc(slices.Collect(maps.Keys(w.network().Labels)), func(key string) bool {
		return strings.HasPrefix(key, pools.KeyWorkerReplace)
	})
}

// upgrade is the upgrade's own call, which records the workers it names in the network's one write.
func (w *workers) upgrade(pool pools.CloudPool, keys ...string) error {
	ctx := context.Background()
	st, err := w.s.plane.Read(ctx)
	require.NoError(w.s.t, err)
	network := w.network()
	labels := maps.Clone(network.Labels)
	replaces, err := pools.ReplaceWorkers(pool, keys)(ctx, w.s.plane, st, labels)
	if err != nil || replaces == "" {
		return err
	}
	return w.s.cloud.SetNetworkLabels(ctx, network.ID, labels)
}

// replace carries the recorded replacements on as a block's calls do, one step a call, and stops at a call that fails,
// once until reports true, or once the network records none.
func (w *workers) replace(pool pools.CloudPool, until func() bool) error {
	w.s.t.Helper()
	for calls := 0; w.replacing(); calls++ {
		require.Less(w.s.t, calls, 200, "the replacement did not finish within 200 calls")
		if until != nil && until() {
			return nil
		}
		step, ok, err := pools.ReplaceStep(context.Background(), w.s.plane, pool)
		if err == nil && ok {
			err = step.Run(context.Background())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *workers) eventsNaming(part string) []string {
	return slices.DeleteFunc(slices.Clone(w.events), func(event string) bool { return !strings.Contains(event, part) })
}

func (w *workers) problems() string {
	return strings.Join(pools.ReplacementProblems(context.Background(), w.s.plane, pool2), "; ")
}

func TestReplaceStep_AConfirmedUpgradeDrainsTheNamedWorkerRemovesItsNodeObjectAndBootsItsSuccessorFromTheWorkerImageInItsLocation(t *testing.T) {
	s, w := withWorkers(t)
	old, other, network := w.worker("fsn1-1"), w.worker("nbg1-1"), w.network()
	var off, objectWentFirst bool
	var podsWhenOff []string
	watch := func() bool {
		srv, stands := s.cloud.Servers[old.ID]
		if stands && srv.Status == controlplane.ServerOff && !off {
			off = true
			for pod, node := range w.pods {
				if node == old.Name {
					podsWhenOff = append(podsWhenOff, pod)
				}
			}
		}
		if stands {
			objectWentFirst = !w.objects[old.Name]
		}
		return false
	}
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	require.NoError(t, w.replace(pool2, watch))

	successor := w.worker("fsn1-1")
	assert.NotEqual(t, old.ID, successor.ID, "the worker in fsn1 after its replacement")
	assert.Equal(t, old.Name, successor.Name, "the successor's name")
	assert.Equal(t, "fsn1", successor.Location, "the successor's location")
	assert.Equal(t, s.image.ID, successor.ImageID, "the image the successor booted from")
	assert.Empty(t, s.cloud.UserData[successor.ID], "the worker image embeds the node's configuration")
	assert.NotEmpty(t, successor.PrivateIPs[network.ID], "the successor's address on the cluster's network %d", network.ID)
	assert.Equal(t, network.ID, w.network().ID, "the cluster's network after the replacement")
	assert.Equal(t, other.ID, w.worker("nbg1-1").ID, "the worker the upgrade did not name")
	assert.True(t, off, "the old server was powered off before it was deleted")
	assert.Empty(t, podsWhenOff, "the pods on the old worker when it was powered off")
	assert.True(t, objectWentFirst, "the old server stood until its Node object was gone")
	assert.False(t, w.removedWhileRunning, "a Node object went while its server ran")
	assert.Equal(t, []string{"write " + drainPath, "delete " + drainPath, "write " + removePath, "delete " + removePath}, w.commits,
		"the replacement's commits to the cluster repository")
	assert.False(t, w.replacing(), "the network still records a replacement: %v", w.network().Labels)
	for _, event := range []string{"began replacing", "powered off", "deleted Cloud worker", "took the place of"} {
		assert.Len(t, w.eventsNaming(event), 1, "the events naming %q: %v", event, w.events)
	}
}

func TestReplaceStep_RefusesToReplaceTheOnlyReadyWorkerThroughTheRedundancyGuardNamingIt(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	w.down[w.worker("nbg1-1").Name], w.down[robot] = true, true
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	err := w.replace(pool2, nil)

	require.Error(t, err, "the replacement of the only Ready worker")
	assert.Equal(t, redundant.ClassBelowMinimum, refusalClass(t, err), "the refusal's class")
	assert.Contains(t, err.Error(), `removing "`+old.Name+`" from the workers`, "the worker the refusal names")
	assert.Equal(t, controlplane.ServerRunning, s.cloud.Servers[old.ID].Status, "the worker the refused replacement named")
	assert.Empty(t, w.commits, "the commits of a refused replacement")
	assert.False(t, w.replacing(), "the network still records the refused replacement: %v", w.network().Labels)
}

func TestReplaceStep_ADrainADisruptionBudgetHoldsWaitsNamingTheBudgetWithTheServerKeptUntilTheBudgetAllowsIt(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	w.held["shop/web-0"] = true
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	require.NoError(t, w.replace(pool2, func() bool { return w.failed["mitstack-drain-fsn1-1"] >= 3 }))

	assert.Contains(t, w.problems(), budget, "where the replacement stands")
	assert.Contains(t, w.problems(), "its server is kept", "where the replacement stands")
	assert.Equal(t, controlplane.ServerRunning, s.cloud.Servers[old.ID].Status, "the worker whose drain waits")
	assert.Equal(t, []string{"write " + drainPath}, w.commits, "the commits while the drain waits")

	delete(w.held, "shop/web-0")
	require.NoError(t, w.replace(pool2, nil), "the replacement once the budget allows the eviction")
	assert.NotEqual(t, old.ID, w.worker("fsn1-1").ID, "the worker in fsn1 once the budget allowed its drain")
}

func TestReplaceStep_WithdrawsTheReplacementAndKeepsTheWorkerOnceItsDrainFailedEveryAttempt(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	w.held["shop/web-0"], w.attempts = true, 2
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	err := w.replace(pool2, nil)

	assert.Equal(t, "worker-drain-failed", refusalClass(t, err), "the refusal's class")
	assert.Contains(t, err.Error(), budget, "the refusal names the budget")
	assert.Equal(t, controlplane.ServerRunning, s.cloud.Servers[old.ID].Status, "the worker whose drain failed")
	assert.Equal(t, []string{"write " + drainPath, "delete " + drainPath}, w.commits, "the commits of a failed drain")
	assert.False(t, w.replacing(), "the network still records the failed replacement: %v", w.network().Labels)
}

func TestReplaceStep_AReplacementStoppedOnceItsWorkerIsOffResumesAtItsNodeObjectsRemovalAfterARestart(t *testing.T) {
	s, w := withWorkers(t)
	old, created := w.worker("fsn1-1"), len(s.cloud.ServerSpecs)
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))
	require.NoError(t, w.replace(pool2, func() bool { return s.cloud.Servers[old.ID].Status == controlplane.ServerOff }))
	require.Equal(t, []string{"write " + drainPath}, w.commits, "precondition: the replacement stopped before its Node object's removal")
	s.plane = s.newPlane()
	w.attach(&s.plane)

	require.NoError(t, w.replace(pool2, nil))

	assert.NotEqual(t, old.ID, w.worker("fsn1-1").ID, "the worker in fsn1 once the resumed replacement ended")
	assert.Len(t, w.eventsNaming("began replacing"), 1, "the resumed replacement began again: %v", w.events)
	assert.Len(t, w.eventsNaming("powered off"), 1, "the resumed replacement powered a worker off again: %v", w.events)
	assert.Equal(t, []string{"write " + drainPath, "delete " + drainPath, "write " + removePath, "delete " + removePath}, w.commits,
		"the commits of the resumed replacement")
	assert.Len(t, s.cloud.ServerSpecs[created:], 1, "the servers created while the replacement ran")
}

func TestReplaceStep_ASuccessorTakesThePoolsNextTypeWhenItsLocationCannotOrderTheFirstAndThePoolPlansNoneMeanwhile(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	s.cloud.SoldOutIn = map[string][]string{"fsn1": {"cx33"}}
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))
	require.NoError(t, w.replace(pool2, func() bool { _, stands := s.cloud.Servers[old.ID]; return !stands }))
	plan, err := s.plan(pool2)
	require.NoError(t, err)
	assert.True(t, plan.Empty(), "the pool's plan while its worker in fsn1 is deleted and not yet replaced: %+v", plan)

	require.NoError(t, w.replace(pool2, nil))

	assert.Equal(t, "cx43", w.worker("fsn1-1").ServerType, "the successor's type")
	assert.Len(t, w.eventsNaming("as cx43, since Hetzner can order no cx33 there now"), 1, "the events: %v", w.events)
}

func TestReplaceStep_AnUpgradeThatNamesNoWorkerDrainsAndReplacesNone(t *testing.T) {
	s, w := withWorkers(t)
	servers, writes := ids(s.members()), s.cloud.Writes()

	require.NoError(t, w.upgrade(pool2))
	step, ok, err := pools.ReplaceStep(context.Background(), s.plane, pool2)

	require.NoError(t, err)
	assert.False(t, ok, "the step of an upgrade naming no worker: %s", step.What)
	assert.Equal(t, writes, s.cloud.Writes(), "Hetzner writes")
	assert.Empty(t, w.commits, "the commits")
	assert.Equal(t, servers, ids(s.members()), "the pool's servers")
}

func TestReplaceWorkers_RefusesAnUpgradeItCannotCarryOutBeforeRecordingAnything(t *testing.T) {
	for _, tc := range []struct {
		name, key, class string
		set              func(t *testing.T, s *site)
	}{
		{"a key the pool holds no member of", "hel1-1", "worker-replacement-unknown", func(*testing.T, *site) {}},
		{"no cluster repository", "fsn1-1", "worker-removal-unwritable", func(_ *testing.T, s *site) { s.plane.Removals = nil }},
		{"no redundancy guard", "fsn1-1", "worker-replacement-unjudged", func(_ *testing.T, s *site) { s.plane.Guard = nil }},
		{"no worker image", "fsn1-1", "worker-image-missing", func(t *testing.T, s *site) {
			require.NoError(t, s.cloud.DeleteSnapshot(context.Background(), s.image.ID))
		}},
		{"a location ordering none of the pool's types", "fsn1-1", "no-server-type-available", func(_ *testing.T, s *site) {
			s.cloud.SoldOutIn = map[string][]string{"fsn1": {"cx33", "cx43"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w := withWorkers(t)
			tc.set(t, s)
			before := w.network().Labels

			err := w.upgrade(pool2, tc.key)

			assert.Equal(t, tc.class, refusalClass(t, err), "the refusal's class")
			assert.Equal(t, before, w.network().Labels, "the network's labels after a refused upgrade")
		})
	}
}

func TestReplaceStep_RefusesToCarryAReplacementOnWhenALaterCallPassesNoRepository(t *testing.T) {
	s, w := withWorkers(t)
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))
	s.plane.Removals = nil

	_, _, err := pools.ReplaceStep(context.Background(), s.plane, pool2)

	assert.Equal(t, "worker-removal-unwritable", refusalClass(t, err), "the refusal's class")
	assert.True(t, w.recorded("fsn1-1"), "the record a later call that passes a repository carries on")
}

func TestReplaceStep_ReplacesTheWorkersAnUpgradeNamesOneAtATime(t *testing.T) {
	s, w := withWorkers(t)
	first, second := w.worker("fsn1-1"), w.worker("nbg1-1")
	var overlapped bool
	watch := func() bool {
		srv, stands := s.cloud.Servers[second.ID]
		overlapped = overlapped || stands && srv.Labels[controlplane.KeyRemovalBegan] != "" && w.recorded("fsn1-1")
		return false
	}
	require.NoError(t, w.upgrade(pool2, "fsn1-1", "nbg1-1"))
	assert.Contains(t, w.problems(), "the Cloud worker nbg1-1 waits", "where the second replacement stands")

	require.NoError(t, w.replace(pool2, watch))

	assert.False(t, overlapped, "the second replacement began before the first ended")
	assert.NotEqual(t, first.ID, w.worker("fsn1-1").ID, "the worker in fsn1")
	assert.NotEqual(t, second.ID, w.worker("nbg1-1").ID, "the worker in nbg1")
}

func TestReplaceStep_BeginsOnlyOnceTheUpgradeHasReplacedTheGatewaysAndTheControlPlaneNodesItNames(t *testing.T) {
	for _, key := range []string{controlplane.KeyGatewayUpgrade, controlplane.KeyNodeReplace + "nbg1"} {
		s, w := withWorkers(t)
		old := w.worker("fsn1-1")
		require.NoError(t, w.upgrade(pool2, "fsn1-1"))
		w.label(key, "5-1794605517")

		step, ok, err := pools.ReplaceStep(context.Background(), s.plane, pool2)

		require.NoError(t, err)
		assert.False(t, ok, "the step while the network carries %s: %s", key, step.What)
		assert.Contains(t, w.problems(), "begins once the upgrade has replaced", "where the replacement stands beside %s", key)
		w.label(key, "")
		require.NoError(t, w.replace(pool2, nil), "the replacement once %s is gone", key)
		assert.NotEqual(t, old.ID, w.worker("fsn1-1").ID, "the worker in fsn1 once %s is gone", key)
	}
}

func TestReplaceStep_TakesItsHeldDrainOutBeforeItCreatesASuccessorForAWorkerWhoseServerWentMeanwhile(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	w.held["shop/web-0"] = true
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))
	require.NoError(t, w.replace(pool2, func() bool { return len(w.commits) > 0 }))
	require.NoError(t, s.cloud.DeleteServer(context.Background(), old.ID))
	var createdWhileHeld bool
	watch := func() bool {
		createdWhileHeld = createdWhileHeld || len(s.members()) == 2 && slices.ContainsFunc(w.argo.Files(), pools.IsRemovalPath)
		return false
	}

	require.NoError(t, w.replace(pool2, watch))

	assert.Equal(t, []string{"write " + drainPath, "delete " + drainPath, "write " + removePath, "delete " + removePath}, w.commits,
		"the commits, whose removal Job releases the claims on the gone worker's local volumes")
	assert.False(t, createdWhileHeld, "the successor was created while the drain was held")
	assert.NotEqual(t, old.ID, w.worker("fsn1-1").ID, "the worker in fsn1")
}

func TestReplaceStep_PowersOffAWorkerWhoseNodeReadsNotReadyWithNoDrain(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	w.down[old.Name] = true
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	require.NoError(t, w.replace(pool2, func() bool {
		if _, stands := s.cloud.Servers[old.ID]; !stands {
			delete(w.down, old.Name)
		}
		return false
	}))

	assert.Equal(t, []string{"write " + removePath, "delete " + removePath}, w.commits, "the commits")
	assert.Len(t, w.eventsNaming("with no drain"), 1, "the events: %v", w.events)
	assert.NotEqual(t, old.ID, w.worker("fsn1-1").ID, "the worker in fsn1")
}

func TestReplaceStep_ReleasesTheLocalClaimsOfAnOffWorkerTheReportNoLongerListsBeforeItsServerGoes(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	require.NoError(t, s.cloud.PowerOffServer(context.Background(), old.ID))
	delete(w.objects, old.Name)
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	require.NoError(t, w.replace(pool2, nil))

	assert.Equal(t, []string{"write " + removePath, "delete " + removePath}, w.commits, "the commits")
	wrote := slices.IndexFunc(w.events, func(event string) bool { return strings.HasPrefix(event, "wrote "+removePath) })
	deleted := slices.IndexFunc(w.events, func(event string) bool { return strings.HasPrefix(event, "deleted Cloud worker") })
	assert.True(t, wrote >= 0 && wrote < deleted, "the removal Job was written before the server went: %v", w.events)
	assert.NotEqual(t, old.ID, w.worker("fsn1-1").ID, "the worker in fsn1")
}

func TestReplaceStep_WritesAFailedNodeRemovalAgainNamingWhyWithTheWorkerKept(t *testing.T) {
	s, w := withWorkers(t)
	old := w.worker("fsn1-1")
	w.failure = "remove-node: the API refused deleting the claim shop/data-db-0"
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	require.NoError(t, w.replace(pool2, func() bool { return len(w.commits) == 5 }))

	assert.Equal(t, []string{"write " + drainPath, "delete " + drainPath, "write " + removePath, "delete " + removePath, "write " + removePath}, w.commits,
		"the commits of a removal that failed once")
	assert.Len(t, w.eventsNaming(w.failure), 1, "the events naming the removal's failure: %v", w.events)
	assert.Equal(t, controlplane.ServerOff, s.cloud.Servers[old.ID].Status, "the worker whose Node object's removal failed")
}

func TestReplaceStep_NamesADrainJobArgoCDHasNotAppliedYet(t *testing.T) {
	_, w := withWorkers(t)
	w.paused = true
	require.NoError(t, w.upgrade(pool2, "fsn1-1"))

	require.NoError(t, w.replace(pool2, func() bool { return len(w.commits) == 1 }))

	assert.Contains(t, w.problems(), "lists no Job mitstack-drain-fsn1-1, so the Application cluster has not applied its file", "where the replacement stands")
	assert.NotContains(t, w.problems(), "has not finished", "where the replacement stands")
}
