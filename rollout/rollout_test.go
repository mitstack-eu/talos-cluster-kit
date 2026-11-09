package rollout_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const repository = "acme/shop-cluster"

var (
	flows      = testutil.ObjectRef{Group: "cilium.io", Kind: "CiliumClusterwideNetworkPolicy", Name: "mitstack-hosting-flows"}
	ssm        = testutil.ObjectRef{Group: "cilium.io", Kind: "CiliumNetworkPolicy", Namespace: "external-secrets", Name: "mitstack-external-secrets-ssm"}
	reportJob  = testutil.ObjectRef{Group: "batch", Kind: "CronJob", Namespace: "mitstack-node", Name: "mitstack-node-report"}
	reportRole = testutil.ObjectRef{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "mitstack-node-report"}
	dropped    = testutil.ObjectRef{Kind: "Namespace", Name: "local-path-storage"}
)

func defaultManifest(t *testing.T, name string) components.Manifest {
	t.Helper()
	i := slices.IndexFunc(components.Default().Inline, func(m components.Manifest) bool { return m.Name == name })
	require.GreaterOrEqual(t, i, 0, "the default set's inline manifest %s", name)
	return components.Default().Inline[i]
}

// nodeReport is the node report's manifest as a release renders it, posting to url.
func nodeReport(t *testing.T, url string) components.Manifest {
	t.Helper()
	out, err := report.Manifests(report.Inputs{URL: url, SubscriptionID: "sub_0123456789ab", ToolsImage: testutil.ToolsImage,
		ProofScript: "echo report-proof\n", GatewayCA: testutil.GatewayTLS(t).CA})
	require.NoError(t, err)
	return components.Manifest{Name: "mitstack-node-report", Path: "report/report.yaml", Bytes: out}
}

// firstRelease carries the hosting flows, the node report and local-path's namespace.
func firstRelease(t *testing.T) rollout.Release {
	t.Helper()
	return rollout.Release{Components: []components.Manifest{
		components.HostingPolicies()[0], nodeReport(t, "https://api.mitstack.dev/v1/cluster-reports"), defaultManifest(t, "local-path-provisioner-namespace"),
	}}
}

// secondRelease changes the report's endpoint, adds the database slices' flows and drops local-path's namespace.
func secondRelease(t *testing.T) rollout.Release {
	t.Helper()
	return rollout.Release{Components: []components.Manifest{
		components.HostingPolicies()[0], nodeReport(t, "https://api.mitstack.dev/v2/cluster-reports"), defaultManifest(t, "mitstack-database-slices"),
	}}
}

// builtOnV0460 is a cluster Talos built from the first release's manifests, whose repository holds what v0.46.0's
// rollout committed for the hosting policies alone, synced.
func builtOnV0460(t *testing.T) *testutil.Cluster {
	t.Helper()
	var built [][]byte
	for _, m := range firstRelease(t).Components {
		built = append(built, m.Bytes)
	}
	c := testutil.BuildCluster(t, built...)
	committed := map[string][]byte{}
	for _, path := range rollout.LegacyFiles {
		data, err := os.ReadFile(filepath.Join("testdata", "v0.46.0", path))
		require.NoError(t, err)
		committed[path] = data
	}
	c.Commit(committed)
	c.Sync()
	return c
}

// upgrade commits release as a block does, its files with what it retires taken out in the same commit.
func upgrade(t *testing.T, c *testutil.Cluster, release rollout.Release) []rollout.File {
	t.Helper()
	files, err := release.Files(repository)
	require.NoError(t, err)
	retired, err := release.Retired(c.Files())
	require.NoError(t, err)
	committed := map[string][]byte{}
	for _, path := range retired {
		committed[path] = nil
	}
	for _, f := range files {
		committed[f.Path] = f.Bytes
	}
	c.Commit(committed)
	return files
}

func readBack(t *testing.T, c *testutil.Cluster, release rollout.Release) rollout.Reading {
	t.Helper()
	reading, err := release.ReadBack(c.Report())
	require.NoError(t, err)
	return reading
}

