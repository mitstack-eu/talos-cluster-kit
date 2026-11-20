package rollout_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const (
	newRevision = "openbao-7d9f8c6b5"
	oldRevision = "openbao-5c4b3a291"
)

// openBaoFile is the file a release commits for OpenBao's component.
func openBaoFile(t *testing.T) []byte {
	t.Helper()
	openBao := testutil.Manifest(t, "openbao", components.HostingServed().Served)
	files, err := rollout.Release{ToolsImage: testutil.ToolsImage, RepoURL: address, Components: []components.Manifest{openBao}}.Files(repository)
	require.NoError(t, err)
	require.Equal(t, "components/openbao/manifest.yaml", files[0].Path)
	return files[0].Bytes
}

// rolloutJob is the hook of OpenBao's file that runs after the sync.
func rolloutJob(t *testing.T) map[string]any {
	t.Helper()
	var after []map[string]any
	for _, job := range testutil.Kind(testutil.Documents(t, openBaoFile(t)), "Job") {
		if testutil.Field(t, job, "metadata", "annotations", "argocd.argoproj.io/hook") == "PostSync" {
			after = append(after, job)
		}
	}
	require.Len(t, after, 1, "the Jobs OpenBao's file runs after its sync")
	return after[0]
}

func TestRelease_RunsOpenBaosRolloutJobAfterEachSyncOfItsComponentUnderItsOwnAccountInItsNamespace(t *testing.T) {
	file := openBaoFile(t)
	job := rolloutJob(t)
	pod := testutil.Field(t, job, "spec", "template", "spec")
	c := testutil.Field(t, pod, "containers", "0")
	security := testutil.Field(t, c, "securityContext")

	assert.Len(t, testutil.Kind(testutil.Documents(t, file), "Job"), 2, "the handover Job and the rollout Job")
	assert.Equal(t, "mitstack-openbao-rollout", testutil.Field(t, job, "metadata", "name"))
	assert.Equal(t, "openbao", testutil.Field(t, job, "metadata", "namespace"))
	assert.Equal(t, "BeforeHookCreation,HookSucceeded", testutil.Field(t, job, "metadata", "annotations", "argocd.argoproj.io/hook-delete-policy"),
		"each sync creates a fresh Job, and one that failed stays to be read until the next attempt replaces it")
	assert.Nil(t, testutil.Field(t, job, "metadata", "annotations", "argocd.argoproj.io/sync-wave"), "a PostSync hook runs after every wave")
	assert.Equal(t, "talos-cluster-kit", testutil.Field(t, job, "metadata", "labels", "mitstack.dev/rendered-by"))
	assert.Equal(t, 0, testutil.Field(t, job, "spec", "backoffLimit"), "Argo CD's own retry runs a failed rollout again")
	assert.Equal(t, 1860, testutil.Field(t, job, "spec", "activeDeadlineSeconds"),
		"past the 300 seconds each of the three members may take to return, the 30 seconds each of the six bounded calls of its pass may take, "+
			"the four calls of each of up to three passes that evict nothing, and 30 seconds each for the StatefulSet's read and the members' last read")
	assert.Equal(t, map[string]any{"app.kubernetes.io/name": "mitstack-openbao-rollout", "mitstack.dev/app": "openbao"},
		testutil.Field(t, job, "spec", "template", "metadata", "labels"), "the label the hosting flows select the Job by, and the app label the admission policy asks for")
	assert.Equal(t, "mitstack-openbao-rollout", testutil.Field(t, pod, "serviceAccountName"))
	assert.Equal(t, "openbao", testutil.Field(t, stated(t, file, testutil.ObjectRef{Kind: "ServiceAccount", Name: "mitstack-openbao-rollout"}), "metadata", "namespace"),
		"the component states the account the Job runs under, so the sync creates it before the Job")
	assert.Equal(t, "Never", testutil.Field(t, pod, "restartPolicy"))
	assert.Equal(t, testutil.ToolsImage, testutil.Field(t, c, "image"))
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/openbao-rollout.sh"}, testutil.Field(t, c, "command"), "the Job runs the script the tools image carries")
	assert.Equal(t, "FallbackToLogsOnError", testutil.Field(t, c, "terminationMessagePolicy"))
	assert.Equal(t, []any{
		map[string]any{"name": "ROLLOUT_NAMESPACE", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.namespace"}}},
		map[string]any{"name": "ROLLOUT_STATEFULSET", "value": "openbao"},
		map[string]any{"name": "ROLLOUT_DISRUPTION_BUDGET", "value": "openbao"},
		map[string]any{"name": "ROLLOUT_MEMBER_TIMEOUT", "value": "300s"},
		map[string]any{"name": "ROLLOUT_CALL_TIMEOUT", "value": "30s"},
		map[string]any{"name": "HOME", "value": "/tmp"},
	}, testutil.Field(t, c, "env"))
	assert.Equal(t, "openbao", testutil.Field(t, stated(t, file, testutil.ObjectRef{Kind: "StatefulSet", Name: "openbao"}), "metadata", "namespace"),
		"the StatefulSet the Job moves sits in the Job's own namespace")
	assert.Equal(t, "openbao", testutil.Field(t, stated(t, file, testutil.ObjectRef{Kind: "PodDisruptionBudget", Name: "openbao"}), "metadata", "namespace"),
		"the disruption budget the Job waits on sits in the Job's own namespace")
	assert.Equal(t, true, testutil.Field(t, pod, "securityContext", "runAsNonRoot"))
	assert.Equal(t, true, testutil.Field(t, security, "readOnlyRootFilesystem"))
	assert.Equal(t, false, testutil.Field(t, security, "allowPrivilegeEscalation"))
	assert.Equal(t, []any{"ALL"}, testutil.Field(t, security, "capabilities", "drop"))
}

func TestRelease_RunsTheRolloutJobForOpenBaosComponentAlone(t *testing.T) {
	var others []components.Manifest
	for _, m := range components.HostingServed().Served {
		if m.Name != "openbao" {
			others = append(others, m)
		}
	}
	files, err := rollout.Release{ToolsImage: testutil.ToolsImage, RepoURL: address, Components: others}.Files(repository)
	require.NoError(t, err)

	for _, f := range files {
		for _, job := range testutil.Kind(testutil.Documents(t, f.Bytes), "Job") {
			assert.NotEqual(t, "PostSync", testutil.Field(t, job, "metadata", "annotations", "argocd.argoproj.io/hook"), "a Job of %s", f.Path)
		}
	}
}

// member is one OpenBao pod as the API server serves it, with the labels OpenBao's Kubernetes registration writes.
func member(name, revision string, active, ready, sealed bool) map[string]any {
	readiness := "False"
	if ready {
		readiness = "True"
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "openbao", "labels": map[string]any{
			"app.kubernetes.io/name": "openbao", "app.kubernetes.io/instance": "openbao", "component": "server",
			"controller-revision-hash": revision, "openbao-active": fmt.Sprint(active), "openbao-sealed": fmt.Sprint(sealed),
		}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": readiness}}},
	}
}

// standby and leader are a member that is Ready and unsealed at revision.
func standby(name, revision string) map[string]any { return member(name, revision, false, true, false) }
func leader(name, revision string) map[string]any  { return member(name, revision, true, true, false) }

// raftIndexes is sys/leader as a member answers bao's read of it, with the indexes the rollout compares.
func raftIndexes(committed, applied int) string {
	return fmt.Sprintf(`{"data": {"ha_enabled": true, "raft_committed_index": %d, "raft_applied_index": %d}}`, committed, applied)
}

const fakeRolloutKubectl = `case "$1 $2" in
"get statefulset") cat "$FAKE_DIR/statefulset.json" ;;
"get pods")
  case "$*" in
  *--watch*)
    name="$*"
    name=${name##*metadata.name=}
    name=${name%% *}
    if [ -f "$FAKE_DIR/watch-$name" ]; then cat "$FAKE_DIR/watch-$name"; else printf '%s True false\n' "$FAKE_REVISION"; fi
    [ "${FAKE_WATCH_THEN:-0}" != hold ] || exec tail -f /dev/null
    exit "${FAKE_WATCH_THEN:-0}"
    ;;
  *)
    n=$(($(cat "$FAKE_DIR/reads" 2> /dev/null || echo 0) + 1))
    echo "$n" > "$FAKE_DIR/reads"
    [ ! -f "$FAKE_DIR/read-$n.json" ] || cp "$FAKE_DIR/read-$n.json" "$FAKE_DIR/pods.json"
    cat "$FAKE_DIR/pods.json"
    ;;
  esac
  ;;
"wait "*)
  case ${FAKE_BUDGET:-allows} in
  hold) exec tail -f /dev/null ;;
  refuse) echo 'Error from server (Forbidden): poddisruptionbudgets.policy "openbao" is forbidden' >&2; exit 1 ;;
  esac
  echo poddisruptionbudget.policy/openbao condition met
  ;;
"create --raw")
  [ "${FAKE_EVICT_EXIT:-0}" = 0 ] || exit "$FAKE_EVICT_EXIT"
  name=${3%/eviction}
  name=${name##*/}
  if [ -f "$FAKE_DIR/after-$name.json" ]; then
    cp "$FAKE_DIR/after-$name.json" "$FAKE_DIR/pods.json"
  else
    jq --arg name "$name" --arg revision "$FAKE_REVISION" \
      '(.items[] | select(.metadata.name == $name) | .metadata.labels["controller-revision-hash"]) = $revision' \
      "$FAKE_DIR/pods.json" > "$FAKE_DIR/pods.next"
    mv "$FAKE_DIR/pods.next" "$FAKE_DIR/pods.json"
  fi
  ;;
"exec -n") cat "$FAKE_DIR/sys-leader-$4.json" ;;
*) echo "kubectl $* is no call the rollout makes" >&2; exit 64 ;;
esac`

type rolloutRun struct {
	calls []string
	out   string
	err   error
}

// evicted names each member the run asked the API server to evict, in order.
func (r rolloutRun) evicted() []string {
	var names []string
	for _, call := range r.calls {
		if m := regexp.MustCompile(`^kubectl create --raw /api/v1/namespaces/openbao/pods/(\S+)/eviction `).FindStringSubmatch(call); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// rolloutCluster is what the fake kubectl serves: the members, sys/leader as each answers it, what the watch of a
// member prints, which by default is the member at the update revision, Ready and unsealed, and the members as they
// read once a member's eviction has run, which by default are the members before it with that one at the update revision,
// and the members as they read from the nth read of them on.
type rolloutCluster struct {
	members  []map[string]any
	after    map[string][]map[string]any
	reads    map[int][]map[string]any
	answers  map[string]string
	watches  map[string]string
	replicas int
}

// runRollout runs the script OpenBao's rollout Job names, with the Job's environment and each of env over it, against c.
func runRollout(t *testing.T, c rolloutCluster, env ...string) rolloutRun {
	t.Helper()
	job := rolloutJob(t)
	script, err := os.ReadFile(filepath.Join("scripts", "openbao-rollout.sh"))
	require.NoError(t, err)
	fakes := testutil.NewTools(t, map[string]string{"kubectl": fakeRolloutKubectl})
	fakes.AddImageJq(t)
	members := stated(t, openBaoFile(t), testutil.ObjectRef{Kind: "StatefulSet", Name: "openbao"})
	if c.replicas != 0 {
		members["spec"].(map[string]any)["replicas"] = c.replicas
	}
	members["status"] = map[string]any{"updateRevision": newRevision, "currentRevision": oldRevision}
	files := map[string]any{"statefulset.json": members, "pods.json": map[string]any{"apiVersion": "v1", "kind": "List", "items": c.members}}
	for name, data := range files {
		bytes, err := json.Marshal(data)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(fakes.Dir, name), bytes, 0o644))
	}
	states := map[string][]map[string]any{}
	for name, members := range c.after {
		states["after-"+name] = members
	}
	for n, members := range c.reads {
		states[fmt.Sprintf("read-%d", n)] = members
	}
	for name, members := range states {
		bytes, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": members})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(fakes.Dir, name+".json"), bytes, 0o644))
	}
	for name, answer := range c.answers {
		require.NoError(t, os.WriteFile(filepath.Join(fakes.Dir, "sys-leader-"+name+".json"), []byte(answer), 0o644))
	}
	for name, printed := range c.watches {
		require.NoError(t, os.WriteFile(filepath.Join(fakes.Dir, "watch-"+name), []byte(printed), 0o644))
	}
	vars := []string{"FAKE_DIR=" + fakes.Dir, "FAKE_REVISION=" + newRevision}
	for _, e := range testutil.Field(t, job, "spec", "template", "spec", "containers", "0", "env").([]any) {
		value := testutil.Field(t, e, "value")
		if value == nil {
			require.Equal(t, "metadata.namespace", testutil.Field(t, e, "valueFrom", "fieldRef", "fieldPath"))
			value = testutil.Field(t, job, "metadata", "namespace")
		}
		if name := testutil.Field(t, e, "name"); name != "HOME" {
			vars = append(vars, fmt.Sprintf("%s=%v", name, value))
		}
	}

	out, err := testutil.RunBash(t, string(script), fakes.Env(append(vars, env...)...))

	return rolloutRun{calls: fakes.Calls(t), out: out, err: err}
}

// caughtUp answers each member's read of sys/leader with the leader's commit applied everywhere.
func caughtUp(names ...string) map[string]string {
	answers := map[string]string{}
	for _, name := range names {
		answers[name] = raftIndexes(100, 100)
	}
	return answers
}

func TestOpenBaoRollout_EvictsNothingWhenEveryMemberRunsTheUpdateRevision(t *testing.T) {
	run := runRollout(t, rolloutCluster{members: []map[string]any{
		standby("openbao-0", newRevision), leader("openbao-1", newRevision), standby("openbao-2", newRevision),
	}})

	require.NoError(t, run.err, run.out)
	assert.Empty(t, run.evicted(), "the members it evicted: %v", run.calls)
	assert.Len(t, run.calls, 2, "a read of the StatefulSet and one of its members, and nothing else: %v", run.calls)
	assert.Contains(t, run.out, "every member runs revision "+newRevision)
}

func TestOpenBaoRollout_EvictsEachOutdatedStandbyOneAtATimeAndThenTheActiveMember(t *testing.T) {
	run := runRollout(t, rolloutCluster{
		members: []map[string]any{standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision)},
		answers: caughtUp("openbao-0", "openbao-1", "openbao-2"),
	})

	require.NoError(t, run.err, run.out)
	assert.Equal(t, []string{"openbao-0", "openbao-2", "openbao-1"}, run.evicted(), "the standbys in name order, then the active member, whatever its ordinal")
	read := func(name string) string {
		return "kubectl exec -n openbao " + name + " -c openbao -- bao read -tls-skip-verify -format=json sys/leader"
	}
	var want []string
	for _, name := range []string{"openbao-0", "openbao-2", "openbao-1"} {
		want = append(want, "members", read("openbao-1"), "budget", "members", "evict "+name, "watch "+name, read(name))
	}
	want = append(want, "members")
	var got []string
	for _, call := range run.calls[1:] {
		switch {
		case call == "kubectl get pods -n openbao -l app.kubernetes.io/instance=openbao,app.kubernetes.io/name=openbao,component=server -o json":
			got = append(got, "members")
		case strings.HasPrefix(call, "kubectl create --raw "):
			assert.Regexp(t, `^kubectl create --raw /api/v1/namespaces/openbao/pods/\S+/eviction -f \S+$`, call, "an eviction through the Eviction API")
			got = append(got, "evict "+regexp.MustCompile(`pods/(\S+)/eviction`).FindStringSubmatch(call)[1])
		case strings.HasPrefix(call, "kubectl wait "):
			assert.Equal(t, "kubectl wait --for=jsonpath={.status.disruptionsAllowed}=1 --timeout=-1s poddisruptionbudget/openbao -n openbao", call,
				"a wait on OpenBao's one budget until it allows a disruption, which timeout alone bounds")
			got = append(got, "budget")
		case strings.Contains(call, " --watch "):
			assert.Regexp(t, `^kubectl get pods -n openbao --field-selector=metadata\.name=\S+ --watch -o jsonpath=`, call, "a watch of one member's pod")
			got = append(got, "watch "+regexp.MustCompile(`metadata\.name=(\S+)`).FindStringSubmatch(call)[1])
		default:
			got = append(got, call)
		}
	}
	assert.Equal(t, want, got, "for each member: a fresh read of the members, the leader's commit, the budget's allowance, another read of the members, "+
		"the eviction, the watch until the member returns, then the member's own index, and a last read of the members that finds none outdated")
	assert.Contains(t, run.out, "openbao-1 runs revision "+newRevision+", unsealed, at Raft index 100")
	for _, name := range []string{"openbao-0", "openbao-2", "openbao-1"} {
		waiting := strings.Index(run.out, "openbao-rollout: waiting up to 30s for OpenBao's disruption budget openbao to allow a disruption before evicting "+name+"\n")
		require.GreaterOrEqual(t, waiting, 0, "the rollout says it waits on the budget before evicting %s", name)
		assert.Less(t, waiting, strings.Index(run.out, "evicted "+name), "it says so before the eviction of %s", name)
	}
}

