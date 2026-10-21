package etcd_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// fakeTalosctl answers etcd members from the fourth proof's table and fails remove-member with FAKE_REMOVE_EXIT.
const fakeTalosctl = `
case "$*" in
  *"etcd members"*) cat "$FAKE_MEMBERS" ;;
  *"etcd remove-member"*) exit "${FAKE_REMOVE_EXIT:-0}" ;;
  *kubeconfig*) : ;;
esac
`

// fakeKubectl lists the Node objects a restored etcd still holds.
const fakeKubectl = `
case "$*" in
  *"get nodes"*) cat "$FAKE_NODES" ;;
esac
`

func newTools(t *testing.T) *testutil.Tools {
	t.Helper()
	return testutil.NewTools(t, map[string]string{"talosctl": fakeTalosctl, "kubectl": fakeKubectl})
}

// removeMemberRun is the Job's container: its script and the environment the kubelet gives it.
type removeMemberRun struct {
	script string
	env    map[string]any
}

func renderRemoveMember(t *testing.T, hostname string) ([]map[string]any, removeMemberRun) {
	t.Helper()
	rendered, err := etcd.RemoveMemberJob(etcd.RemoveMember{Hostname: hostname, ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)
	docs := testutil.Documents(t, rendered)
	jobs := testutil.Kind(docs, "Job")
	require.Len(t, jobs, 1)
	container := testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0")
	run := removeMemberRun{script: testutil.Field(t, container, "command", "2").(string), env: map[string]any{}}
	for _, e := range testutil.Field(t, container, "env").([]any) {
		entry := e.(map[string]any)
		run.env[entry["name"].(string)] = entry["value"]
	}
	return docs, run
}

func TestRemoveMemberJob_RemovesTheEtcdMemberBeforeDeletingItsNode(t *testing.T) {
	_, run := renderRemoveMember(t, "poc-talos4-cp3")
	tools := newTools(t)

	out, err := testutil.RunBash(t, run.script, tools.Env(
		"MEMBER_HOSTNAME="+run.env["MEMBER_HOSTNAME"].(string), "HOST_IP=10.0.0.6", "NODE_NAME=poc-talos4-cp2",
		"FAKE_MEMBERS="+filepath.Join("testdata", "members.txt")))

	require.NoError(t, err, out)
	assert.Equal(t, []string{
		"talosctl -n 10.0.0.6 etcd members",
		"talosctl -n 10.0.0.6 etcd remove-member 9341865dff9e2b03",
		"kubectl delete node poc-talos4-cp3 --ignore-not-found",
	}, tools.Calls(t))
}

func TestRemoveMemberJob_KeepsTheNodeWhenTheMemberRemovalFails(t *testing.T) {
	_, run := renderRemoveMember(t, "poc-talos4-cp3")
	tools := newTools(t)

	out, err := testutil.RunBash(t, run.script, tools.Env(
		"MEMBER_HOSTNAME=poc-talos4-cp3", "HOST_IP=10.0.0.6", "NODE_NAME=poc-talos4-cp2",
		"FAKE_MEMBERS="+filepath.Join("testdata", "members.txt"), "FAKE_REMOVE_EXIT=1"))

	require.Error(t, err, out)
	assert.Equal(t, []string{
		"talosctl -n 10.0.0.6 etcd members",
		"talosctl -n 10.0.0.6 etcd remove-member 9341865dff9e2b03",
	}, tools.Calls(t))
}

func TestRemoveMemberJob_RefusesToRemoveTheNodeItRunsOn(t *testing.T) {
	_, run := renderRemoveMember(t, "poc-talos4-cp2")
	tools := newTools(t)

	out, err := testutil.RunBash(t, run.script, tools.Env(
		"MEMBER_HOSTNAME=poc-talos4-cp2", "HOST_IP=10.0.0.6", "NODE_NAME=poc-talos4-cp2",
		"FAKE_MEMBERS="+filepath.Join("testdata", "members.txt")))

	require.Error(t, err)
	assert.Contains(t, out, "the node this Job runs on")
	assert.Empty(t, tools.Calls(t))
}

func TestRemoveMemberJob_GrantsTheAdminRoleAloneAndOnlyThatNodesDeletion(t *testing.T) {
	docs, _ := renderRemoveMember(t, "poc-talos4-cp3")

	talosAccounts := testutil.Kind(docs, "ServiceAccount")
	roles := testutil.Kind(docs, "ClusterRole")

	var talosRoles []any
	for _, account := range talosAccounts {
		if account["apiVersion"] == "talos.dev/v1alpha1" {
			talosRoles = append(talosRoles, testutil.Field(t, account, "spec", "roles").([]any)...)
		}
	}
	assert.Equal(t, []any{"os:admin"}, talosRoles, "Talos accepts a member removal from os:admin alone, and the Job needs no other role")
	require.Len(t, roles, 1)
	assert.Equal(t, []any{map[string]any{
		"apiGroups": []any{""}, "resources": []any{"nodes"}, "resourceNames": []any{"poc-talos4-cp3"}, "verbs": []any{"get", "delete"},
	}}, roles[0]["rules"])
}

func TestRemoveMemberJob_KeepsAHostnameAStringAndRefusesOneNoJobNameHolds(t *testing.T) {
	_, run := renderRemoveMember(t, "true")
	long := strings.Repeat("a", 43)

	_, err := etcd.RemoveMemberJob(etcd.RemoveMember{Hostname: long, ToolsImage: testutil.ToolsImage})

	assert.Equal(t, "true", run.env["MEMBER_HOSTNAME"])
	require.Error(t, err)
	assert.Contains(t, err.Error(), long)
}

func TestBootstrapScript_RestoresFromTheSnapshotThenDeletesTheOldNodes(t *testing.T) {
	tools := newTools(t)
	dir := t.TempDir()
	talosconfig := filepath.Join(dir, "talosconfig")
	snapshot := filepath.Join(dir, "db.snap")
	require.NoError(t, os.WriteFile(talosconfig, []byte("context: admin"), 0o600))
	require.NoError(t, os.WriteFile(snapshot, []byte("snapshot"), 0o600))
	args := etcd.Bootstrap{
		Node: netip.MustParseAddr("10.0.0.5"), Talosconfig: talosconfig, Snapshot: snapshot,
		Keep: []string{"poc-talos4-restore", "poc-talos4-restore2"},
	}.Args()

	out, err := testutil.RunBash(t, string(etcd.BootstrapScript()), tools.Env("FAKE_NODES="+filepath.Join("testdata", "nodes-after-restore.txt")), args...)

	require.NoError(t, err, out)
	calls := tools.Calls(t)
	require.Len(t, calls, 6, "calls: %v", calls)
	assert.Equal(t, "talosctl --talosconfig "+talosconfig+" -e 10.0.0.5 -n 10.0.0.5 bootstrap --recover-from="+snapshot, calls[0])
	assert.Regexp(t, `^talosctl --talosconfig \S+ -e 10.0.0.5 -n 10.0.0.5 kubeconfig \S+/kubeconfig --force$`, calls[1])
	assert.Regexp(t, `^kubectl --kubeconfig \S+/kubeconfig get nodes -o name$`, calls[2])
	for i, node := range []string{"poc-talos4-cp1", "poc-talos4-cp2", "poc-talos4-cp4"} {
		assert.Regexp(t, `^kubectl --kubeconfig \S+/kubeconfig delete node `+node+`$`, calls[3+i])
	}
	assert.NoFileExists(t, talosconfig, "the admin talosconfig outlives the restore")
}

func TestBootstrapScript_StartsANewClusterAndDeletesTheTalosconfig(t *testing.T) {
	tools := newTools(t)
	talosconfig := filepath.Join(t.TempDir(), "talosconfig")
	require.NoError(t, os.WriteFile(talosconfig, []byte("context: admin"), 0o600))
	args := etcd.Bootstrap{Node: netip.MustParseAddr("10.0.0.5"), Talosconfig: talosconfig}.Args()

	out, err := testutil.RunBash(t, string(etcd.BootstrapScript()), tools.Env(), args...)

	require.NoError(t, err, out)
	assert.Equal(t, []string{"talosctl --talosconfig " + talosconfig + " -e 10.0.0.5 -n 10.0.0.5 bootstrap"}, tools.Calls(t))
	assert.NoFileExists(t, talosconfig, "the admin talosconfig outlives the bootstrap")
}