// stated is the document a manifest states under ref.
func stated(t *testing.T, manifest []byte, ref testutil.ObjectRef) map[string]any {
	t.Helper()
	for _, doc := range testutil.Documents(t, manifest) {
		if doc["kind"] == ref.Kind && testutil.Field(t, doc, "metadata", "name") == ref.Name {
			return doc
		}
	}
	require.Failf(t, "not stated", "the manifest states no %s", ref)
	return nil
}

func applications(t *testing.T, c *testutil.Cluster) []string {
	t.Helper()
	var body struct {
		ArgoCD struct {
			Applications []struct{ Name string } `json:"applications"`
		} `json:"argocd"`
	}
	require.NoError(t, json.Unmarshal(c.Report(), &body))
	var names []string
	for _, app := range body.ArgoCD.Applications {
		names = append(names, app.Name)
	}
	return names
}

func TestRelease_UpgradesAClusterV0460BuiltGivingEachComponentItsApplicationAndTakingOutTheHostingRoutesFiles(t *testing.T) {
	c := builtOnV0460(t)
	release := firstRelease(t)
	report := release.Components[1].Bytes

	files := upgrade(t, c, release)
	c.Sync()
	reading := readBack(t, c, release)

	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	assert.ElementsMatch(t, paths, c.Files(), "the repository after the upgrade, with v0.46.0's hosting files taken out")
	assert.Empty(t, reading.Problem, "the read-back after Argo CD synced the release")
	assert.Subset(t, reading.Applied, []string{reportJob.String(), reportRole.String(), flows.String()}, "the objects read back")
	assert.Equal(t, stated(t, report, reportJob), c.Object(reportJob), "the report's CronJob the cluster's API serves")
	assert.Equal(t, stated(t, components.HostingPolicies()[0].Bytes, flows), c.Object(flows), "the hosting flows, which the release adopts")
	assert.NotContains(t, applications(t, c), "mitstack-hosting", "the Applications once v0.46.0's is pruned")
	assert.Contains(t, applications(t, c), "component-mitstack-node-report", "the Applications once the release is synced")
}

func TestRelease_ASecondUpgradeChangesTheReportAddsAComponentPrunesTheOneItDropsAndReadsTheSetBack(t *testing.T) {
	c := builtOnV0460(t)
	upgrade(t, c, firstRelease(t))
	c.Sync()
	next := secondRelease(t)
	databaseSlices := testutil.ObjectRef{Group: "cilium.io", Kind: "CiliumClusterwideNetworkPolicy", Name: "mitstack-database-slices"}

	upgrade(t, c, next)
	stale := readBack(t, c, next)
	c.Sync()
	reading := readBack(t, c, next)

	assert.NotEmpty(t, stale.Problem, "the read-back before Argo CD synced the release")
	assert.Empty(t, reading.Problem, "the read-back after Argo CD synced the release")
	assert.Equal(t, stated(t, next.Components[1].Bytes, reportJob), c.Object(reportJob), "the changed report's CronJob the cluster's API serves")
	assert.NotNil(t, c.Object(databaseSlices), "the added component's policy the cluster's API serves")
	assert.Nil(t, c.Object(dropped), "the dropped component's namespace the cluster's API serves")
	assert.NotContains(t, reading.Applied, dropped.String(), "the objects read back")
	assert.NotContains(t, applications(t, c), "component-local-path-provisioner-namespace", "the Applications once the dropped one is pruned")
}

func TestRelease_ReadsNothingBackBeforeArgoCDComparedAReleaseThatOnlyChangesAComponent(t *testing.T) {
	c := builtOnV0460(t)
	upgrade(t, c, firstRelease(t))
	c.Sync()
	next := firstRelease(t)
	next.Components[1] = nodeReport(t, "https://api.mitstack.dev/v2/cluster-reports")

	upgrade(t, c, next)
	reading := readBack(t, c, next)

	assert.Regexp(t, `component-mitstack-node-report does not list ConfigMap argocd/component-mitstack-node-report-[0-9a-f]{12}`, reading.Problem,
		"the read-back of a report made at the earlier release")
}

