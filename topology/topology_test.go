package topology_test

// The labeller labels each joined node with its zone and the region under one field manager, and may touch no other node.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/mitstack-eu/talos-cluster-kit/topology"
)

// fakeKubectl finds the nodes FAKE_JOINED names, fails a read with FAKE_GET_EXIT and a label with FAKE_LABEL_EXIT.
const fakeKubectl = `
case "$1 $2" in
  "get node")
    [ "${FAKE_GET_EXIT:-0}" -eq 0 ] || exit "$FAKE_GET_EXIT"
    case " $FAKE_JOINED " in *" $3 "*) echo "node/$3" ;; esac ;;
  "label node") exit "${FAKE_LABEL_EXIT:-0}" ;;
  *) exit 1 ;;
esac
`

var shop = topology.Inputs{
	Region:     "eu-central",
	Nodes:      []topology.Node{{Name: "shop-sub-0123-node-fsn1", Zone: "fsn1"}, {Name: "shop-sub-0123-node-nbg1", Zone: "nbg1"}},
	ToolsImage: testutil.ToolsImage,
}

func render(t *testing.T, in topology.Inputs) []map[string]any {
	t.Helper()
	rendered, err := topology.Manifests(in)
	require.NoError(t, err)
	return testutil.Documents(t, rendered)
}

func container(t *testing.T, docs []map[string]any) any {
	t.Helper()
	cronJobs := testutil.Kind(docs, "CronJob")
	require.Len(t, cronJobs, 1)
	return testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
}

func runLabeller(t *testing.T, in topology.Inputs, env ...string) (*testutil.Tools, string, error) {
	t.Helper()
	c := container(t, render(t, in))
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/label-nodes.sh"}, testutil.Field(t, c, "command"),
		"the CronJob runs the labeller the tools image carries")
	script, err := os.ReadFile(filepath.Join("scripts", "label-nodes.sh"))
	require.NoError(t, err)
	fakes := testutil.NewTools(t, map[string]string{"kubectl": fakeKubectl})
	vars := []string{}
	for name, value := range testutil.ContainerEnv(t, c) {
		vars = append(vars, name+"="+value)
	}
	out, err := testutil.RunBash(t, string(script), fakes.Env(append(vars, env...)...))
	return fakes, out, err
}

func labelCall(node, zone string) string {
	return "kubectl label node " + node + " --overwrite --field-manager=" + topology.FieldManager + " " +
		topology.ZoneLabel + "=" + zone + " " + topology.RegionLabel + "=eu-central"
}

func TestManifests_LabelsEachJoinedNodeWithItsZoneAndTheRegionUnderTheLabellersFieldManager(t *testing.T) {
	tools, out, err := runLabeller(t, shop, "FAKE_JOINED=shop-sub-0123-node-fsn1 shop-sub-0123-node-nbg1")

	require.NoError(t, err, out)
	assert.Equal(t, "mitstack-node-topology", topology.FieldManager, "the manager each Node's managedFields names")
	assert.Equal(t, []string{
		"kubectl get node shop-sub-0123-node-fsn1 --ignore-not-found -o name",
		labelCall("shop-sub-0123-node-fsn1", "fsn1"),
		"kubectl get node shop-sub-0123-node-nbg1 --ignore-not-found -o name",
		labelCall("shop-sub-0123-node-nbg1", "nbg1"),
	}, tools.Calls(t))
}

func TestManifests_LeavesANodeThatHasNotJoinedToALaterRun(t *testing.T) {
	tools, out, err := runLabeller(t, shop, "FAKE_JOINED=shop-sub-0123-node-fsn1")

	require.NoError(t, err, out)
	assert.NotContains(t, strings.Join(tools.Calls(t), "\n"), "label node shop-sub-0123-node-nbg1")
	assert.Contains(t, out, "shop-sub-0123-node-nbg1 has not joined")
}

func TestManifests_FailsWithoutLabellingWhenANodeCannotBeRead(t *testing.T) {
	tools, out, err := runLabeller(t, shop, "FAKE_JOINED=shop-sub-0123-node-fsn1", "FAKE_GET_EXIT=1")

	require.Error(t, err, "a refused read is never taken for a node that has not joined")
	assert.Contains(t, out, "shop-sub-0123-node-fsn1")
	assert.NotContains(t, strings.Join(tools.Calls(t), "\n"), "kubectl label")
}

func TestManifests_FailsWhenALabelIsRefused(t *testing.T) {
	_, out, err := runLabeller(t, shop, "FAKE_JOINED=shop-sub-0123-node-fsn1", "FAKE_LABEL_EXIT=1")

	require.Error(t, err, out)
}

