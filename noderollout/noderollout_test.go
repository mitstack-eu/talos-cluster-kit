package noderollout_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/internal/redundanttest"
	"github.com/mitstack-eu/talos-cluster-kit/noderollout"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// release is the release testutil.ToolsImage carries, the digest each node's annotation names once it runs it.
const release = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeKubectl lists FAKE_NODES, each "<name>:<address>:<control|worker>", with the release FAKE_STATE/<name> holds as
// the node's machine-config annotation.
const fakeKubectl = `
node_json() {
  local labels='{}' annotations='{}'
  [ "$3" = control ] && labels='{"node-role.kubernetes.io/control-plane":""}'
  [ -f "$FAKE_STATE/$1" ] && annotations="{\"mitstack.dev/machine-config\":\"$(cat "$FAKE_STATE/$1")\"}"
  printf '{"metadata":{"name":"%s","labels":%s,"annotations":%s},"status":{"addresses":[{"type":"InternalIP","address":"%s"}],"conditions":[{"type":"Ready","status":"True"}]}}' \
    "$1" "$labels" "$annotations" "$2"
}
case "$*" in
  "get nodes -o json")
    printf '{"items":['
    sep=
    for n in $FAKE_NODES; do IFS=: read -r name address role <<<"$n"; printf '%s' "$sep"; node_json "$name" "$address" "$role"; sep=,; done
    printf ']}\n' ;;
  "get node "*" -o json")
    for n in $FAKE_NODES; do IFS=: read -r name address role <<<"$n"; [ "$name" != "$3" ] || node_json "$name" "$address" "$role"; done ;;
esac
`

// fakeTalosctl answers a node's configuration as text naming its address, reads etcd unhealthy on each address
// FAKE_UNHEALTHY names, or FAKE_UNHEALTHY_AFTER_APPLY once a node took a configuration, and on an apply records the
// release the applied file names as the node's annotation, unless FAKE_APPLY_IGNORED names the node's address.
const fakeTalosctl = `
case "$3 $4" in
  "get machineconfig") echo "the running configuration of $2" ;;
  "service etcd")
    for address in ${2//,/ }; do
      health=OK
      [[ " ${FAKE_UNHEALTHY:-} $(cat "$FAKE_STATE/.unhealthy" 2>/dev/null) " != *" $address "* ]] || health=Fail
      printf 'NODE\t%s\nID\tetcd\nSTATE\tRunning\nHEALTH\t%s\n' "$address" "$health"
    done ;;
  "apply-config --mode")
    [[ " ${FAKE_APPLY_IGNORED:-} " != *" $2 "* ]] || exit 0
    for n in $FAKE_NODES; do IFS=: read -r name address role <<<"$n"; [ "$address" != "$2" ] || sed -n 's/^release: //p' "$7" > "$FAKE_STATE/$name"; done
    echo "${FAKE_UNHEALTHY_AFTER_APPLY:-}" >"$FAKE_STATE/.unhealthy" ;;
esac
`

// fakeMachineConfig renders a configuration naming the release, and refuses with machine-config's own refusal the
// configuration of each address FAKE_REBOOT_ONLY names.
const fakeMachineConfig = `
running=$(cat)
for address in ${FAKE_REBOOT_ONLY:-}; do
  if [ "$running" = "the running configuration of $address" ]; then
    echo "machine-config: render the running configuration: the release changes machine.install: a node takes this setting only when it reboots" >&2
    exit 3
  fi
done
printf 'release: %s\n' "$2"
`

// threeNodes lists a worker first, so the order the Job takes nodes in is its own.
const threeNodes = "poc-worker1:10.0.1.4:worker poc-cp2:10.0.0.5:control poc-cp1:10.0.0.4:control"

// rolloutBegan is when the Update began, a minute before the node report it is judged by.
var rolloutBegan = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

// etcdReport is a node report read a minute after the Update began, its three members healthy but those unhealthy names.
func etcdReport(unhealthy ...string) report.Read {
	var members []string
	for _, hostname := range []string{"poc-cp1", "poc-cp2", "poc-cp3"} {
		members = append(members, fmt.Sprintf(`{"hostname":%q,"healthy":%t}`, hostname, !slices.Contains(unhealthy, hostname)))
	}
	return report.Read{Body: []byte(`{"etcd":{"members":[` + strings.Join(members, ",") + `]}}`), At: rolloutBegan.Add(time.Minute)}
}

func rollout(read report.Read) noderollout.Rollout {
	return noderollout.Rollout{ToolsImage: testutil.ToolsImage, Report: read, StepStart: rolloutBegan, Guard: redundanttest.Guard}
}

// renderJob renders the rollout while every etcd member reads healthy, with its Job's container.
func renderJob(t *testing.T) ([]map[string]any, map[string]any) {
	t.Helper()
	rendered, err := noderollout.Job(rollout(etcdReport()))
	require.NoError(t, err)
	docs := testutil.Documents(t, rendered)
	jobs := testutil.Kind(docs, "Job")
	require.Len(t, jobs, 1)
	return docs, testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0").(map[string]any)
}

