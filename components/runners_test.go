// The optional runner services a cluster takes: ARC's controller, the runners' namespace and policy, and the scale set.
package components_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	set, err := components.Default().WithRunners(runnerInputs(options...))
	require.NoError(t, err)
	return set
}

func inSet(t *testing.T, set components.Set, name string) components.Manifest {
	t.Helper()
	return testutil.Manifest(t, name, set.Served, set.Inline)
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
	scaleSet := testutil.Named(t, testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline).Bytes), "AutoscalingRunnerSet", "mitstack-ci")

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
	scaleSet := testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline)

	doc := testutil.Named(t, testutil.Documents(t, scaleSet.Bytes), "AutoscalingRunnerSet", "mitstack-ci")

	assert.Equal(t, components.RunnerTokenSecret, testutil.Field(t, doc, "spec", "githubConfigSecret"), "the Secret the controller reads, named and never inlined")
	assert.Equal(t, "mitstack-runner-token", components.RunnerTokenSecret)
	for _, secret := range []string{"PRIVATE KEY", "github_app_private_key", "ghp_", "github_pat_"} {
		assert.NotContains(t, string(scaleSet.Bytes), secret, "the scale set holds no GitHub credential")
	}
}

func TestWithRunners_RunsEachJobInADindPodWithNoServiceAccountTokenAndOnlyEmptyDirVolumes(t *testing.T) {
	set := runnerSet(t)
	manifest := testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline)
	docs := testutil.Documents(t, manifest.Bytes)
	scaleSet := testutil.Named(t, docs, "AutoscalingRunnerSet", "mitstack-ci")

	pod := testutil.Field(t, scaleSet, "spec", "template", "spec")
	labels := testutil.Field(t, scaleSet, "spec", "template", "metadata", "labels").(map[string]any)
	byName := map[string]map[string]any{}
	for _, list := range []string{"initContainers", "containers"} {
		for _, c := range testutil.Field(t, pod, list).([]any) {
			byName[testutil.Field(t, c, "name").(string)] = c.(map[string]any)
		}
	}

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

func TestWithRunners_KeepsTheListenerOffBurstMembersAndLabelsIt(t *testing.T) {
	set := runnerSet(t)
	scaleSet := testutil.Named(t, testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline).Bytes), "AutoscalingRunnerSet", "mitstack-ci")

	listener := testutil.Field(t, scaleSet, "spec", "listenerTemplate")

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

func TestDefault_HoldsNoRunnerManifest(t *testing.T) {
	names := manifestNames(components.Default())

	for _, name := range []string{"mitstack-runners", "gha-runner-scale-set-controller", "mitstack-runner-scale-set"} {
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

func TestWithRunnerServices_OpensTheAPIServerAndTheInternetToThePodsOfTheRunnersNamespaceAlone(t *testing.T) {
	set := components.Default().WithRunnerServices()
	var policy map[string]any
	for _, doc := range testutil.Documents(t, inSet(t, set, "mitstack-runners").Bytes) {
		if strings.HasPrefix(doc["kind"].(string), "Cilium") {
			policy = doc
		}
	}
	require.NotNil(t, policy, "the runners' Cilium policy")
	pods := map[string]testutil.CiliumEndpoint{
		"a runner pod": testutil.Endpoint(components.RunnersNamespace, nil, nil),
		"an app pod":   testutil.Endpoint("app-shop", nil, nil),
	}

	runner := testutil.Grants(t, policy, pods, "a runner pod")
	app := testutil.Grants(t, policy, pods, "an app pod")

	assert.Contains(t, runner, "egress to entity kube-apiserver", "ARC's controller and listener reach the API server")
	assert.Contains(t, runner, "egress to entity world", "a job reaches the internet and GitHub")
	assert.Empty(t, app, "the policy opens nothing beyond the runners' namespace")
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
			_, setErr := components.Default().WithRunners(in)

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
