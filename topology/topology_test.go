package topology_test

// The labeller labels each joined node with its zone, the region and whether it is a burst node, under one field manager,
// and moves each pod whose placement refuses a burst node off it.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/mitstack-eu/talos-cluster-kit/topology"
)

// fakeKubectl finds the nodes FAKE_JOINED names, fails a read with FAKE_GET_EXIT and a label with FAKE_LABEL_EXIT, lists the
// pods in the file FAKE_PODS names, and refuses the eviction of FAKE_REFUSED with FAKE_EVICT_ERR once it checks the eviction names its pod.
const fakeKubectl = `
case "$1 $2" in
  "get node")
    [ "${FAKE_GET_EXIT:-0}" -eq 0 ] || exit "$FAKE_GET_EXIT"
    case " $FAKE_JOINED " in *" $3 "*) echo "node/$3" ;; esac ;;
  "label node") exit "${FAKE_LABEL_EXIT:-0}" ;;
  "get pods") cat "${FAKE_PODS:?}" ;;
  "create --raw")
    jq -e --arg path "$3" '.apiVersion == "policy/v1" and .kind == "Eviction"
      and "/api/v1/namespaces/\(.metadata.namespace)/pods/\(.metadata.name)/eviction" == $path' >/dev/null || exit 3
    case "$3" in *"/pods/${FAKE_REFUSED:-}/eviction") echo "$FAKE_EVICT_ERR" >&2; exit 1 ;; esac ;;
  *) exit 1 ;;
esac
`

var shop = topology.Inputs{
	Region:     "eu-central",
	Nodes:      []topology.Node{{Name: "shop-sub-0123-node-fsn1", Zone: "fsn1"}, {Name: "shop-sub-0123-node-nbg1", Zone: "nbg1"}},
	ToolsImage: testutil.ToolsImage,
}

// onBurst lists the pods on a burst node: two the kit keeps off burst nodes, one leaving, one finished, a runner's
// and one pinned to the node by name.
var onBurst = filepath.Join("testdata", "burst-node-pods.json")

var withBurst = topology.Inputs{
	Region:     "eu-central",
	Nodes:      []topology.Node{{Name: "shop-sub-0123-node-fsn1", Zone: "fsn1"}, {Name: "shop-sub-0123-node-nbg1", Zone: "nbg1", Burst: true}},
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
	fakes.AddImageJq(t)
	vars := []string{}
	for name, value := range testutil.ContainerEnv(t, c) {
		vars = append(vars, name+"="+value)
	}
	out, err := testutil.RunBash(t, string(script), fakes.Env(append(vars, env...)...))
	return fakes, out, err
}

// labelCall labels a steady node, taking off the burst label it may carry.
func labelCall(node, zone string) string { return labelling(node, zone, "mitstack.dev/burst-") }

func burstCall(node, zone string) string { return labelling(node, zone, "mitstack.dev/burst=true") }

func labelling(node, zone, burst string) string {
	return "kubectl label node " + node + " --overwrite --field-manager=" + topology.FieldManager + " " +
		topology.ZoneLabel + "=" + zone + " " + topology.RegionLabel + "=eu-central " + burst
}

func eviction(namespace, name string) string {
	return "kubectl create --raw /api/v1/namespaces/" + namespace + "/pods/" + name + "/eviction -f -"
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

func TestManifests_LabelsABurstNodeBurstThenEvictsEachPodOnItWhosePlacementRefusesABurstNode(t *testing.T) {
	tools, out, err := runLabeller(t, withBurst, "FAKE_JOINED=shop-sub-0123-node-fsn1 shop-sub-0123-node-nbg1", "FAKE_PODS="+onBurst)

	require.NoError(t, err, out)
	assert.Equal(t, []string{
		"kubectl get node shop-sub-0123-node-fsn1 --ignore-not-found -o name",
		labelCall("shop-sub-0123-node-fsn1", "fsn1"),
		"kubectl get node shop-sub-0123-node-nbg1 --ignore-not-found -o name",
		burstCall("shop-sub-0123-node-nbg1", "nbg1"),
		"kubectl get pods -A --field-selector spec.nodeName=shop-sub-0123-node-nbg1 -o json",
		eviction("argocd", "argocd-server-0"),
		eviction("logs", "loki-0"),
	}, tools.Calls(t), "no pod already leaving, finished, or free to run on a burst node is evicted")
}

func TestManifests_ARefusedEvictionFailsTheRun(t *testing.T) {
	cases := []struct {
		name, refusal string
		rest          bool
	}{
		{"a pod its disruption budget holds is named with the budget's refusal, the rest are evicted and the run fails",
			"Error from server (TooManyRequests): Cannot evict pod as it would violate the pod's disruption budget.", true},
		{"any other refusal fails the run before the next eviction", "Error from server (Forbidden): pods is forbidden", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, out, err := runLabeller(t, withBurst, "FAKE_JOINED=shop-sub-0123-node-nbg1", "FAKE_PODS="+onBurst,
				"FAKE_REFUSED=argocd-server-0", "FAKE_EVICT_ERR="+tc.refusal)

			require.Error(t, err, out)
			assert.Contains(t, out, "argocd/argocd-server-0")
			assert.Contains(t, out, tc.refusal)
			assert.Equal(t, tc.rest, slices.Contains(tools.Calls(t), eviction("logs", "loki-0")), "whether the next pod is evicted")
		})
	}
}

func TestManifests_FailsWhenTheBurstNodesPodsCannotBeRead(t *testing.T) {
	tools, out, err := runLabeller(t, withBurst, "FAKE_JOINED=shop-sub-0123-node-nbg1")

	require.Error(t, err, out)
	assert.NotContains(t, strings.Join(tools.Calls(t), "\n"), "create --raw")
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

func TestManifests_MayReadAndPatchTheNodesItNamesAndListAndEvictPodsAndNoOtherObject(t *testing.T) {
	docs := render(t, shop)

	roles, bindings := testutil.Kind(docs, "ClusterRole"), testutil.Kind(docs, "ClusterRoleBinding")
	require.Len(t, roles, 1)
	assert.Equal(t, []any{map[string]any{
		"apiGroups": []any{""}, "resources": []any{"nodes"},
		"resourceNames": []any{"shop-sub-0123-node-fsn1", "shop-sub-0123-node-nbg1"}, "verbs": []any{"get", "patch"},
	}, map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"list"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods/eviction"}, "verbs": []any{"create"}},
	}, roles[0]["rules"], "no node list, no status, no other node, and no pod write but an eviction")
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
	}, testutil.ContainerEnv(t, c), "no burst node to name")
	assert.Equal(t, "shop-sub-0123-node-nbg1", testutil.ContainerEnv(t, container(t, render(t, withBurst)))["TOPOLOGY_BURST_NODES"])
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
