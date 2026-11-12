package rollout_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
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
	return rollout.Release{ToolsImage: testutil.ToolsImage, Components: []components.Manifest{
		components.HostingPolicies()[0], nodeReport(t, "https://api.mitstack.dev/v1/cluster-reports"), defaultManifest(t, "local-path-provisioner-namespace"),
	}}
}

// secondRelease changes the report's endpoint, adds the database slices' flows and drops local-path's namespace.
func secondRelease(t *testing.T) rollout.Release {
	t.Helper()
	return rollout.Release{ToolsImage: testutil.ToolsImage, Components: []components.Manifest{
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
		{"a handover Job that has not succeeded, which Argo CD lists until it does", string(edited(t, func(app map[string]any) {
			app["resources"] = append(app["resources"].([]any), map[string]any{"group": "batch", "kind": "Job", "namespace": "argocd",
				"name": "component-mitstack-node-report-handover", "sync": "Unknown"})
		})), "component-mitstack-node-report's handover Job argocd/component-mitstack-node-report-handover has not succeeded, so its objects may still hold a field Talos owns"},
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
	assert.Equal(t, true, testutil.Field(t, app, "spec", "syncPolicy", "retry", "refresh"), "a retry takes the newest commit, so a later release reaches a component whose sync keeps failing")
	assert.Error(t, refused, "a repository that is not owner/name")
}

func TestRelease_CommitsNoSecretsDataNoJobAndNoHookOfAComponentAndCarriesTheRestOfItsManifest(t *testing.T) {
	mixed := components.Manifest{Name: "mitstack-cluster-tokens", Bytes: []byte(strings.Join([]string{
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: mitstack-enrolment\n  namespace: mitstack-node\nstringData:\n  token: \"spent once\"\n",
		"apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: mitstack-repo-token\n  namespace: mitstack-node\n",
		"apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: mitstack-enrolment\n  namespace: mitstack-node\nspec: {}\n",
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: redis-secret-init\n  namespace: argocd\n  annotations:\n    \"helm.sh/hook\": pre-install,pre-upgrade\n",
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: argocd-secret\n  namespace: argocd\ntype: Opaque\n",
	}, "---\n"))}
	keyOnly := components.Manifest{Name: "mitstack-openbao-seal-key", Bytes: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: seal\n  namespace: openbao\ndata:\n  key: c2VhbA==\n")}

	files, err := rollout.Release{ToolsImage: testutil.ToolsImage, Components: []components.Manifest{mixed, keyOnly}}.Files(repository)

	require.NoError(t, err)
	require.Len(t, files, 2, "the files a block commits: the mixed manifest's carried part and its Application, and nothing of the key")
	var kept []string
	for _, doc := range testutil.Documents(t, files[0].Bytes) {
		kept = append(kept, doc["kind"].(string)+" "+testutil.Field(t, doc, "metadata", "name").(string))
		assert.Nil(t, doc["data"], "the data of %s", doc["kind"])
		assert.Nil(t, doc["stringData"], "the string data of %s", doc["kind"])
	}
	require.Len(t, kept, 4, "the documents carried: %v", kept)
	assert.Equal(t, []string{"ServiceAccount mitstack-repo-token", "Secret argocd-secret"}, kept[:2], "the component's documents carried")
	assert.Equal(t, "Job component-mitstack-cluster-tokens-handover", kept[2], "the carriage's own handover Job, the one Job and the one hook a file holds")
	assert.Regexp(t, `^ConfigMap component-mitstack-cluster-tokens-[0-9a-f]{12}$`, kept[3], "the marker that closes the file")
	assert.NotContains(t, string(files[0].Bytes), "spent once", "the enrolment token")
}

func TestRelease_RetiresOnlyTheFilesTheCarriageCommittedThatTheReleaseNoLongerStates(t *testing.T) {
	held := []string{
		"README.md", "cluster/README.md", "cluster/app-shop-application.json", "repository-credentials/app-shop-repository-secret.json",
		"openbao-init.asc", "cluster/mitstack-hosting.yaml", "hosting/policies.yaml",
		"cluster/component-mitstack-node-report.yaml", "components/mitstack-node-report/manifest.yaml",
		"cluster/component-local-path-provisioner-namespace.yaml", "components/local-path-provisioner-namespace/manifest.yaml",
		"components/README.md", "components/shop/values.yaml", "components/shop/base/manifest.yaml", "cluster/component-shop.json",
	}

	retired, err := secondRelease(t).Retired(held)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"cluster/mitstack-hosting.yaml", "hosting/policies.yaml",
		"cluster/component-local-path-provisioner-namespace.yaml", "components/local-path-provisioner-namespace/manifest.yaml",
	}, retired, "the files the commit takes out")
}

func TestRelease_RefusesAComponentWhoseNameCannotNameAnApplicationOrItsHandoverJob(t *testing.T) {
	for _, tc := range []struct{ name, component string }{
		{"a name that is no Kubernetes label", "Not_A_Label"},
		{"a name that leaves no room for the handover Job's suffix", strings.Repeat("a", 50)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := rollout.Release{ToolsImage: testutil.ToolsImage, Components: []components.Manifest{
				{Name: tc.component, Bytes: []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n")}}}

			_, err := release.Files(repository)

			assert.ErrorContains(t, err, tc.component, "the refusal names the component")
		})
	}
}

func TestRelease_RefusesAToolsImageThatIsNotNamedByDigest(t *testing.T) {
	for _, image := range []string{"", "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.60.0"} {
		release := firstRelease(t)
		release.ToolsImage = image

		_, err := release.Files(repository)

		assert.ErrorContains(t, err, "tools image", "Files with the tools image %q, which the handover Job would run", image)
	}
}

// handoverJob is the one Job a component's file holds.
func handoverJob(t *testing.T, manifest []byte) map[string]any {
	t.Helper()
	jobs := testutil.Kind(testutil.Documents(t, manifest), "Job")
	require.Len(t, jobs, 1, "the Jobs a component's file holds")
	return jobs[0]
}

func handoverContainer(t *testing.T, job map[string]any) any {
	t.Helper()
	return testutil.Field(t, job, "spec", "template", "spec", "containers", "0")
}

// reportFile is the node report component's file of the first release.
func reportFile(t *testing.T) []byte {
	t.Helper()
	files, err := firstRelease(t).Files(repository)
	require.NoError(t, err)
	require.Equal(t, "components/mitstack-node-report/manifest.yaml", files[1].Path)
	return files[1].Bytes
}

func TestRelease_EndsEachComponentsSyncWithAJobThatNamesEveryObjectTheComponentStates(t *testing.T) {
	file := reportFile(t)
	docs := testutil.Documents(t, file)
	var want []any
	for _, doc := range docs[:len(docs)-1] {
		if doc["kind"] == "Job" {
			continue
		}
		metadata := map[string]any{"name": testutil.Field(t, doc, "metadata", "name")}
		if namespace := testutil.Field(t, doc, "metadata", "namespace"); namespace != nil {
			metadata["namespace"] = namespace
		}
		want = append(want, map[string]any{"apiVersion": doc["apiVersion"], "kind": doc["kind"], "metadata": metadata})
	}
	require.Greater(t, len(want), 5, "the objects the report's component states")

	job := handoverJob(t, file)
	env := testutil.ContainerEnv(t, handoverContainer(t, job))

	assert.Equal(t, "component-mitstack-node-report-handover", testutil.Field(t, job, "metadata", "name"))
	assert.Equal(t, "PostSync", testutil.Field(t, job, "metadata", "annotations", "argocd.argoproj.io/hook"),
		"Argo CD runs the Job once the sync applied the component's objects and they read Healthy")
	assert.Equal(t, "BeforeHookCreation,HookSucceeded", testutil.Field(t, job, "metadata", "annotations", "argocd.argoproj.io/hook-delete-policy"),
		"a Job that succeeded goes, so the Application lists one only while a handover is unfinished")
	var objects map[string]any
	require.NoError(t, json.Unmarshal([]byte(env["HANDOVER_OBJECTS"]), &objects), "HANDOVER_OBJECTS: %s", env["HANDOVER_OBJECTS"])
	assert.Equal(t, "List", objects["kind"])
	assert.Equal(t, want, objects["items"], "the objects the Job reads, which are the component's and never the marker Argo CD alone creates")
	assert.Equal(t, constants.KubernetesFieldManagerName, env["HANDOVER_TALOS_MANAGER"], "the field manager Talos creates an object under")
	assert.Equal(t, "argocd-controller", env["HANDOVER_ARGOCD_MANAGER"], "the field manager Argo CD applies under")
}

func TestRelease_NamesItsMarkerForTheHandoverJobToo(t *testing.T) {
	release := firstRelease(t)
	moved := firstRelease(t)
	moved.ToolsImage = strings.Replace(testutil.ToolsImage, "v0.1.0", "v0.2.0", 1)
	marker := func(r rollout.Release) any {
		files, err := r.Files(repository)
		require.NoError(t, err)
		docs := testutil.Documents(t, files[1].Bytes)
		return testutil.Field(t, docs[len(docs)-1], "metadata", "name")
	}

	assert.NotEqual(t, marker(release), marker(moved),
		"the marker of a file whose handover Job changed, so each Application syncs once and runs the Job a release changes")
}

func TestRelease_RunsTheHandoverInTheToolsImageAsArgoCDsApplicationControllerWithoutRootOrAWritableRoot(t *testing.T) {
	job := handoverJob(t, reportFile(t))
	pod := testutil.Field(t, job, "spec", "template", "spec")
	c := handoverContainer(t, job)

	assert.Equal(t, "argocd", testutil.Field(t, job, "metadata", "namespace"))
	assert.Equal(t, "argocd-application-controller", testutil.Field(t, pod, "serviceAccountName"),
		"the one identity that applies the component's objects, so the cluster gains no other that may patch them")
	assert.Equal(t, "Never", testutil.Field(t, pod, "restartPolicy"))
	assert.Equal(t, 0, testutil.Field(t, job, "spec", "backoffLimit"), "Argo CD's own retry runs a failed handover again")
	assert.Equal(t, testutil.ToolsImage, testutil.Field(t, c, "image"))
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/handover.sh"}, testutil.Field(t, c, "command"), "the Job runs the script the tools image carries")
	assert.Equal(t, true, testutil.Field(t, pod, "securityContext", "runAsNonRoot"))
	assert.Equal(t, true, testutil.Field(t, c, "securityContext", "readOnlyRootFilesystem"))
	assert.Equal(t, false, testutil.Field(t, c, "securityContext", "allowPrivilegeEscalation"))
	assert.Equal(t, []any{"ALL"}, testutil.Field(t, c, "securityContext", "capabilities", "drop"))
}

func TestRelease_TheKitsArgoCDGivesItsApplicationControllerEveryRightTheHandoverUses(t *testing.T) {
	docs := testutil.Documents(t, testutil.Manifest(t, "argocd", components.Default().Served).Bytes)
	pod := testutil.Field(t, handoverJob(t, reportFile(t)), "spec", "template", "spec")
	subject := map[string]any{"kind": "ServiceAccount", "name": testutil.Field(t, pod, "serviceAccountName"), "namespace": "argocd"}

	var role any
	for _, binding := range testutil.Kind(docs, "ClusterRoleBinding") {
		if slices.ContainsFunc(testutil.Field(t, binding, "subjects").([]any), func(s any) bool { return assert.ObjectsAreEqual(subject, s) }) {
			role = testutil.Field(t, binding, "roleRef", "name")
		}
	}

	require.NotNil(t, role, "the kit's Argo CD binds no ClusterRole to %v", subject)
	roles := slices.DeleteFunc(testutil.Kind(docs, "ClusterRole"), func(r map[string]any) bool { return testutil.Field(t, r, "metadata", "name") != role })
	require.Len(t, roles, 1, "the ClusterRole %v", role)
	assert.Contains(t, roles[0]["rules"], map[string]any{"apiGroups": []any{"*"}, "resources": []any{"*"}, "verbs": []any{"*"}},
		"the handover reads and server-side applies every kind a component states, a ClusterRole's rules among them")
}

const fakeKubectl = `case "$1" in
get)
  read_count=$(cat "$FAKE_DIR/reads" 2> /dev/null || echo 0)
  echo $((read_count + 1)) > "$FAKE_DIR/reads"
  [ "${FAKE_GET_EXIT:-0}" = 0 ] || exit "$FAKE_GET_EXIT"
  if [ "$read_count" = 0 ]; then cat "$FAKE_DIR/live.json"; else cat "$FAKE_DIR/after.json"; fi
  ;;
apply)
  for last; do :; done
  cp "$last" "$FAKE_DIR/applied.json"
  exit "${FAKE_APPLY_EXIT:-0}"
  ;;
esac`

// live is an object as the API serves it with its managed fields, each manager having applied it server-side.
func live(apiVersion, kind, namespace, name string, managers ...string) map[string]any {
	metadata := map[string]any{"name": name, "resourceVersion": "7"}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	var managed []any
	for _, m := range managers {
		managed = append(managed, map[string]any{"manager": m, "operation": "Apply", "apiVersion": apiVersion, "fieldsType": "FieldsV1"})
	}
	metadata["managedFields"] = managed
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata, "spec": map[string]any{"kept": true}}
}

func list(t *testing.T, objects ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objects})
	require.NoError(t, err)
	return data
}

