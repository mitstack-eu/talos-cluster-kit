// The optional runner services a cluster takes: ARC's controller, the runners' namespace and policy, and the scale set.
package components_test

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	controllerImage = "ghcr.io/actions/gha-runner-scale-set-controller:0.15.1@sha256:4b51a20db717b194e2f3fa96cfe2ddcb21602811f79053c75fd1a383517b9f90"
	runnerImage     = "ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300"
	dindImage       = "docker.io/library/docker:29.10.0-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c"
)

// runnerInputs is valid inputs, changed by each option.
func runnerInputs(options ...func(*components.RunnerInputs)) components.RunnerInputs {
	in := components.RunnerInputs{Organization: "acme-org", Group: "mitstack-shared", ScaleSet: "mitstack-ci",
		RunnerImage: runnerImage, DindImage: dindImage, MaxRunners: 4, LeaseResource: "runner-token"}
	for _, option := range options {
		option(&in)
	}
	return in
}

func runnerSet(t *testing.T, options ...func(*components.RunnerInputs)) components.Set {
	t.Helper()
	set, err := components.Default().WithRunners(runnerInputs(options...), testutil.ToolsImage)
	require.NoError(t, err)
	return set
}

func inSet(t *testing.T, set components.Set, name string) components.Manifest {
	t.Helper()
	return testutil.Manifest(t, name, set.Served, set.Inline)
}

func scaleSetIn(t *testing.T, set components.Set) map[string]any {
	t.Helper()
	return testutil.Named(t, testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline).Bytes), "AutoscalingRunnerSet", "mitstack-ci")
}

func jobContainers(t *testing.T, pod any) map[string]map[string]any {
	t.Helper()
	byName := map[string]map[string]any{}
	for _, list := range []string{"initContainers", "containers"} {
		for _, c := range testutil.Field(t, pod, list).([]any) {
			byName[testutil.Field(t, c, "name").(string)] = c.(map[string]any)
		}
	}
	return byName
}

func manifestNames(set components.Set) []string {
	var names []string
	for _, m := range slices.Concat(set.Served, set.Inline) {
		names = append(names, m.Name)
	}
	return names
}