func TestOpenBaoRollout_ReadsTheMembersAgainBeforeEachEvictionAndTheLeaderOfThatPass(t *testing.T) {
	active0 := []map[string]any{leader("openbao-0", oldRevision), standby("openbao-1", oldRevision), standby("openbao-2", oldRevision)}
	active1 := []map[string]any{standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision)}
	for _, tc := range []struct {
		name    string
		after   []map[string]any
		reads   map[int][]map[string]any
		evicted []string
		leaders []string
		fails   bool
		says    string
	}{
		{"leadership moves to an outdated standby after the first eviction, which then goes last",
			[]map[string]any{standby("openbao-0", newRevision), standby("openbao-1", oldRevision), leader("openbao-2", oldRevision)}, nil,
			[]string{"openbao-0", "openbao-1", "openbao-2"}, []string{"openbao-1", "openbao-2", "openbao-2"}, false, ""},
		{"leadership moves to a member already moved",
			[]map[string]any{leader("openbao-0", newRevision), standby("openbao-1", oldRevision), standby("openbao-2", oldRevision)}, nil,
			[]string{"openbao-0", "openbao-1", "openbao-2"}, []string{"openbao-1", "openbao-0", "openbao-0"}, false, ""},
		{"a member reads sealed between passes",
			[]map[string]any{standby("openbao-0", newRevision), leader("openbao-1", oldRevision), member("openbao-2", oldRevision, false, true, true)}, nil,
			[]string{"openbao-0"}, []string{"openbao-1"}, true, "openbao-2 is not Ready and unsealed"},
		{"a member that never reads the update revision after its return", active1, nil,
			[]string{"openbao-0", "openbao-0", "openbao-0"}, []string{"openbao-1", "openbao-1", "openbao-1"}, true,
			"3 evictions, one for each of the 3 members, left openbao-0 openbao-1 openbao-2 on an older revision than " + newRevision},
		{"the standby a pass picked turns active just before its eviction, so it is not evicted then and goes last", nil,
			map[int][]map[string]any{2: active0},
			[]string{"openbao-1", "openbao-2", "openbao-0"}, []string{"openbao-1", "openbao-0", "openbao-0", "openbao-0"}, false,
			"openbao-0 turned active before its eviction while openbao-1 openbao-2 are outdated standbys, so this pass evicts nothing"},
		{"leadership moves onto the member each pass picked until the passes that evict nothing run out", nil,
			map[int][]map[string]any{2: active0, 4: active1, 6: active0},
			nil, []string{"openbao-1", "openbao-0", "openbao-1"}, true,
			"3 passes, one for each of the 3 members, evicted nothing as leadership moved onto the member each had picked, so the rollout stops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := rolloutCluster{members: active1, answers: caughtUp("openbao-0", "openbao-1", "openbao-2"), reads: tc.reads}
			if tc.after != nil {
				cluster.after = map[string][]map[string]any{"openbao-0": tc.after}
			}

			run := runRollout(t, cluster)

			if tc.fails {
				require.Error(t, run.err, run.out)
			} else {
				require.NoError(t, run.err, run.out)
			}
			assert.Contains(t, run.out, tc.says, "what the rollout printed")
			assert.Equal(t, tc.evicted, run.evicted(), "the members it evicted, in order")
			var leaders []string
			for i, call := range run.calls {
				if strings.HasPrefix(call, "kubectl wait ") {
					leaders = append(leaders, regexp.MustCompile(`^kubectl exec -n openbao (\S+) `).FindStringSubmatch(run.calls[i-1])[1])
				}
			}
			assert.Equal(t, tc.leaders, leaders, "the member each pass read the commit from, the one active in that pass")
		})
	}
}