// cluster is a run of the Job's script against the fake cluster of nodes, where each name of atRelease runs the
// release already.
type cluster struct {
	tools *testutil.Tools
	state string
	env   []string
}

func newCluster(t *testing.T, nodes string, atRelease ...string) cluster {
	t.Helper()
	c := cluster{state: t.TempDir(), tools: testutil.NewTools(t, map[string]string{"kubectl": fakeKubectl, "talosctl": fakeTalosctl, "machine-config": fakeMachineConfig})}
	c.tools.AddImageJq(t)
	for _, name := range atRelease {
		require.NoError(t, os.WriteFile(filepath.Join(c.state, name), []byte(release+"\n"), 0o600))
	}
	c.env = []string{"FAKE_NODES=" + nodes, "FAKE_STATE=" + c.state, "NODE_WAIT_SECONDS=0", "POLL_SECONDS=0"}
	return c
}

func (c cluster) run(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	_, container := renderJob(t)
	args := append(slices.Clone(c.env), "RELEASE="+testutil.ContainerEnv(t, container)["RELEASE"])
	return testutil.RunBash(t, testutil.Field(t, container, "command", "2").(string), c.tools.Env(append(args, extra...)...))
}

// calls are the tools' calls, each applied file's path read as <rendered>, since the script writes it in a directory
// of its own.
func (c cluster) calls(t *testing.T) []string {
	t.Helper()
	return strings.Split(regexp.MustCompile(`--file \S+`).ReplaceAllString(strings.Join(c.tools.Calls(t), "\n"), "--file <rendered>"), "\n")
}

func applies(calls []string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(call string) bool { return !strings.Contains(call, "apply-config") })
}

func TestJob_GivesEachNodeTheReleasesConfigurationOneAtATimeTheControlPlaneFirstAndKeepsNoConfiguration(t *testing.T) {
	c := newCluster(t, threeNodes)
	tmp := t.TempDir()

	out, err := c.run(t, "TMPDIR="+tmp)

	require.NoError(t, err, out)
	etcdRead := "talosctl -n 10.0.0.4,10.0.0.5 service etcd"
	assert.Equal(t, []string{
		"kubectl get nodes -o json",
		"talosctl -n 10.0.0.4 get machineconfig v1alpha1 -o jsonpath={.spec}", "machine-config --release " + release,
		"talosctl -n 10.0.0.5 get machineconfig v1alpha1 -o jsonpath={.spec}", "machine-config --release " + release,
		"talosctl -n 10.0.1.4 get machineconfig v1alpha1 -o jsonpath={.spec}", "machine-config --release " + release,
		etcdRead, "talosctl -n 10.0.0.4 apply-config --mode no-reboot --file <rendered>", "kubectl get node poc-cp1 -o json",
		etcdRead, "talosctl -n 10.0.0.5 apply-config --mode no-reboot --file <rendered>", "kubectl get node poc-cp2 -o json",
		etcdRead, "talosctl -n 10.0.1.4 apply-config --mode no-reboot --file <rendered>", "kubectl get node poc-worker1 -o json",
	}, c.calls(t), "every node's configuration is rendered before the first one changes, then each changes once etcd reads healthy and only after the one before it runs the release")
	for _, name := range []string{"poc-cp1", "poc-cp2", "poc-worker1"} {
		annotation, err := os.ReadFile(filepath.Join(c.state, name))
		require.NoError(t, err)
		assert.Equal(t, release+"\n", string(annotation), "the release %s runs", name)
	}
	left, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, left, "files the Job left, which hold the cluster's keys")
	assert.NotContains(t, out, "the running configuration of", "the Job printed a node's configuration")
}

func TestJob_ChangesOnlyTheNodesThatDoNotRunTheReleaseYet(t *testing.T) {
	for _, tc := range []struct {
		name            string
		atRelease, want []string
	}{
		{"a sent-again Update", []string{"poc-cp1", "poc-cp2"}, []string{"talosctl -n 10.0.1.4 apply-config --mode no-reboot --file <rendered>"}},
		{"a cluster at the release", []string{"poc-cp1", "poc-cp2", "poc-worker1"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, threeNodes, tc.atRelease...)

			out, err := c.run(t)

			require.NoError(t, err, out)
			assert.Equal(t, tc.want, applies(c.calls(t)))
			assert.NotRegexp(t, `-n 10\.0\.0\.[45] get`, strings.Join(c.calls(t), "\n"), "the configuration of a node at the release is read")
		})
	}
}