type handoverRun struct {
	calls   []string
	applied []byte
	out     string
	err     error
}

// runHandover runs the script the report component's Job names, with its environment, against a cluster that serves
// before on the first read and after on a later one.
func runHandover(t *testing.T, before, after []byte, env ...string) handoverRun {
	t.Helper()
	c := handoverContainer(t, handoverJob(t, reportFile(t)))
	script, err := os.ReadFile(filepath.Join("scripts", "handover.sh"))
	require.NoError(t, err)
	fakes := testutil.NewTools(t, map[string]string{"kubectl": fakeKubectl})
	require.NoError(t, os.WriteFile(filepath.Join(fakes.Dir, "live.json"), before, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(fakes.Dir, "after.json"), after, 0o644))
	vars := []string{"FAKE_DIR=" + fakes.Dir}
	for name, value := range testutil.ContainerEnv(t, c) {
		vars = append(vars, name+"="+value)
	}

	out, err := testutil.RunBash(t, string(script), fakes.Env(append(vars, env...)...))

	applied, _ := os.ReadFile(filepath.Join(fakes.Dir, "applied.json"))
	return handoverRun{calls: fakes.Calls(t), applied: applied, out: out, err: err}
}

var (
	bothApplied = live("batch/v1", "CronJob", "mitstack-node", "mitstack-node-report", "talos", "argocd-controller")
	roleApplied = live("rbac.authorization.k8s.io/v1", "ClusterRole", "", "mitstack-node-report", "argocd-controller", "talos")
	argoCDOnly  = live("v1", "ServiceAccount", "mitstack-node", "mitstack-node-report", "argocd-controller")
	talosOnly   = live("v1", "ConfigMap", "mitstack-node", "mitstack-node-report", "talos")
)

