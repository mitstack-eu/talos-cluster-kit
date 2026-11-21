package talosclusterkit

// Every pod the kit renders that selects control-plane nodes tolerates their NoSchedule taint, which they keep when a
// cluster runs its apps on workers alone.

import (
	"bytes"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/internal/redundanttest"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
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
	telemetry, err := components.Default().TelemetryCollector(testutil.TelemetryExport(), "sub_0123456789ab")
	require.NoError(t, err)
	for _, m := range telemetry {
		out["telemetry/"+m.Name] = m.Bytes
	}
	coreDNS, err := components.CoreDNS([]string{"fsn1", "nbg1", "hel1"})
	require.NoError(t, err)
	out["coredns"] = coreDNS[0].Bytes
	reportManifests, err := report.Manifests(report.Inputs{URL: "https://api.mitstack.dev/v1/cluster-reports", SubscriptionID: "sub_0123456789ab",
		ToolsImage: testutil.ToolsImage, ProofScript: "echo report-proof\n"})
	require.NoError(t, err)
	out["report"] = reportManifests
	began := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	healthy := report.Read{At: began, Body: []byte(`{"etcd":{"members":[{"hostname":"shop-node-fsn1","healthy":true},{"hostname":"shop-node-nbg1","healthy":false},{"hostname":"shop-node-hel1","healthy":true}]}}`)}
	removeMember, err := etcd.RemoveMemberJob(etcd.RemoveMember{Hostname: "shop-node-nbg1", ToolsImage: testutil.ToolsImage, Report: healthy, StepStart: began, Guard: redundanttest.Guard})
	require.NoError(t, err)
	out["etcd remove-member"] = removeMember
	initJob, err := openbaoinit.Manifests(testutil.InitInputs(t))
	require.NoError(t, err)
	out["openbao init"] = initJob
	addControlPlane(t, out, began)
	return out
}

// The shared set's two renderings of the control plane's credentials, named by the repository credential and telemetry each takes.
const (
	tokenAndLeaseCredentials = "control plane with the platform's token and a lease/mitstack-cluster-tokens"
	deployKeyCredentials     = "control plane with a deploy key and no lease/mitstack-cluster-tokens"
)

// tokenAndLeasePlane is a plane that reads its repository with the platform's token and leases telemetry, under a
// gateway CA minted at now.
func tokenAndLeasePlane(t *testing.T, now time.Time) controlplane.Plane {
	t.Helper()
	ca, err := gateway.NewCA(now, time.Hour, []netip.Addr{netip.MustParseAddr("10.0.0.2")})
	require.NoError(t, err)
	return controlplane.Plane{Subscription: "sub_0123456789ab", PlatformURL: "https://api.mitstack.dev", RepoURL: "https://github.com/acme/cluster",
		ReportProofScript: "echo report-proof\n", GatewayCA: &ca, Telemetry: testutil.TelemetryExport()}
}

// The shared set's two renderings of the component rollout, named by the plane whose release it carries.
const (
	unhostedRollout = "rollout of the unhosted plane"
	hostingRollout  = "rollout of the hosting plane"
)

// addControlPlane adds what the control plane renders beside its node report: the topology labeller, its credentials
// twice, since the template renders the token renewal only for the platform's token and the lease only for a lease
// resource, and the rollout of both releases, since only the hosting plane's carries OpenBao.
func addControlPlane(t *testing.T, out map[string][]byte, now time.Time) {
	t.Helper()
	unhosted := tokenAndLeasePlane(t, now)
	release, err := unhosted.Release(controlplane.State{})
	require.NoError(t, err)
	out["control plane/mitstack-node-topology"] = testutil.Manifest(t, "mitstack-node-topology", release.Components).Bytes
	out[tokenAndLeaseCredentials] = testutil.Manifest(t, "mitstack-cluster-tokens", release.Components).Bytes
	addRollout(t, out, unhostedRollout, release)

	hosting := unhosted
	hosting.Hosting = &controlplane.Hosting{Repository: testutil.InitRepository}
	hosting.Telemetry = components.TelemetryExport{}
	// A hosting plane's Traefik publishes the ingress load balancer's address, so its release needs one.
	ingress := controlplane.State{LoadBalancers: map[controlplane.Role][]controlplane.LoadBalancer{
		controlplane.RoleIngress: {{Location: controlplane.GatewayLocation, PublicIPv4: "203.0.113.10"}}}}
	release, err = hosting.Release(ingress)
	require.NoError(t, err)
	out[deployKeyCredentials] = testutil.Manifest(t, "mitstack-cluster-tokens", release.Components).Bytes
	addRollout(t, out, hostingRollout, release)
}

// addRollout adds each file the rollout of release commits to the cluster repository, by its path under name, holding
// only the documents the rollout adds to those the release's components state, which the set holds already.
func addRollout(t *testing.T, out map[string][]byte, name string, release rollout.Release) {
	t.Helper()
	stated := map[string][]map[string]any{}
	for _, m := range release.Components {
		stated[rollout.ComponentsDir+"/"+m.Name+"/manifest.yaml"] = testutil.Documents(t, m.Bytes)
	}
	files, err := release.Files(testutil.InitRepository)
	require.NoError(t, err)

	for _, f := range files {
		var added bytes.Buffer
		for _, doc := range testutil.Documents(t, f.Bytes) {
			if slices.ContainsFunc(stated[f.Path], func(own map[string]any) bool { return reflect.DeepEqual(own, doc) }) {
				continue
			}
			data, err := yaml.Marshal(doc)
			require.NoError(t, err)
			added.WriteString("---\n")
			added.Write(data)
		}
		out[name+"/"+f.Path] = added.Bytes()
	}
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
	assert.Contains(t, selecting, "telemetry/mitstack-telemetry: Deployment mitstack-telemetry-cluster.spec.template.spec", "the pods that select control-plane nodes")
}