func TestWithRunners_HoldsTheControllerTheRunnersNamespaceAndTheScaleSetTheInputsDescribe(t *testing.T) {
	set := runnerSet(t)

	controller := testutil.Named(t, testutil.Documents(t, inSet(t, set, "gha-runner-scale-set-controller").Bytes), "Deployment", "arc-gha-rs-controller")
	namespace := testutil.Named(t, testutil.Documents(t, inSet(t, set, "mitstack-runners").Bytes), "Namespace", components.RunnersNamespace)
	scaleSet := scaleSetIn(t, set)

	containers := testutil.Field(t, controller, "spec", "template", "spec", "containers").([]any)
	require.Len(t, containers, 1, "the controller's containers")
	assert.Equal(t, controllerImage, testutil.Field(t, containers[0], "image"), "the controller image the kit pins")
	assert.Equal(t, components.RunnersNamespace, testutil.Field(t, controller, "metadata", "namespace"), "where the controller runs")
	assert.Equal(t, "privileged", testutil.Field(t, namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce"), "the level the dind sidecar needs")
	assert.Equal(t, components.RunnersNamespace, testutil.Field(t, scaleSet, "metadata", "namespace"), "where the scale set lives")
	assert.Equal(t, "https://github.com/acme-org", testutil.Field(t, scaleSet, "spec", "githubConfigUrl"), "the organisation the runners serve")
	assert.Equal(t, "mitstack-shared", testutil.Field(t, scaleSet, "spec", "runnerGroup"), "the group the runners register into")
	assert.Equal(t, "mitstack-ci", testutil.Field(t, scaleSet, "spec", "runnerScaleSetName"), "the label a job runs on")
	assert.EqualValues(t, 0, testutil.Field(t, scaleSet, "spec", "minRunners"), "no placeholder runner waits")
	assert.EqualValues(t, 4, testutil.Field(t, scaleSet, "spec", "maxRunners"), "the most runners at once")
}

func TestWithRunners_ReadsTheGitHubTokenFromTheLeasedSecretAndRendersNoKeyOrTokenValue(t *testing.T) {
	set := runnerSet(t)

	assert.Equal(t, components.RunnerTokenSecret, testutil.Field(t, scaleSetIn(t, set), "spec", "githubConfigSecret"), "the Secret the controller reads, named and never inlined")
	assert.Equal(t, "mitstack-runner-token", components.RunnerTokenSecret)
	for _, secret := range []string{"PRIVATE KEY", "github_app_private_key", "ghp_", "github_pat_"} {
		assert.NotContains(t, string(testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline).Bytes), secret, "the scale set holds no GitHub credential")
	}
}

func TestWithRunners_RunsEachJobInADindPodWithNoServiceAccountTokenAndOnlyEmptyDirVolumes(t *testing.T) {
	docs := testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", runnerSet(t).Inline).Bytes)
	scaleSet := testutil.Named(t, docs, "AutoscalingRunnerSet", "mitstack-ci")

	pod := testutil.Field(t, scaleSet, "spec", "template", "spec")
	labels := testutil.Field(t, scaleSet, "spec", "template", "metadata", "labels").(map[string]any)
	byName := jobContainers(t, pod)

	require.Contains(t, byName, "runner", "the runner container")
	require.Contains(t, byName, "dind", "the Docker daemon sidecar")
	assert.Equal(t, runnerImage, byName["runner"]["image"], "the runner image the operator passes")
	assert.Equal(t, dindImage, byName["dind"]["image"], "the dind image the operator passes")
	assert.Equal(t, "Always", byName["dind"]["restartPolicy"], "a native sidecar that lives as long as the runner")
	assert.Equal(t, true, testutil.Field(t, byName["dind"], "securityContext", "privileged"), "the daemon needs it")
	assert.NotEmpty(t, labels["mitstack.dev/app"], "the app-label policy refuses an unlabelled pod")
	for _, volume := range testutil.Field(t, pod, "volumes").([]any) {
		assert.Contains(t, volume, "emptyDir", "volume %v of the runner pod", volume)
		assert.NotContains(t, volume, "hostPath", "volume %v of the runner pod", volume)
	}
	account := testutil.Named(t, docs, "ServiceAccount", testutil.Field(t, pod, "serviceAccountName").(string))
	assert.Equal(t, false, account["automountServiceAccountToken"], "a job reaches no cluster token")
}

func TestWithRunners_RunsEachJobInItsOwnUserNamespaceWithAnUnprivilegedDaemonWhileTheSwitchIsOnAndAsBeforeOnceItIsOff(t *testing.T) {
	isolated := testutil.Manifest(t, "mitstack-runner-scale-set", runnerSet(t, func(in *components.RunnerInputs) { in.IsolateJobs = true }).Inline)

	testutil.Golden(t, "testdata/golden/isolated-runner-scale-set.yaml", isolated.Bytes)
	testutil.Golden(t, "testdata/golden/runner-manifests.txt", manifestSums(runnerSet(t)))
}

func TestWithRunners_HasIstioRerouteDockersDefaultAndCustomBridgesOutOfEachJobPodWithTheSwitchOffOrOnAndNoListenerInterface(t *testing.T) {
	for _, isolate := range []bool{false, true} {
		scaleSet := scaleSetIn(t, runnerSet(t, func(in *components.RunnerInputs) { in.IsolateJobs = isolate }))

		assert.Equal(t, "docker0,br-+", testutil.Field(t, scaleSet, "spec", "template", "metadata", "annotations", "istio.io/reroute-virtual-interfaces"), "the interfaces Istio sends out of a job pod with IsolateJobs %t", isolate)
		assert.Nil(t, testutil.Field(t, scaleSet, "spec", "listenerTemplate", "metadata", "annotations"), "the listener starts no Docker, so it needs no rerouted interface")
	}
}

func TestWithRunners_ReservesEachJobContainersCPUAndMemoryAsItsLimitAndWholeCoresForTheRunnerSoTheJobPodIsGuaranteed(t *testing.T) {
	containers := jobContainers(t, testutil.Field(t, scaleSetIn(t, runnerSet(t)), "spec", "template", "spec"))

	require.ElementsMatch(t, []string{"init-dind-externals", "npmrc", "dind", "runner"}, slices.Collect(maps.Keys(containers)), "the job pod's containers")
	for _, name := range slices.Sorted(maps.Keys(containers)) {
		resources, _ := containers[name]["resources"].(map[string]any)
		requests, _ := resources["requests"].(map[string]any)
		assert.Subset(t, slices.Collect(maps.Keys(requests)), []string{"cpu", "memory"}, "%s requests %v; a pod is Guaranteed only when each container requests both", name, requests)
		assert.Equal(t, requests, resources["limits"], "%s's limits; a pod is Guaranteed only when each limit equals its request", name)
	}
	cpu, err := resource.ParseQuantity(fmt.Sprint(testutil.Field(t, containers["runner"], "resources", "requests", "cpu")))
	require.NoError(t, err, "the runner's CPU")
	assert.True(t, cpu.MilliValue() > 0 && cpu.MilliValue()%1000 == 0, "the runner's CPU %s; the static CPU manager gives a container cores of its own only for whole cores", &cpu)
}

func TestWithRunners_KeepsTheListenerOffBurstMembersAndLabelsIt(t *testing.T) {
	listener := testutil.Field(t, scaleSetIn(t, runnerSet(t)), "spec", "listenerTemplate")

	terms := testutil.Field(t, listener, "spec", "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)
	require.NotEmpty(t, terms, "the listener's node selector terms")
	for _, term := range terms {
		assert.True(t, slices.ContainsFunc(testutil.Field(t, term, "matchExpressions").([]any), func(e any) bool {
			m, _ := e.(map[string]any)
			return m["key"] == components.BurstLabel && m["operator"] == "DoesNotExist"
		}), "term %v refuses a burst member", term)
	}
	assert.NotEmpty(t, testutil.Field(t, listener, "metadata", "labels", "mitstack.dev/app"), "the app-label policy refuses an unlabelled listener")
}

func TestWithRunners_GrantsTheControllerARoleToCreateTheListenersRoleAndBindingAndNamesThemForCleanup(t *testing.T) {
	docs := testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", runnerSet(t).Inline).Bytes)

	role := testutil.Named(t, docs, "Role", "mitstack-ci-gha-rs-manager")
	binding := testutil.Named(t, docs, "RoleBinding", "mitstack-ci-gha-rs-manager")
	scaleSet := testutil.Named(t, docs, "AutoscalingRunnerSet", "mitstack-ci")

	assert.Equal(t, components.RunnersNamespace, testutil.Field(t, role, "metadata", "namespace"), "the namespace the listener's Role is created in")
	for _, resource := range []string{"roles", "rolebindings"} {
		granted := false
		for _, rule := range role["rules"].([]any) {
			if slices.Contains(testutil.Field(t, rule, "resources").([]any), any(resource)) && slices.Contains(testutil.Field(t, rule, "verbs").([]any), any("create")) {
				granted = true
			}
		}
		assert.True(t, granted, "the manager Role rules %v grant create on %s", role["rules"], resource)
	}
	assert.Equal(t, components.RunnersNamespace, testutil.Field(t, binding, "metadata", "namespace"))
	assert.Equal(t, map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "mitstack-ci-gha-rs-manager"}, binding["roleRef"], "the Role the binding grants")
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "arc-gha-rs-controller", "namespace": components.RunnersNamespace}}, binding["subjects"], "the controller's ServiceAccount is who holds it")
	assert.Equal(t, "mitstack-ci-gha-rs-manager", testutil.Field(t, scaleSet, "metadata", "annotations", "actions.github.com/cleanup-manager-role-name"), "ARC removes the Role on deletion")
	assert.Equal(t, "mitstack-ci-gha-rs-manager", testutil.Field(t, scaleSet, "metadata", "annotations", "actions.github.com/cleanup-manager-role-binding"), "ARC removes the binding on deletion")
}