func TestHandover_GivesUpTalossFieldsOnEachObjectTalosAndArgoCDBothAppliedAndOnNoOther(t *testing.T) {
	handedOver := list(t, live("batch/v1", "CronJob", "mitstack-node", "mitstack-node-report", "argocd-controller"),
		live("rbac.authorization.k8s.io/v1", "ClusterRole", "", "mitstack-node-report", "argocd-controller"))

	run := runHandover(t, list(t, bothApplied, argoCDOnly, roleApplied), handedOver)

	require.NoError(t, run.err, run.out)
	require.Len(t, run.calls, 3, "the calls: %v", run.calls)
	assert.Regexp(t, `^kubectl get -f \S+ --show-managed-fields -o json$`, run.calls[0], "the read of every object the component states")
	assert.Regexp(t, `^kubectl apply --server-side --validate=false --field-manager=talos -f \S+$`, run.calls[1],
		"an apply under Talos's name with no force, which states no field and so gives up each field Talos owns")
	assert.Regexp(t, `^kubectl get -f \S+ --show-managed-fields -o json$`, run.calls[2], "the read that checks the handover took")
	assert.JSONEq(t, `{"apiVersion": "v1", "kind": "List", "items": [
		{"apiVersion": "batch/v1", "kind": "CronJob", "metadata": {"name": "mitstack-node-report", "namespace": "mitstack-node"}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": {"name": "mitstack-node-report"}}]}`,
		string(run.applied), "what the apply states: each object's identity and nothing else")
	assert.Contains(t, run.out, "CronJob mitstack-node/mitstack-node-report", "the report of what it handed over")
	assert.Contains(t, run.out, "ClusterRole mitstack-node-report", "the report of what it handed over")
}

