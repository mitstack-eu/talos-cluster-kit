package pools_test

// A worker's Jobs drain it under its pods' disruption budgets, naming the budget that holds one, and remove its Node
// object once they have released each claim on a local volume of it.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const worker = "shop-cloud-worker-fsn1-1"

// fakeKubectl drains with FAKE_DRAIN_EXIT and answers each read from testdata, a claim's uid as a new claim of its name has.
const fakeKubectl = `
case "$*" in
  drain*) exit "${FAKE_DRAIN_EXIT:-0}" ;;
  "get job"*) printf '%s' "${FAKE_FAILED:-}" ;;
  "get pods -A"*) cat testdata/pods.json ;;
  "get pods -n"*) cat testdata/claim-pods.json ;;
  "get pv "*) cat testdata/volumes.json ;;
  "get pvc data-db-0"*) printf uid-of-data-db-0 ;;
  "get pvc cache-0"*) printf uid-of-cache-0 ;;
esac
`

// fakeCurl answers a dry-run eviction of the pod FAKE_HELD names as the API does one a disruption budget refuses.
const fakeCurl = `
if [ "${*: -1}" = "https://10.96.0.1:443/api/v1/namespaces/shop/pods/${FAKE_HELD:-}/eviction?dryRun=All" ]; then
  cat testdata/eviction-429.json
else
  printf '{"kind":"Status","status":"Success","code":201}'
fi
`

// runWorkerJob runs step's container as the kubelet would on node, with fake settings such as the Job's failed attempts.
func runWorkerJob(t *testing.T, step, node string, fakes ...string) (string, error, []string) {
	t.Helper()
	rendered, err := pools.RenderWorkerJob(step, "fsn1-1", worker)
	require.NoError(t, err)
	jobs := testutil.Kind(testutil.Documents(t, rendered), "Job")
	require.Len(t, jobs, 1)
	container := testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0")
	tools := testutil.NewTools(t, map[string]string{"kubectl": fakeKubectl, "curl": fakeCurl})
	tools.AddImageJq(t)
	account := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(account, "token"), []byte("token-of-the-job"), 0o600))
	var env []string
	for name, value := range testutil.ContainerEnv(t, container) {
		env = append(env, name+"="+value)
	}
	env = append(env, "HOME="+tools.Dir, "NODE_NAME="+node, "JOB_NAMESPACE=mitstack-node", "SERVICE_ACCOUNT_DIR="+account,
		"KUBERNETES_SERVICE_HOST=10.96.0.1", "KUBERNETES_SERVICE_PORT=443")
	out, err := testutil.RunBash(t, testutil.Field(t, container, "command", "2").(string), tools.Env(append(env, fakes...)...))
	return out, err, tools.Calls(t)
}

func TestWorkerJob_DrainsTheNodeUnderItsPodsDisruptionBudgets(t *testing.T) {
	out, err, calls := runWorkerJob(t, pools.DrainStep, "shop-node-hel1")

	require.NoError(t, err, out)
	assert.Equal(t, []string{"kubectl drain " + worker + " --ignore-daemonsets --delete-emptydir-data --force --timeout=600s"}, calls)
}

func TestWorkerJob_ADrainADisruptionBudgetHoldsFailsNamingTheBudgetAndUncordonsOnlyOnItsLastAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		failed    string
		uncordons bool
	}{
		"its first attempt":      {"", false},
		"an attempt before last": {"4", false},
		"its last attempt":       {"5", true},
	} {
		t.Run(name, func(t *testing.T) {
			out, err, calls := runWorkerJob(t, pools.DrainStep, "shop-node-hel1", "FAKE_DRAIN_EXIT=1", "FAKE_FAILED="+tc.failed, "FAKE_HELD=web-0")

			require.Error(t, err, "the drain of a node whose pod a budget holds")
			lines := strings.Split(strings.TrimSpace(out), "\n")
			assert.Equal(t, "drain: pod shop/web-0 waits on a disruption budget in namespace shop: The disruption budget web needs 1 healthy pods and has 1 currently",
				lines[len(lines)-1], "the Job's last words, which its failed pod's message keeps")
			assert.Contains(t, strings.Join(calls, "\n"), "Authorization: Bearer token-of-the-job", "the dry-run eviction's credential")
			assert.Equal(t, tc.uncordons, slices.Contains(calls, "kubectl uncordon "+worker), "the node given back to the scheduler: %v", calls)
		})
	}
}

func TestWorkerJob_RemoveNodeReleasesEachClaimOnALocalVolumeOfTheNodeAndThenDeletesItsNodeObject(t *testing.T) {
	out, err, calls := runWorkerJob(t, pools.RemoveNodeStep, "shop-node-hel1")

	require.NoError(t, err, out)
	assert.Equal(t, []string{
		"kubectl delete pvc data-db-0 -n shop --wait=false",
		"kubectl delete pod db-0 -n shop --wait=false",
		"kubectl delete node " + worker + " --ignore-not-found",
	}, slices.DeleteFunc(calls, func(call string) bool { return strings.HasPrefix(call, "kubectl get ") }),
		"the claim on another node's volume, a claim made anew under a released one's name and a network volume's are kept")
}

func TestWorkerJob_RefusesToActOnTheNodeItRunsOn(t *testing.T) {
	for _, step := range []string{pools.DrainStep, pools.RemoveNodeStep} {
		out, err, calls := runWorkerJob(t, step, worker)

		require.Error(t, err, "the %s Job on the node it acts on", step)
		assert.Contains(t, out, "runs on "+worker, "the %s Job's refusal", step)
		assert.Empty(t, calls, "the %s Job's calls", step)
	}
}

func TestWorkerJob_GrantsEachStepOnlyItsOneNodeAndCountsTheAttemptsItsJobGets(t *testing.T) {
	for step, verbs := range map[string][]any{pools.DrainStep: {"get", "patch"}, pools.RemoveNodeStep: {"get", "delete"}} {
		rendered, err := pools.RenderWorkerJob(step, "fsn1-1", worker)
		require.NoError(t, err)
		docs := testutil.Documents(t, rendered)

		roles, jobs := testutil.Kind(docs, "ClusterRole"), testutil.Kind(docs, "Job")
		require.Len(t, roles, 1)
		require.Len(t, jobs, 1)
		nodes := testutil.Field(t, roles[0], "rules", "0")
		assert.Equal(t, []any{"nodes"}, testutil.Field(t, nodes, "resources"), "the %s Job's first rule", step)
		assert.Equal(t, []any{worker}, testutil.Field(t, nodes, "resourceNames"), "the nodes the %s Job may change", step)
		assert.Equal(t, verbs, testutil.Field(t, nodes, "verbs"), "what the %s Job may do to its node", step)
		assert.Equal(t, "mitstack-"+step+"-fsn1-1", testutil.Field(t, jobs[0], "metadata", "name"), "the %s Job's name", step)
		env := testutil.ContainerEnv(t, testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0"))
		assert.Equal(t, fmt.Sprint(testutil.Field(t, jobs[0], "spec", "backoffLimit").(int)+1), env["ATTEMPTS"], "the attempts the %s Job's script counts", step)
	}
}
