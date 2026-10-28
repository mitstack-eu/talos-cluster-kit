package talosclusterkit

// Every pod the kit renders that selects control-plane nodes tolerates their NoSchedule taint, which they keep when a
// cluster runs its apps on workers alone.

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const controlPlaneRole = "node-role.kubernetes.io/control-plane"

// renderedManifests is every manifest the kit renders, by the name of what renders it.
func renderedManifests(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	addSet := func(prefix string, set components.Set) {
		for _, m := range slices.Concat(set.Served, set.Inline) {
			out[prefix+"/"+m.Name] = m.Bytes
		}
	}
	addSet("default", components.Default())
	hosting, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	addSet("hosting", hosting)
	collector, err := components.LogCollector(testutil.LogExport(t))
	require.NoError(t, err)
	out["logs/"+collector.Name] = collector.Bytes
	reportManifests, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)
	out["report"] = reportManifests
	removeMember, err := etcd.RemoveMemberJob(etcd.RemoveMember{Hostname: "shop-node-nbg1", ToolsImage: testutil.ToolsImage})
	require.NoError(t, err)
	out["etcd remove-member"] = removeMember
	initJob, err := openbaoinit.Manifests(testutil.InitInputs(t))
	require.NoError(t, err)
	out["openbao init"] = initJob
	return out
}

// podSpecs finds every pod spec in a document, as the map that holds its containers, by its path.
func podSpecs(node any, path string, found map[string]map[string]any) {
	switch n := node.(type) {
	case map[string]any:
		if _, ok := n["containers"]; ok {
			found[path] = n
		}
		for k, v := range n {
			podSpecs(v, path+"."+k, found)
		}
	case []any:
		for i, v := range n {
			podSpecs(v, fmt.Sprintf("%s[%d]", path, i), found)
		}
	}
}

func toleratesControlPlane(spec map[string]any) bool {
	tolerations, _ := spec["tolerations"].([]any)
	for _, raw := range tolerations {
		tol, _ := raw.(map[string]any)
		key, _ := tol["key"].(string)
		operator, _ := tol["operator"].(string)
		effect, _ := tol["effect"].(string)
		value, _ := tol["value"].(string)
		keyMatches := key == controlPlaneRole || (key == "" && operator == "Exists")
		valueMatches := operator == "Exists" || value == ""
		if keyMatches && valueMatches && (effect == "" || effect == "NoSchedule") {
			return true
		}
	}
	return false
}

func TestManifests_EveryPodThatSelectsControlPlaneNodesToleratesTheirNoScheduleTaint(t *testing.T) {
	var selecting []string

	for source, manifest := range renderedManifests(t) {
		for _, doc := range testutil.Documents(t, manifest) {
			found := map[string]map[string]any{}
			podSpecs(doc, "", found)
			for path, spec := range found {
				selector, _ := spec["nodeSelector"].(map[string]any)
				if _, ok := selector[controlPlaneRole]; !ok {
					continue
				}
				where := fmt.Sprintf("%s: %v %v%s", source, doc["kind"], testutil.Field(t, doc, "metadata", "name"), path)
				selecting = append(selecting, where)
				assert.True(t, toleratesControlPlane(spec),
					"%s selects control-plane nodes but tolerates no %s:NoSchedule, so it never schedules on a cluster that keeps apps on its workers", where, controlPlaneRole)
			}
		}
	}

	assert.Contains(t, selecting, "report: CronJob mitstack-node-report.spec.jobTemplate.spec.template.spec", "the pods that select control-plane nodes")
	assert.Contains(t, selecting, "etcd remove-member: Job mitstack-etcd-remove-shop-node-nbg1.spec.template.spec", "the pods that select control-plane nodes")
}