func TestHandover_AppliesNothingWhenNoObjectNamesTalos(t *testing.T) {
	run := runHandover(t, list(t, argoCDOnly), nil)

	require.NoError(t, run.err, run.out)
	assert.Len(t, run.calls, 1, "the calls, a read alone: %v", run.calls)
}

func TestHandover_ReadsOneObjectAsKubectlPrintsItOutsideAList(t *testing.T) {
	one, err := json.Marshal(bothApplied)
	require.NoError(t, err)
	after, err := json.Marshal(live("batch/v1", "CronJob", "mitstack-node", "mitstack-node-report", "argocd-controller"))
	require.NoError(t, err)

	run := runHandover(t, one, after)

	require.NoError(t, run.err, run.out)
	assert.Contains(t, string(run.applied), `"CronJob"`, "what the apply states")
}

func TestHandover_FailsNamingWhatItCouldNotHandOver(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after []byte
		env           []string
		says          string
		applies       bool
	}{
		{"an object Talos created that Argo CD has not applied, whose every field the apply would take away",
			list(t, bothApplied, talosOnly), nil, nil, "ConfigMap mitstack-node/mitstack-node-report", false},
		{"objects that cannot be read", list(t, bothApplied), nil, []string{"FAKE_GET_EXIT=1"}, "cannot read", false},
		{"an apply the API server refuses", list(t, bothApplied), nil, []string{"FAKE_APPLY_EXIT=1"}, "refused", true},
		{"an object that still names Talos after the apply", list(t, bothApplied), list(t, bothApplied), nil, "CronJob mitstack-node/mitstack-node-report still", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runHandover(t, tc.before, tc.after, tc.env...)

			require.Error(t, run.err, run.out)
			assert.Contains(t, run.out, tc.says, "what the handover printed")
			assert.Equal(t, tc.applies, strings.Contains(strings.Join(run.calls, "\n"), "kubectl apply"), "whether it applied: %v", run.calls)
		})
	}
}

func TestRelease_ReadsTheSSMPolicyBackFromAClusterBuiltOnV0320(t *testing.T) {
	built, err := os.ReadFile("testdata/v0.32.0-flows.yaml")
	require.NoError(t, err)
	c := testutil.BuildCluster(t, built)
	release := rollout.Release{ToolsImage: testutil.ToolsImage, Components: components.HostingPolicies()}

	upgrade(t, c, release)
	c.Sync()
	reading := readBack(t, c, release)

	assert.Empty(t, reading.Problem, "the read-back after Argo CD synced the release")
	assert.Contains(t, reading.Applied, ssm.String(), "the objects read back")
	assert.Equal(t, stated(t, components.HostingPolicies()[0].Bytes, ssm), c.Object(ssm), "the SSM policy the cluster's API serves")
	assert.Equal(t, stated(t, components.HostingPolicies()[0].Bytes, flows), c.Object(flows), "the hosting flows, which the release changes from those v0.32.0 built")
}