func TestDefault_HoldsNoRunnerManifest(t *testing.T) {
	names := manifestNames(components.Default())

	for _, name := range []string{"mitstack-runners", "gha-runner-scale-set-controller", "mitstack-runner-scale-set", components.RunnerCachesName} {
		assert.NotContains(t, names, name, "a cluster that asks for no runner services")
	}
}

func TestWithRunnerServices_HoldsTheControllerAndNamespaceButNoScaleSetAndLeavesTheSetItReadsAlone(t *testing.T) {
	base := components.Default()
	before := manifestSums(base)

	set := base.WithRunnerServices()

	names := manifestNames(set)
	assert.Contains(t, names, "mitstack-runners", "the namespace and its policy")
	assert.Contains(t, names, "gha-runner-scale-set-controller", "ARC's controller")
	assert.NotContains(t, names, "mitstack-runner-scale-set", "the scale set is what WithRunners adds")
	assert.NotContains(t, names, components.RunnerCachesName, "the caches are what WithRunners adds")
	assert.Empty(t, slices.DeleteFunc(slices.Clone(set.Inline), func(m components.Manifest) bool {
		return slices.ContainsFunc(base.Inline, func(b components.Manifest) bool { return b.Name == m.Name })
	}),
		"the services are served, which a gateway holds, not inline")
	assert.Equal(t, string(before), string(manifestSums(base)), "the set WithRunnerServices reads")
}

