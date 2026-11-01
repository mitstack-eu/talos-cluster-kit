package rollout_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const repository = "acme/shop-cluster"

var (
	flows = testutil.ObjectRef{Group: "cilium.io", Kind: "CiliumClusterwideNetworkPolicy", Name: "mitstack-hosting-flows"}
	ssm   = testutil.ObjectRef{Group: "cilium.io", Kind: "CiliumNetworkPolicy", Namespace: "external-secrets", Name: "mitstack-external-secrets-ssm"}
)

func commit(t *testing.T, c *testutil.Cluster, release rollout.Release) {
	t.Helper()
	files, err := release.Files(repository)
	require.NoError(t, err)
	committed := map[string][]byte{}
	for _, f := range files {
		committed[f.Path] = f.Bytes
	}
	c.Commit(committed)
}

func readBack(t *testing.T, c *testutil.Cluster, release rollout.Release) rollout.Reading {
	t.Helper()
	reading, err := release.ReadBack(c.Report())
	require.NoError(t, err)
	return reading
}

// stated is the document release states under ref.
func stated(t *testing.T, policies []byte, ref testutil.ObjectRef) map[string]any {
	t.Helper()
	for _, doc := range testutil.Documents(t, policies) {
		if doc["kind"] == ref.Kind && testutil.Field(t, doc, "metadata", "name") == ref.Name {
			return doc
		}
	}
	require.Failf(t, "not stated", "the policies state no %s", ref)
	return nil
}

func upgradedCluster(t *testing.T) (*testutil.Cluster, []byte) {
	t.Helper()
	built, err := os.ReadFile("testdata/v0.32.0-flows.yaml")
	require.NoError(t, err)
	c := testutil.BuildCluster(t, built)
	commit(t, c, rollout.Current())
	c.Sync()
	return c, built
}

func TestRelease_UpgradesAClusterBuiltOnV0320AndReadsTheSSMPolicyBackFromItsAPI(t *testing.T) {
	c, built := upgradedCluster(t)
	release := components.HostingPolicies()[0].Bytes

	reading := readBack(t, c, rollout.Current())

	assert.Empty(t, reading.Problem, "the read-back after Argo CD synced the release")
	assert.Contains(t, reading.Applied, ssm.String(), "the objects read back")
	assert.Equal(t, stated(t, release, ssm), c.Object(ssm), "the SSM policy the cluster's API serves")
	assert.Equal(t, stated(t, built, flows), c.Object(flows), "the hosting flows, which the release leaves as v0.32.0 built them")
}

func TestRelease_ASecondUpgradeChangesOnePolicyDropsAnotherAndReadsBothBack(t *testing.T) {
	c, _ := upgradedCluster(t)
	changed := changedFlows(t)
	next := releaseOf(t, changed)
	commit(t, c, next)

	stale := readBack(t, c, next)
	c.Sync()
	reading := readBack(t, c, next)

	assert.NotEmpty(t, stale.Problem, "the read-back before Argo CD synced the release")
	assert.Empty(t, reading.Problem, "the read-back after Argo CD synced the release")
	assert.NotContains(t, reading.Applied, ssm.String(), "the objects read back")
	assert.Equal(t, changed, c.Object(flows), "the changed policy the cluster's API serves")
	assert.Nil(t, c.Object(ssm), "the dropped policy the cluster's API serves")
}

func TestRelease_ReadsNothingBackBeforeArgoCDComparedAReleaseThatOnlyChangesAPolicy(t *testing.T) {
	c, _ := upgradedCluster(t)
	next := releaseOf(t, changedFlows(t), stated(t, components.HostingPolicies()[0].Bytes, ssm))
	commit(t, c, next)

	reading := readBack(t, c, next)

	assert.Regexp(t, `ConfigMap argocd/mitstack-hosting-[0-9a-f]{12} is not listed`, reading.Problem, "the read-back of a report made at the earlier release")
}