// edited is a synced cluster's report with change applied to the report component's Application.
func edited(t *testing.T, change func(app map[string]any)) []byte {
	t.Helper()
	c := builtOnV0460(t)
	upgrade(t, c, firstRelease(t))
	c.Sync()
	var report map[string]any
	require.NoError(t, json.Unmarshal(c.Report(), &report))
	for _, app := range testutil.Field(t, report, "argocd", "applications").([]any) {
		if app.(map[string]any)["name"] == "component-mitstack-node-report" {
			change(app.(map[string]any))
		}
	}
	body, err := json.Marshal(report)
	require.NoError(t, err)
	return body
}

func TestRelease_ReadsNothingBackFromAReportThatDoesNotHoldEachComponentsObjectsSynced(t *testing.T) {
	for _, tc := range []struct {
		name, report, problem string
	}{
		{"a report that is not JSON", "{", "is not a node report"},
		{"a report without a component's Application", string(edited(t, func(app map[string]any) { app["name"] = "other" })), "lists no Argo CD Application component-mitstack-node-report"},
		{"a report too large to list what it applied", string(edited(t, func(app map[string]any) { delete(app, "resources") })), "carries no list of the objects Argo CD's Application component-mitstack-node-report applied"},
		{"an Application still progressing", string(edited(t, func(app map[string]any) { app["health"] = "Progressing" })), `health "Progressing"`},
		{"an object out of sync", string(edited(t, func(app map[string]any) {
			app["resources"].([]any)[0].(map[string]any)["sync"] = "OutOfSync"
		})), "OutOfSync"},
		{"an object the release does not state", string(edited(t, func(app map[string]any) {
			app["resources"] = append(app["resources"].([]any), map[string]any{"group": "", "kind": "ConfigMap", "namespace": "argocd", "name": "left", "sync": "Synced"})
		})), "component-mitstack-node-report lists ConfigMap argocd/left, which the release does not state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reading, err := firstRelease(t).ReadBack([]byte(tc.report))

			require.NoError(t, err)
			assert.Contains(t, reading.Problem, tc.problem, "the read-back's problem")
			assert.Empty(t, reading.Applied, "the objects read back")
		})
	}
}

func TestRelease_CommitsEachComponentBeforeTheApplicationsThatApplyItWithServerSideApplyPruningAndItsFinalizer(t *testing.T) {
	release := firstRelease(t)

	files, err := release.Files(repository)
	_, refused := release.Files("not a repository")

	require.NoError(t, err)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	assert.Equal(t, []string{
		"components/mitstack-hosting-flows/manifest.yaml", "components/mitstack-node-report/manifest.yaml",
		"components/local-path-provisioner-namespace/manifest.yaml",
		"cluster/component-mitstack-hosting-flows.yaml", "cluster/component-mitstack-node-report.yaml",
		"cluster/component-local-path-provisioner-namespace.yaml",
	}, paths, "the files a block commits, in order")
	app := testutil.Documents(t, files[4].Bytes)[0]
	assert.Equal(t, "component-mitstack-node-report", testutil.Field(t, app, "metadata", "name"), "the Application's name")
	assert.Equal(t, []any{"resources-finalizer.argocd.argoproj.io"}, testutil.Field(t, app, "metadata", "finalizers"), "what deleting the Application deletes")
	assert.Equal(t, components.RepositorySSHURL(repository), testutil.Field(t, app, "spec", "source", "repoURL"), "the repository Argo CD reads")
	assert.Equal(t, "components/mitstack-node-report", testutil.Field(t, app, "spec", "source", "path"), "the directory it applies")
	assert.Equal(t, map[string]any{"prune": true, "selfHeal": true}, testutil.Field(t, app, "spec", "syncPolicy", "automated"), "how it syncs")
	assert.Contains(t, testutil.Field(t, app, "spec", "syncPolicy", "syncOptions"), "ServerSideApply=true", "how it applies a resource definition past 262,144 bytes")
	assert.Equal(t, -1, testutil.Field(t, app, "spec", "syncPolicy", "retry", "limit"), "how often it retries a sync another component's definition holds up")
	assert.Error(t, refused, "a repository that is not owner/name")
}

