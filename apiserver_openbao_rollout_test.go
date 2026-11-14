//go:build apiserver

// OpenBao's rollout Job moves each outdated member through the Eviction API of an API server at the Kubernetes release
// the kit pins, under the account OpenBao's component states, the standbys first and the active member last.
package talosclusterkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	rolloutAccount     = "mitstack-openbao-rollout"
	rolloutOldRevision = "openbao-5c4b3a291"
	rolloutNewRevision = "openbao-7d9f8c6b5"
	// otherBudget stands for each disruption budget in OpenBao's namespace but OpenBao's own.
	otherBudget = "mitstack-apiserver-check-other"
	// rolloutNode is the node each member is bound to, which no kubelet serves, so a member's deletion waits for a force.
	rolloutNode = "shop-node-fsn1"
)

// standInKubelet answers kubectl's exec into a member, which the bare API server has no kubelet to run, once the API
// server has authorized it, runs kubectl's wait at the verbosity that logs when its watch holds the budget, and passes
// every other call to the tools image's kubectl.
const standInKubelet = `#!/usr/bin/env bash
set -euo pipefail
[ "$1" != wait ] || exec /usr/local/bin/kubectl -v=6 "$@"
[ "$1" = exec ] || exec /usr/local/bin/kubectl "$@"
if refusal=$(/usr/local/bin/kubectl "$@" 2>&1); then
  echo "stand-in kubelet: kubectl $* ran with no kubelet to run it" >&2
  exit 1
fi
case $refusal in *orbidden*) echo "$refusal" >&2; exit 1 ;; esac
echo "stand-in kubelet: the API server passed kubectl $* on to the kubelet of ` + rolloutNode + `: $refusal" >&2
cat /stand-in/sys-leader.json`

// openBaoMember is a member's pod as the StatefulSet controller and OpenBao's Kubernetes registration label it.
func openBaoMember(t *testing.T, statefulSet map[string]any, name, revision string, active bool) []byte {
	t.Helper()
	labels := map[string]any{"statefulset.kubernetes.io/pod-name": name, "controller-revision-hash": revision,
		"openbao-active": fmt.Sprint(active), "openbao-sealed": "false", "openbao-initialized": "true"}
	for key, value := range testutil.Field(t, statefulSet, "spec", "template", "metadata", "labels").(map[string]any) {
		labels[key] = value
	}
	template := testutil.Field(t, statefulSet, "spec", "template", "spec")
	return stream(t, map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": "openbao", "labels": labels},
		"spec": map[string]any{"nodeName": rolloutNode, "serviceAccountName": testutil.Field(t, template, "serviceAccountName"),
			"containers": []any{map[string]any{"name": "openbao", "image": testutil.Field(t, template, "containers", "0", "image")}}}})
}

// memberRuns writes the status the kubelet would once the member's container runs and passes its readiness probe.
func (s *apiServer) memberRuns(t *testing.T, name string) {
	t.Helper()
	s.admin(t, nil, "patch", "pod", name, "-n", "openbao", "--subresource=status", "--type=merge",
		"-p", `{"status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}]}}`)
}

// budgetCounts writes the status the disruption controller would for healthy of the three members, which allows a
// disruption only while all three are, so the API server evicts a member only while no other is down.
func (s *apiServer) budgetCounts(t *testing.T, healthy int) {
	t.Helper()
	s.admin(t, nil, "patch", "poddisruptionbudget", "openbao", "-n", "openbao", "--subresource=status", "--type=merge", "-p", fmt.Sprintf(
		`{"status": {"observedGeneration": 1, "currentHealthy": %d, "desiredHealthy": 2, "expectedPods": 3, "disruptionsAllowed": %d, "disruptedPods": null}}`,
		healthy, max(healthy-2, 0)))
}

type evictedMember struct {
	Metadata struct {
		Name              string
		DeletionTimestamp string
	}
}

type budgetStatus struct {
	Status struct {
		DisruptionsAllowed int
		DisruptedPods      map[string]string
	}
}

