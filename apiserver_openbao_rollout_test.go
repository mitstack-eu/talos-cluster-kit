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
	// rolloutNode is the node each member is bound to, which no kubelet serves, so a member's deletion waits for a force.
	rolloutNode = "shop-node-fsn1"
)

// standInKubelet answers kubectl's exec into a member, which the bare API server has no kubelet to run, once the API
// server has authorized it, and passes every other call to the tools image's kubectl.
const standInKubelet = `#!/usr/bin/env bash
set -euo pipefail
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
		DeletionTimestamp string
		Labels            map[string]string
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

	t.Run("the account may make each call the rollout makes in OpenBao's namespace and no other", func(t *testing.T) {
		for _, tc := range []struct {
			verb, resource, subresource, namespace, answer string
		}{
			{"get", "statefulsets.apps", "", "openbao", "yes"},
			{"list", "pods", "", "openbao", "yes"},
			{"watch", "pods", "", "openbao", "yes"},
			{"create", "pods", "eviction", "openbao", "yes"},
			{"create", "pods", "exec", "openbao", "yes"},
			{"delete", "pods", "", "openbao", "no"},
			{"patch", "statefulsets.apps", "", "openbao", "no"},
			{"get", "secrets", "", "openbao", "no"},
			{"create", "pods", "eviction", "kube-system", "no"},
			{"create", "pods", "exec", "kube-system", "no"},
		} {
			out, _ := server.asIn(t, "openbao", rolloutAccount, nil, "kubectl", "auth", "can-i", tc.verb, tc.resource, "--subresource="+tc.subresource, "-n", tc.namespace)

			assert.Equal(t, tc.answer, strings.TrimSpace(out), "may the rollout's account %s %s %s in %s", tc.verb, tc.resource, tc.subresource, tc.namespace)
		}
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
	rolloutJob := func() *exec.Cmd {
		return server.asInCommand(t, "openbao", testutil.Field(t, pod, "serviceAccountName").(string), args, command(t, container)...)
	}

	t.Run("an eviction the disruption budget refuses, as it does until it counts a returned member, fails the Job and evicts no member", func(t *testing.T) {
		server.budgetCounts(t, 2)

		out, err := rolloutJob().CombinedOutput()

		require.Error(t, err, "the rollout printed:\n%s", out)
		assert.Contains(t, string(out), "openbao-rollout: the API server refused to evict openbao-0")
		var members struct{ Items []evictedMember }
		require.NoError(t, json.Unmarshal(server.admin(t, nil, "get", "pods", "-n", "openbao", "-o", "json"), &members))
		require.Len(t, members.Items, 3)
		for _, m := range members.Items {
			assert.Empty(t, m.Metadata.DeletionTimestamp, "a member's deletion after the refusal")
		}
	})

	server.budgetCounts(t, 3)
	cmd := rolloutJob()
	printed, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = cmd.Stdout
	require.NoError(t, cmd.Start())

	evictedLine := regexp.MustCompile(`^openbao-rollout: evicted (\S+) `)
	leader := "openbao-1"
	var out strings.Builder
	var order []string
	for lines := bufio.NewScanner(printed); lines.Scan(); {
		out.WriteString(lines.Text() + "\n")
		m := evictedLine.FindStringSubmatch(lines.Text())
		if m == nil {
			continue
		}
		name := m[1]
		order = append(order, name)
		var evicted evictedMember
		require.NoError(t, json.Unmarshal(server.admin(t, nil, "get", "pod", name, "-n", "openbao", "-o", "json"), &evicted))
		assert.NotEmpty(t, evicted.Metadata.DeletionTimestamp, "the eviction of %s deletes its pod gracefully", name)
		var budget budgetStatus
		require.NoError(t, json.Unmarshal(server.admin(t, nil, "get", "poddisruptionbudget", "openbao", "-n", "openbao", "-o", "json"), &budget))
		assert.Equal(t, 0, budget.Status.DisruptionsAllowed, "the budget allows no other disruption while %s is down", name)
		assert.Contains(t, budget.Status.DisruptedPods, name, "the API server took the eviction of %s through the budget", name)
		server.admin(t, nil, "delete", "pod", name, "-n", "openbao", "--grace-period=0", "--force")
		if name == leader {
			leader = "openbao-0"
			server.admin(t, nil, "label", "pod", leader, "-n", "openbao", "--overwrite", "openbao-active=true")
		}
		server.admin(t, openBaoMember(t, statefulSet, name, rolloutNewRevision, name == leader), "create", "-f", "-")
		// The disruption controller recounts the moment a member turns Ready, before the rollout's next two reads end,
		// which a stand-in whose every write is a container's run cannot match, so it writes the budget first.
		server.budgetCounts(t, 3)
		server.memberRuns(t, name)
	}
	err = cmd.Wait()

	require.NoError(t, err, "the rollout printed:\n%s", out.String())
	assert.Equal(t, []string{"openbao-0", "openbao-2", "openbao-1"}, order, "the standbys in name order, then the active member, whatever its ordinal")
	assert.Contains(t, out.String(), "the API server passed kubectl exec -n openbao openbao-1 -c openbao -- bao read -tls-skip-verify -format=json sys/leader on to the kubelet",
		"the account's exec into a member passes the API server's authorization")
	assert.Contains(t, out.String(), "openbao-rollout: openbao-1 runs revision "+rolloutNewRevision+", unsealed, at Raft index 4100")
}