func TestWithRunnerServices_ExemptsTheRunnersNamespaceFromTheThreePoliciesAndKeepsTheirNames(t *testing.T) {
	base := components.Default()
	set := base.WithRunnerServices()

	for _, name := range []string{"mitstack-restrict-image-registries", "mitstack-require-resource-limits", "mitstack-disallow-host-path"} {
		t.Run(name, func(t *testing.T) {
			before := testutil.Documents(t, inSet(t, base, name).Bytes)
			variant := inSet(t, set, name)
			after := testutil.Documents(t, variant.Bytes)

			require.Len(t, after, len(before), "the policy's documents")
			for i, doc := range after {
				exempt := testutil.Field(t, doc, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any)
				require.Contains(t, exempt, components.RunnersNamespace, "document %d skips the runners' namespace", i)
				assert.Equal(t, "local-path-storage", exempt[slices.Index(exempt, any(components.RunnersNamespace))-1], "document %d lists it after the namespaces it skips already", i)
				assert.True(t, reflect.DeepEqual(before[i], withoutRunnersNamespace(doc)), "document %d differs from the base policy by more than that namespace", i)
			}
			assert.Equal(t, inSet(t, base, name).TalosName(), variant.TalosName(), "the name a node applies it under")
		})
	}
}

func withoutRunnersNamespace(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range n {
			out[k] = withoutRunnersNamespace(v)
		}
		return out
	case []any:
		var out []any
		for _, v := range n {
			if v != components.RunnersNamespace {
				out = append(out, withoutRunnersNamespace(v))
			}
		}
		return out
	}
	return node
}

func TestWithRunnerServices_RendersEveryOtherManifestOfTheDefaultSetByteForByte(t *testing.T) {
	base := components.Default()
	exempting := []string{"mitstack-restrict-image-registries", "mitstack-require-resource-limits", "mitstack-disallow-host-path"}
	set := base.WithRunnerServices()

	var changed, missing []string
	for _, m := range slices.Concat(base.Served, base.Inline) {
		if slices.Contains(exempting, m.Name) {
			continue
		}
		if got := inSet(t, set, m.Name); got.SHA256() != m.SHA256() {
			changed = append(changed, m.Name)
		}
	}
	for _, name := range manifestNames(base) {
		if !slices.Contains(manifestNames(set), name) {
			missing = append(missing, name)
		}
	}

	assert.Empty(t, changed, "manifests that differ from the default set's")
	assert.Empty(t, missing, "manifests the default set holds")
	assert.ElementsMatch(t, []string{"mitstack-runners", "gha-runner-scale-set-controller"},
		slices.DeleteFunc(manifestNames(set), func(name string) bool { return slices.Contains(manifestNames(base), name) }), "what the services add")
}

func runnersPolicy(t *testing.T) map[string]any {
	t.Helper()
	for _, doc := range testutil.Documents(t, inSet(t, components.Default().WithRunnerServices(), "mitstack-runners").Bytes) {
		if strings.HasPrefix(doc["kind"].(string), "Cilium") {
			return doc
		}
	}
	require.FailNow(t, "the runners' Cilium policy is missing")
	return nil
}

func TestWithRunnerServices_OpensTheInternetToJobPodsAloneAndTheAPIServerAndGitHubToEveryPodOfTheRunnersNamespace(t *testing.T) {
	policy := runnersPolicy(t)
	pods := map[string]testutil.CiliumEndpoint{
		"a job pod":      testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/component": "runner"}),
		"a listener pod": testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/component": "runner-scale-set-listener"}),
		"the controller": testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/name": "gha-rs-controller"}),
		"an app pod":     testutil.Endpoint("app-shop", nil, nil),
		"kube-dns":       testutil.Endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
	}
	everyPod := []string{
		"egress to entity kube-apiserver",
		"egress with kube-dns on 53/UDP 53/TCP through the DNS proxy",
		"egress to github.com on 443/TCP",
		"egress to api.github.com on 443/TCP",
		"egress to *.actions.githubusercontent.com on 443/TCP",
	}

	job := testutil.Grants(t, policy, pods, "a job pod")
	listener := testutil.Grants(t, policy, pods, "a listener pod")
	controller := testutil.Grants(t, policy, pods, "the controller")
	app := testutil.Grants(t, policy, pods, "an app pod")

	assert.ElementsMatch(t, append(slices.Clone(everyPod), "egress to entity world"), job, "what a job pod reaches")
	assert.ElementsMatch(t, everyPod, listener, "what ARC's listener reaches: no world")
	assert.ElementsMatch(t, everyPod, controller, "what ARC's controller reaches: no world")
	assert.Empty(t, app, "the policy opens nothing beyond the runners' namespace")
}