func TestAPIServer_OpenBaosRolloutEvictsEachOutdatedStandbyAndThenTheActiveMemberThroughTheDisruptionBudgetUnderItsOwnAccount(t *testing.T) {
	file := carriedFile(t, testutil.Manifest(t, "openbao", components.HostingServed().Served))
	var job, statefulSet map[string]any
	for _, doc := range testutil.Documents(t, file) {
		switch {
		case doc["kind"] == "Job" && testutil.Field(t, doc, "metadata", "name") == rolloutAccount:
			job = doc
		case doc["kind"] == "StatefulSet":
			statefulSet = doc
		}
	}
	require.NotNil(t, job, "OpenBao's file runs its rollout Job")
	server := startAPIServer(t)
	server.admin(t, argoCDIdentity(t), "apply", "-f", "-")
	server.admin(t, builtInObjects(t, stream(t, statedObjects(t, file)...)), "apply", "-f", "-")
	server.admin(t, nil, "patch", "statefulset", "openbao", "-n", "openbao", "--subresource=status", "--type=merge",
		"-p", `{"status": {"observedGeneration": 1, "replicas": 3, "readyReplicas": 3, "currentRevision": "`+rolloutOldRevision+`", "updateRevision": "`+rolloutNewRevision+`"}}`)
	for _, name := range []string{"openbao-0", "openbao-1", "openbao-2"} {
		server.admin(t, openBaoMember(t, statefulSet, name, rolloutOldRevision, name == "openbao-1"), "create", "-f", "-")
		server.memberRuns(t, name)
	}
	server.admin(t, nil, "create", "poddisruptionbudget", otherBudget, "-n", "openbao", "--selector=app.kubernetes.io/name=none", "--min-available=1")

	t.Run("the account may make each call the rollout makes in OpenBao's namespace and no other", func(t *testing.T) {
		for _, tc := range []struct {
			verb, resource, subresource, namespace, answer string
		}{
			{"get", "statefulsets.apps", "", "openbao", "yes"},
			{"list", "pods", "", "openbao", "yes"},
			{"watch", "pods", "", "openbao", "yes"},
			{"create", "pods", "eviction", "openbao", "yes"},
			{"create", "pods", "exec", "openbao", "yes"},
			{"get", "poddisruptionbudgets.policy/openbao", "", "openbao", "yes"},
			{"watch", "poddisruptionbudgets.policy/openbao", "", "openbao", "yes"},
			{"delete", "pods", "", "openbao", "no"},
			{"patch", "statefulsets.apps", "", "openbao", "no"},
			{"get", "secrets", "", "openbao", "no"},
			{"create", "pods", "eviction", "kube-system", "no"},
			{"create", "pods", "exec", "kube-system", "no"},
			{"get", "poddisruptionbudgets.policy/" + otherBudget, "", "openbao", "no"},
			{"list", "poddisruptionbudgets.policy", "", "openbao", "no"},
			{"watch", "poddisruptionbudgets.policy", "", "openbao", "no"},
			{"patch", "poddisruptionbudgets.policy/openbao", "", "openbao", "no"},
		} {
			out, _ := server.asIn(t, "openbao", rolloutAccount, nil, "kubectl", "auth", "can-i", tc.verb, tc.resource, "--subresource="+tc.subresource, "-n", tc.namespace)

			assert.Equal(t, tc.answer, strings.TrimSpace(out), "may the rollout's account %s %s %s in %s", tc.verb, tc.resource, tc.subresource, tc.namespace)
		}
	})

	t.Run("the account waits on OpenBao's budget by name, which lists no budget, and on no other budget", func(t *testing.T) {
		server.budgetCounts(t, 3)
		wait := []string{"kubectl", "wait", "-v=6", "--for=jsonpath={.status.disruptionsAllowed}=1", "--timeout=3s", "-n", "openbao"}

		waited, err := server.asIn(t, "openbao", rolloutAccount, nil, append(wait, "poddisruptionbudget/openbao")...)
		other, otherErr := server.asIn(t, "openbao", rolloutAccount, nil, append(wait, "poddisruptionbudget/"+otherBudget)...)

		require.NoError(t, err, waited)
		assert.Contains(t, waited, "poddisruptionbudget.policy/openbao condition met")
		assert.Regexp(t, `verb="GET" url="[^"]*/apis/policy/v1/namespaces/openbao/poddisruptionbudgets\?[^"]*fieldSelector=metadata\.name%3Dopenbao&[^"]*sendInitialEvents=true[^"]*watch=true" status="200 OK"`,
			waited, "kubectl wait streams its first read of the budget as a watch by name, which the rule naming the budget holds")
		require.Error(t, otherErr)
		assert.Contains(t, other, `poddisruptionbudgets.policy "`+otherBudget+`" is forbidden`)
	})

	server.admin(t, stream(t, job), "apply", "--server-side", "--dry-run=server", "--field-manager="+rollout.ArgoCDManager, "--as="+argoCDUser, "-f", "-")
	standIn := sharedDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(standIn, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(standIn, "bin", "kubectl"), []byte(standInKubelet), 0o755))
	writeFiles(t, standIn, map[string][]byte{"sys-leader.json": []byte(`{"data": {"ha_enabled": true, "raft_committed_index": 4100, "raft_applied_index": 4100}}`)})
	pod := testutil.Field(t, job, "spec", "template", "spec")
	container := testutil.Field(t, pod, "containers", "0")
	args := append(containerArgs(t, job, pod, container, nil), "-v", standIn+":/stand-in:ro",
		"-e", "PATH=/stand-in/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	rolloutJob := func(env ...string) *exec.Cmd {
		over := slices.Clone(args)
		for _, variable := range env {
			over = append(over, "-e", variable)
		}
		return server.asInCommand(t, "openbao", testutil.Field(t, pod, "serviceAccountName").(string), over, command(t, container)...)
	}
	deleting := func(t *testing.T) []string {
		var members struct{ Items []evictedMember }
		require.NoError(t, json.Unmarshal(server.admin(t, nil, "get", "pods", "-n", "openbao", "-o", "json"), &members))
		require.Len(t, members.Items, 3)
		var names []string
		for _, m := range members.Items {
			if m.Metadata.DeletionTimestamp != "" {
				names = append(names, m.Metadata.Name)
			}
		}
		return names
	}

	t.Run("a budget that allows no disruption within the bound fails the Job, which evicts no member", func(t *testing.T) {
		server.budgetCounts(t, 2)

		out, err := rolloutJob("ROLLOUT_CALL_TIMEOUT=3s").CombinedOutput()

		require.Error(t, err, "the rollout printed:\n%s", out)
		assert.Contains(t, string(out), "OpenBao's disruption budget openbao allowed no disruption within 3s, so openbao-0 is not evicted and no member moves")
		assert.Empty(t, deleting(t), "the members being deleted after the run")
	})

	server.budgetCounts(t, 2)
	cmd := rolloutJob()
	printed, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = cmd.Stdout
	require.NoError(t, cmd.Start())

	evictedLine := regexp.MustCompile(`^openbao-rollout: evicted (\S+) `)
	waitingLine := regexp.MustCompile(`^openbao-rollout: waiting up to \S+ for OpenBao's disruption budget openbao to allow a disruption before evicting (\S+)$`)
	watchHolds := regexp.MustCompile(`reflector\.go:\d+\] "Caches populated"`)
	leader := "openbao-1"
	var out strings.Builder
	var order, waited []string
	freed := false
	for lines := bufio.NewScanner(printed); lines.Scan(); {
		out.WriteString(lines.Text() + "\n")
		if m := waitingLine.FindStringSubmatch(lines.Text()); m != nil && len(order) == 0 {
			waited = append(waited, m[1])
			continue
		}
		if watchHolds.MatchString(lines.Text()) && len(order) == 0 && !freed {
			var budget budgetStatus
			require.NoError(t, json.Unmarshal(server.admin(t, nil, "get", "poddisruptionbudget", "openbao", "-n", "openbao", "-o", "json"), &budget))
			assert.Equal(t, 0, budget.Status.DisruptionsAllowed, "the budget allows no disruption while the rollout's wait holds it")
			assert.Empty(t, deleting(t), "the members being deleted while the rollout waits on the budget")
			server.budgetCounts(t, 3)
			freed = true
			continue
		}
		m := evictedLine.FindStringSubmatch(lines.Text())
		if m == nil {
			continue
		}
		name := m[1]
		order = append(order, name)
		assert.Equal(t, []string{name}, deleting(t), "the eviction of %s deletes its pod gracefully, and no other", name)
		var budget budgetStatus
		require.NoError(t, json.Unmarshal(server.admin(t, nil, "get", "poddisruptionbudget", "openbao", "-n", "openbao", "-o", "json"), &budget))
		assert.Equal(t, 0, budget.Status.DisruptionsAllowed, "the budget allows no other disruption while %s is down", name)
		assert.Contains(t, budget.Status.DisruptedPods, name, "the API server took the eviction of %s through the budget", name)
		server.admin(t, nil, "delete", "pod", name, "-n", "openbao", "--grace-period=0", "--force")
		if name == leader {
			leader = slices.DeleteFunc([]string{"openbao-0", "openbao-1", "openbao-2"}, func(m string) bool { return m == name })[0]
			server.admin(t, nil, "label", "pod", leader, "-n", "openbao", "--overwrite", "openbao-active=true")
		}
		server.admin(t, openBaoMember(t, statefulSet, name, rolloutNewRevision, name == leader), "create", "-f", "-")
		// The disruption controller counts a member back only once it reads Ready, so the stand-in does the same.
		server.memberRuns(t, name)
		server.budgetCounts(t, 3)
	}
	err = cmd.Wait()

	require.NoError(t, err, "the rollout printed:\n%s", out.String())
	assert.Equal(t, []string{"openbao-0"}, waited, "the rollout said it waits on the budget before its first eviction")
	assert.True(t, freed, "the rollout's wait held the budget while it allowed no disruption, and evicted only once it allowed one")
	assert.Equal(t, []string{"openbao-0", "openbao-2", "openbao-1"}, order, "the standbys in name order, then the active member, whatever its ordinal")
	assert.Contains(t, out.String(), "the API server passed kubectl exec -n openbao openbao-1 -c openbao -- bao read -tls-skip-verify -format=json sys/leader on to the kubelet",
		"the account's exec into a member passes the API server's authorization")
	assert.Contains(t, out.String(), "openbao-rollout: openbao-1 runs revision "+rolloutNewRevision+", unsealed, at Raft index 4100")
}