func TestRelease_CommitsNoSecretsDataNoJobAndNoHookAndCarriesTheRestOfTheirManifest(t *testing.T) {
	mixed := components.Manifest{Name: "mitstack-cluster-tokens", Bytes: []byte(strings.Join([]string{
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: mitstack-enrolment\n  namespace: mitstack-node\nstringData:\n  token: \"spent once\"\n",
		"apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: mitstack-repo-token\n  namespace: mitstack-node\n",
		"apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: mitstack-enrolment\n  namespace: mitstack-node\nspec: {}\n",
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: redis-secret-init\n  namespace: argocd\n  annotations:\n    \"helm.sh/hook\": pre-install,pre-upgrade\n",
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: argocd-secret\n  namespace: argocd\ntype: Opaque\n",
	}, "---\n"))}
	keyOnly := components.Manifest{Name: "mitstack-openbao-seal-key", Bytes: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: seal\n  namespace: openbao\ndata:\n  key: c2VhbA==\n")}

	files, err := rollout.Release{Components: []components.Manifest{mixed, keyOnly}}.Files(repository)

	require.NoError(t, err)
	require.Len(t, files, 2, "the files a block commits: the mixed manifest's carried part and its Application, and nothing of the key")
	var kept []string
	for _, doc := range testutil.Documents(t, files[0].Bytes) {
		kept = append(kept, doc["kind"].(string)+" "+testutil.Field(t, doc, "metadata", "name").(string))
		assert.Nil(t, doc["data"], "the data of %s", doc["kind"])
		assert.Nil(t, doc["stringData"], "the string data of %s", doc["kind"])
	}
	require.Len(t, kept, 3, "the documents carried: %v", kept)
	assert.Equal(t, []string{"ServiceAccount mitstack-repo-token", "Secret argocd-secret"}, kept[:2], "the documents carried")
	assert.Regexp(t, `^ConfigMap component-mitstack-cluster-tokens-[0-9a-f]{12}$`, kept[2], "the marker that closes the carried documents")
	assert.NotContains(t, string(files[0].Bytes), "spent once", "the enrolment token")
}

func TestRelease_RetiresOnlyTheFilesTheCarriageCommittedThatTheReleaseNoLongerStates(t *testing.T) {
	held := []string{
		"README.md", "cluster/README.md", "cluster/app-shop-application.json", "repository-credentials/app-shop-repository-secret.json",
		"openbao-init.asc", "cluster/mitstack-hosting.yaml", "hosting/policies.yaml",
		"cluster/component-mitstack-node-report.yaml", "components/mitstack-node-report/manifest.yaml",
		"cluster/component-local-path-provisioner-namespace.yaml", "components/local-path-provisioner-namespace/manifest.yaml",
	}

	retired, err := secondRelease(t).Retired(held)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"cluster/mitstack-hosting.yaml", "hosting/policies.yaml",
		"cluster/component-local-path-provisioner-namespace.yaml", "components/local-path-provisioner-namespace/manifest.yaml",
	}, retired, "the files the commit takes out")
}

func TestRelease_RefusesAComponentWhoseNameCannotNameAnApplication(t *testing.T) {
	release := rollout.Release{Components: []components.Manifest{{Name: "Not_A_Label", Bytes: []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n")}}}

	_, err := release.Files(repository)

	assert.ErrorContains(t, err, "Not_A_Label", "the refusal names the component")
}

func TestRelease_ReadsTheSSMPolicyBackFromAClusterBuiltOnV0320(t *testing.T) {
	built, err := os.ReadFile("testdata/v0.32.0-flows.yaml")
	require.NoError(t, err)
	c := testutil.BuildCluster(t, built)
	release := rollout.Release{Components: components.HostingPolicies()}

	upgrade(t, c, release)
	c.Sync()
	reading := readBack(t, c, release)

	assert.Empty(t, reading.Problem, "the read-back after Argo CD synced the release")
	assert.Contains(t, reading.Applied, ssm.String(), "the objects read back")
	assert.Equal(t, stated(t, components.HostingPolicies()[0].Bytes, ssm), c.Object(ssm), "the SSM policy the cluster's API serves")
	assert.Equal(t, stated(t, components.HostingPolicies()[0].Bytes, flows), c.Object(flows), "the hosting flows, which the release changes from those v0.32.0 built")
}