func TestWithRunnerServices_GrantsTheWorldOnlyUnderAnEndpointSelectorForJobPodsAndSelectsOnlyPodsOfTheRunnersNamespace(t *testing.T) {
	policy := runnersPolicy(t)

	specs, _ := policy["specs"].([]any)
	if spec, ok := policy["spec"]; ok {
		specs = append(specs, spec)
	}

	require.NotEmpty(t, specs, "the policy's rules")
	for i, spec := range specs {
		selector := testutil.Field(t, spec, "endpointSelector")
		assert.Equal(t, components.RunnersNamespace, testutil.Field(t, selector, "matchLabels", "k8s:io.kubernetes.pod.namespace"), "rule %d selects pods of the runners' namespace alone", i)
		egress, _ := testutil.Field(t, spec, "egress").([]any)
		for _, rule := range egress {
			entities, _ := rule.(map[string]any)["toEntities"].([]any)
			if slices.Contains(entities, any("world")) {
				assert.Equal(t, "runner", testutil.Field(t, selector, "matchLabels", "k8s:app.kubernetes.io/component"), "rule %d grants world to pods %v, not job pods alone", i, selector)
			}
		}
	}
}

func TestRunnerInputs_ValidateRefusesWhatTheScaleSetCannotRunWith(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*components.RunnerInputs)
		field  string
	}{
		{"an empty organisation", func(in *components.RunnerInputs) { in.Organization = "" }, "Organization"},
		{"an empty runner group", func(in *components.RunnerInputs) { in.Group = "" }, "Group"},
		{"an empty scale set name", func(in *components.RunnerInputs) { in.ScaleSet = "" }, "ScaleSet"},
		{"an empty runner image", func(in *components.RunnerInputs) { in.RunnerImage = "" }, "RunnerImage"},
		{"an empty dind image", func(in *components.RunnerInputs) { in.DindImage = "" }, "DindImage"},
		{"an empty lease resource", func(in *components.RunnerInputs) { in.LeaseResource = "" }, "LeaseResource"},
		{"a runner image with no digest", func(in *components.RunnerInputs) { in.RunnerImage = "ghcr.io/actions/actions-runner:2.338.2" }, "RunnerImage"},
		{"a dind image with no digest", func(in *components.RunnerInputs) { in.DindImage = "docker.io/library/docker:29.10.0-dind" }, "DindImage"},
		{"no runner allowed", func(in *components.RunnerInputs) { in.MaxRunners = 0 }, "MaxRunners"},
		{"a negative runner count", func(in *components.RunnerInputs) { in.MaxRunners = -3 }, "MaxRunners"},
		{"a scale set name that is not a DNS label", func(in *components.RunnerInputs) { in.ScaleSet = "Mitstack_CI" }, "ScaleSet"},
		{"a runner group that is not a DNS label", func(in *components.RunnerInputs) { in.Group = "shared runners" }, "Group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runnerInputs(tc.change)

			err := in.Validate()
			_, setErr := components.Default().WithRunners(in, testutil.ToolsImage)

			assert.ErrorContains(t, err, tc.field, "Validate(%+v)", in)
			assert.Error(t, setErr, "WithRunners(%+v) builds a set from refused inputs", in)
		})
	}
}

func TestRunnerInputs_ValidateAcceptsInputsEachFieldOfWhichIsSet(t *testing.T) {
	in := runnerInputs()

	assert.NoError(t, in.Validate(), "Validate(%+v)", in)
}

func TestRunnerInputs_ImagesNamesTheRunnerAndDindImagesForTheMirror(t *testing.T) {
	in := runnerInputs()

	images, err := in.Images()

	require.NoError(t, err)
	var refs []string
	for _, image := range images {
		refs = append(refs, image.Ref())
	}
	assert.ElementsMatch(t, []string{runnerImage, dindImage}, refs, "the images a runner pod starts")
}

func TestWithRunners_MirrorsTheControllerTheRunnerAndTheDindImage(t *testing.T) {
	services, err := mirror.List(components.Default().WithRunnerServices())
	require.NoError(t, err)
	both, err := mirror.List(runnerSet(t))
	require.NoError(t, err)

	var withServices, withScaleSet []string
	for _, image := range services {
		withServices = append(withServices, image.Ref())
	}
	for _, image := range both {
		withScaleSet = append(withScaleSet, image.Ref())
	}

	assert.Contains(t, withServices, controllerImage, "the mirror holds the controller image")
	assert.Subset(t, withScaleSet, []string{controllerImage, runnerImage, dindImage}, "the mirror holds each image of the runner services")
}