func TestJob_ChangesNoNodeAfterARefusalOrANodeThatDoesNotComeUpAtTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name, nodes, env string
		exit             int
		applies, names   []string
	}{
		{"a setting a node takes only at reboot", threeNodes, "FAKE_REBOOT_ONLY=10.0.1.4", 3, []string{}, []string{"poc-worker1", "machine.install"}},
		{"an unhealthy etcd member", threeNodes, "FAKE_UNHEALTHY=10.0.0.5", 1, []string{}, []string{"10.0.0.5"}},
		{"an etcd member that turns unhealthy after the first node", threeNodes, "FAKE_UNHEALTHY_AFTER_APPLY=10.0.0.5", 1, []string{"talosctl -n 10.0.0.4 apply-config --mode no-reboot --file <rendered>"}, []string{"10.0.0.5", "poc-cp2 keeps its configuration"}},
		{"a control-plane node listing no address", "poc-cp2:10.0.0.5:control poc-cp1::control", "NODE_WAIT_SECONDS=0", 1, []string{}, []string{"poc-cp1 lists no internal address to read etcd at"}},
		{"a worker listing no address", "poc-cp1:10.0.0.4:control poc-worker1::worker", "NODE_WAIT_SECONDS=0", 1, []string{}, []string{"poc-worker1 lists no internal address to reach its Talos API at"}},
		{"a node that does not come up at the release", threeNodes, "FAKE_APPLY_IGNORED=10.0.0.4", 1, []string{"talosctl -n 10.0.0.4 apply-config --mode no-reboot --file <rendered>"}, []string{"poc-cp1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, tc.nodes)

			out, err := c.run(t, tc.env)

			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			assert.Equal(t, tc.exit, exit.ExitCode(), "the Job's pod failure policy fails the Job on exit 3 without a retry")
			assert.Equal(t, tc.applies, applies(c.calls(t)), "the nodes that changed")
			for _, name := range tc.names {
				assert.Contains(t, out, name)
			}
		})
	}
}

func TestJob_RendersNoRolloutUnlessTheReportReadsEveryEtcdMemberServingSinceTheUpdateBegan(t *testing.T) {
	for _, tc := range []struct {
		name string
		read report.Read
		want []string
	}{
		{"an unhealthy member", etcdReport("poc-cp2"), []string{"refused:redundancy-below-minimum", `"poc-cp2" read not serving at 2026-11-19T09:01:00Z`}},
		{"a report read before the Update began", report.Read{Body: etcdReport().Body, At: rolloutBegan.Add(-time.Minute)}, []string{"before the step began"}},
		{"a report listing no member", report.Read{Body: []byte(`{"etcd":{"members":[]}}`), At: rolloutBegan.Add(time.Minute)}, []string{"no etcd member"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := noderollout.Job(rollout(tc.read))

			require.Error(t, err)
			assert.Empty(t, rendered, "the rollout Job rendered")
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestJob_RunsTheToolsImageWithTheLeastRightsAndNoRetryOnARefusal(t *testing.T) {
	docs, container := renderJob(t)
	job := testutil.Kind(docs, "Job")[0]

	assert.Equal(t, testutil.ToolsImage, container["image"])
	assert.Equal(t, release, testutil.ContainerEnv(t, container)["RELEASE"])
	assert.Equal(t, []any{map[string]any{
		"action":      "FailJob",
		"onExitCodes": map[string]any{"containerName": "apply-machine-config", "operator": "In", "values": []any{3}},
	}}, testutil.Field(t, job, "spec", "podFailurePolicy", "rules"))
	assert.Equal(t, 3, testutil.Field(t, job, "spec", "backoffLimit"), "the retries of any other failure")
	var talosRoles []any
	for _, account := range testutil.Kind(docs, "ServiceAccount") {
		if account["apiVersion"] == "talos.dev/v1alpha1" {
			talosRoles = append(talosRoles, testutil.Field(t, account, "spec", "roles").([]any)...)
		}
	}
	assert.Equal(t, []any{"os:admin"}, talosRoles, "Talos applies a configuration and answers one for os:admin alone")
	roles := testutil.Kind(docs, "ClusterRole")
	require.Len(t, roles, 1)
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"nodes"}, "verbs": []any{"get", "list"}}}, roles[0]["rules"])
}

func TestRelease_IsTheToolsImagesDigestAndNamesItsRolloutInTheClustersOwnDirectory(t *testing.T) {
	got, err := noderollout.Release(testutil.ToolsImage)
	require.NoError(t, err)
	_, err = noderollout.Release("ghcr.io/mitstack-eu/talos-tools:v0.1.0")
	require.Error(t, err, "a tools image named by its tag alone")
	path := noderollout.Path(release)

	assert.Equal(t, release, got)
	assert.Equal(t, "cluster/mitstack-machine-config-0123456789ab.yaml", path)
	assert.Equal(t, "mitstack-machine-config-0123456789ab", noderollout.JobName(release))
	assert.True(t, noderollout.IsPath(path))
	for _, other := range []string{etcd.RemovalPath("poc-cp1"), "cluster/component-argocd.yaml", "components/mitstack-machine-config-0123456789ab.yaml", "cluster/mitstack-machine-config-.yaml"} {
		assert.False(t, noderollout.IsPath(other), other)
	}
}