func changedFlows(t *testing.T) map[string]any {
	t.Helper()
	changed := stated(t, components.HostingPolicies()[0].Bytes, flows)
	changed["specs"].([]any)[0].(map[string]any)["description"] = "cert-manager's flows, as a later release states them."
	return changed
}

func releaseOf(t *testing.T, docs ...map[string]any) rollout.Release {
	t.Helper()
	var policies []byte
	for _, doc := range docs {
		out, err := yaml.Marshal(doc)
		require.NoError(t, err)
		policies = append(append(policies, "---\n"...), out...)
	}
	return rollout.Release{Policies: []components.Manifest{{Name: "mitstack-hosting-flows", Bytes: policies}}}
}

// edited is a synced cluster's report with change applied to the Application's JSON.
func edited(t *testing.T, change func(app map[string]any)) []byte {
	t.Helper()
	c, _ := upgradedCluster(t)
	var report map[string]any
	require.NoError(t, json.Unmarshal(c.Report(), &report))
	for _, app := range testutil.Field(t, report, "argocd", "applications").([]any) {
		if app.(map[string]any)["name"] == rollout.Application {
			change(app.(map[string]any))
		}
	}
	body, err := json.Marshal(report)
	require.NoError(t, err)
	return body
}

func TestRelease_ReadsNothingBackFromAReportThatDoesNotHoldEachObjectSynced(t *testing.T) {
	for _, tc := range []struct {
		name, report, problem string
	}{
		{"a report that is not JSON", "{", "is not a node report"},
		{"a report without the Application", string(edited(t, func(app map[string]any) { app["name"] = "other" })), "lists no Argo CD Application mitstack-hosting"},
		{"a report too large to list what it applied", string(edited(t, func(app map[string]any) { delete(app, "resources") })), "carries no list"},
		{"an Application still progressing", string(edited(t, func(app map[string]any) { app["health"] = "Progressing" })), `health "Progressing"`},
		{"an object out of sync", string(edited(t, func(app map[string]any) {
			app["resources"].([]any)[0].(map[string]any)["sync"] = "OutOfSync"
		})), "reads OutOfSync"},
		{"an object the release does not state", string(edited(t, func(app map[string]any) {
			app["resources"] = append(app["resources"].([]any), map[string]any{"group": "", "kind": "ConfigMap", "namespace": "argocd", "name": "left", "sync": "Synced"})
		})), "ConfigMap argocd/left, which the release does not state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reading, err := rollout.Current().ReadBack([]byte(tc.report))

			require.NoError(t, err)
			assert.Contains(t, reading.Problem, tc.problem, "the read-back's problem")
			assert.Empty(t, reading.Applied, "the objects read back")
		})
	}
}

func TestRelease_CommitsThePoliciesBeforeTheApplicationThatPrunesWhatTheyDrop(t *testing.T) {
	files, err := rollout.Current().Files(repository)
	_, refused := rollout.Current().Files("not a repository")

	require.NoError(t, err)
	require.Len(t, files, 2, "the files a block commits")
	assert.Equal(t, []string{rollout.PoliciesPath, rollout.ApplicationPath}, []string{files[0].Path, files[1].Path}, "the order a block commits them in")
	assert.True(t, bytes.HasPrefix(files[0].Bytes, components.HostingPolicies()[0].Bytes), "the policies file opens with the release's flows")
	app := testutil.Documents(t, files[1].Bytes)[0]
	assert.Equal(t, components.RepositorySSHURL(repository), testutil.Field(t, app, "spec", "source", "repoURL"), "the repository Argo CD reads")
	assert.Equal(t, "hosting", testutil.Field(t, app, "spec", "source", "path"), "the directory it applies")
	assert.Equal(t, map[string]any{"prune": true, "selfHeal": true}, testutil.Field(t, app, "spec", "syncPolicy", "automated"), "how it syncs")
	assert.Nil(t, testutil.Field(t, app, "metadata").(map[string]any)["finalizers"], "a finalizer that would delete the policies with the Application")
	assert.Error(t, refused, "a repository that is not owner/name")
}