func TestOpenBaoRollout_EvictsNothingUnlessTheDisruptionBudgetAllowsADisruptionWithinItsBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		says string
	}{
		{"a budget that allows no disruption within the bound", []string{"FAKE_BUDGET=hold", "ROLLOUT_CALL_TIMEOUT=1s"},
			"OpenBao's disruption budget openbao allowed no disruption within 1s, so openbao-0 is not evicted and no member moves"},
		{"a wait the API server refuses", []string{"FAKE_BUDGET=refuse"},
			"kubectl could not wait on OpenBao's disruption budget openbao, so openbao-0 is not evicted and no member moves"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runRollout(t, rolloutCluster{
				members: []map[string]any{standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision)},
				answers: caughtUp("openbao-0", "openbao-1", "openbao-2"),
			}, tc.env...)

			require.Error(t, run.err, run.out)
			assert.Empty(t, run.evicted())
			assert.Contains(t, run.out, tc.says)
		})
	}
}

func TestOpenBaoRollout_StopsAtAMemberThatReturnsBehindTheLeadersCommitAndEvictsNoOther(t *testing.T) {
	answers := caughtUp("openbao-1", "openbao-2")
	answers["openbao-0"] = raftIndexes(90, 90)

	run := runRollout(t, rolloutCluster{
		members: []map[string]any{standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision)},
		answers: answers,
	})

	require.Error(t, run.err, run.out)
	assert.Equal(t, []string{"openbao-0"}, run.evicted())
	assert.Contains(t, run.out, "openbao-0 has applied Raft index 90, short of the 100 openbao-1 committed before its eviction, so no other member moves")
}