func TestManifests_MayReadAndPatchTheNodesItNamesAndNoOtherObject(t *testing.T) {
	docs := render(t, shop)

	roles, bindings := testutil.Kind(docs, "ClusterRole"), testutil.Kind(docs, "ClusterRoleBinding")
	require.Len(t, roles, 1)
	assert.Equal(t, []any{map[string]any{
		"apiGroups": []any{""}, "resources": []any{"nodes"},
		"resourceNames": []any{"shop-sub-0123-node-fsn1", "shop-sub-0123-node-nbg1"}, "verbs": []any{"get", "patch"},
	}}, roles[0]["rules"], "no list, no status, no other node")
	assert.Empty(t, testutil.Kind(docs, "Role"))
	require.Len(t, bindings, 1)
	assert.Equal(t, testutil.Field(t, roles[0], "metadata", "name"), testutil.Field(t, bindings[0], "roleRef", "name"))
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-node-topology", "namespace": "mitstack-node"}},
		testutil.Field(t, bindings[0], "subjects"))
	for _, account := range testutil.Kind(docs, "ServiceAccount") {
		assert.Equal(t, "v1", account["apiVersion"], "the labeller holds no Talos identity")
	}
}

func TestManifests_RunsTheToolsImageOnAControlPlaneNodeAsANonRootUserWithAReadOnlyRoot(t *testing.T) {
	docs := render(t, shop)
	cronJob := testutil.Kind(docs, "CronJob")[0]
	pod := testutil.Field(t, cronJob, "spec", "jobTemplate", "spec", "template", "spec")
	c := container(t, docs)

	assert.Equal(t, "mitstack-node", testutil.Field(t, cronJob, "metadata", "namespace"))
	assert.Equal(t, "Forbid", testutil.Field(t, cronJob, "spec", "concurrencyPolicy"))
	assert.Equal(t, testutil.ToolsImage, testutil.Field(t, c, "image"))
	assert.Equal(t, "mitstack-node-topology", testutil.Field(t, pod, "serviceAccountName"))
	assert.Equal(t, map[string]any{"node-role.kubernetes.io/control-plane": ""}, testutil.Field(t, pod, "nodeSelector"))
	assert.Equal(t, true, testutil.Field(t, pod, "securityContext", "runAsNonRoot"))
	assert.Equal(t, true, testutil.Field(t, c, "securityContext", "readOnlyRootFilesystem"))
	assert.Equal(t, false, testutil.Field(t, c, "securityContext", "allowPrivilegeEscalation"))
	assert.Equal(t, map[string]string{
		"TOPOLOGY_NODES":         "shop-sub-0123-node-fsn1=fsn1 shop-sub-0123-node-nbg1=nbg1",
		"TOPOLOGY_REGION":        "eu-central",
		"TOPOLOGY_FIELD_MANAGER": topology.FieldManager,
		"HOME":                   "/tmp",
	}, testutil.ContainerEnv(t, c))
}

func TestManifests_RefusesInputsItCannotLabelANodeWith(t *testing.T) {
	for name, tc := range map[string]struct {
		in   func(topology.Inputs) topology.Inputs
		want string
	}{
		"no node": {func(in topology.Inputs) topology.Inputs { in.Nodes = nil; return in }, "no node"},
		"a node name that is no DNS label": {func(in topology.Inputs) topology.Inputs {
			in.Nodes = []topology.Node{{Name: "Shop Node", Zone: "fsn1"}}
			return in
		}, "Shop Node"},
		"a zone that is no label value": {func(in topology.Inputs) topology.Inputs {
			in.Nodes = []topology.Node{{Name: "shop-node-fsn1", Zone: "fsn1 dc14"}}
			return in
		}, "fsn1 dc14"},
		"an empty zone": {func(in topology.Inputs) topology.Inputs {
			in.Nodes = []topology.Node{{Name: "shop-node-fsn1"}}
			return in
		}, "shop-node-fsn1"},
		"a region that is no label value": {func(in topology.Inputs) topology.Inputs { in.Region = "eu central"; return in }, "eu central"},
		"a node named twice": {func(in topology.Inputs) topology.Inputs {
			in.Nodes = append(in.Nodes, topology.Node{Name: "shop-sub-0123-node-fsn1", Zone: "hel1"})
			return in
		}, "shop-sub-0123-node-fsn1"},
		"an image not named by digest": {func(in topology.Inputs) topology.Inputs {
			in.ToolsImage = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.33.0"
			return in
		}, "talos-cluster-kit-tools:v0.33.0"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := topology.Manifests(tc.in(shop))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLabels_NameTheZoneAndTheRegion(t *testing.T) {
	assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "hel1", "topology.kubernetes.io/region": "eu-central"},
		topology.Labels("hel1", "eu-central"))
}