func TestOpenBaoRollout_StopsAtAMemberThatDoesNotReturnWithinItsBoundAndEvictsNoOther(t *testing.T) {
	run := runRollout(t, rolloutCluster{
		members: []map[string]any{standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision)},
		answers: caughtUp("openbao-0", "openbao-1", "openbao-2"),
		watches: map[string]string{"openbao-0": oldRevision + " True false\n" + newRevision + "  \n" + newRevision + " False false\n"},
	}, "FAKE_WATCH_THEN=hold", "ROLLOUT_MEMBER_TIMEOUT=1s")

	require.Error(t, run.err, run.out)
	assert.Equal(t, []string{"openbao-0"}, run.evicted())
	assert.Contains(t, run.out, "openbao-0 did not run revision "+newRevision+" Ready and unsealed within 1s of its eviction, so no other member moves")
}

func TestOpenBaoRollout_EvictsNothingWhileTheMembersCannotSpareOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster rolloutCluster
		env     []string
		says    string
	}{
		{"a member that is not Ready", rolloutCluster{members: []map[string]any{
			standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), member("openbao-2", newRevision, false, false, false),
		}}, nil, "openbao-2 is not Ready and unsealed"},
		{"a member that is sealed", rolloutCluster{members: []map[string]any{
			member("openbao-0", oldRevision, false, true, true), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision),
		}}, nil, "openbao-0 is not Ready and unsealed"},
		{"a member missing", rolloutCluster{members: []map[string]any{
			standby("openbao-0", oldRevision), leader("openbao-1", oldRevision),
		}}, nil, "2 of 3 members exist"},
		{"two members labelled active", rolloutCluster{members: []map[string]any{
			leader("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision),
		}}, nil, "2 members are labelled active, want one"},
		{"an eviction the disruption budget refuses", rolloutCluster{
			members: []map[string]any{standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision)},
			answers: caughtUp("openbao-1"),
		}, []string{"FAKE_EVICT_EXIT=1"}, "the API server refused to evict openbao-0"},
		{"a timeout that is no number of seconds", rolloutCluster{members: []map[string]any{
			standby("openbao-0", oldRevision), leader("openbao-1", oldRevision), standby("openbao-2", oldRevision),
		}}, []string{"ROLLOUT_MEMBER_TIMEOUT=5m"}, "not a number of seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runRollout(t, tc.cluster, tc.env...)

			require.Error(t, run.err, run.out)
			assert.Contains(t, run.out, tc.says, "what the rollout printed")
			assert.False(t, strings.Contains(strings.Join(run.calls, "\n"), "--watch"), "it waited on no member: %v", run.calls)
		})
	}
}
