package talosclusterkit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

type renderedDoc struct {
	doc    map[string]any
	source string
}

func (d renderedDoc) String() string {
	metadata, _ := d.doc["metadata"].(map[string]any)
	return fmt.Sprintf("%s: %v %v", d.source, d.doc["kind"], metadata["name"])
}

func renderedDocs(t *testing.T) []renderedDoc {
	t.Helper()
	var docs []renderedDoc
	for source, manifest := range renderedManifests(t) {
		for _, doc := range testutil.Documents(t, manifest) {
			docs = append(docs, renderedDoc{doc, source})
		}
	}
	return docs
}

func TestManifests_NoWebhookConfigurationFailsOpen(t *testing.T) {
	var open []string
	for _, d := range renderedDocs(t) {
		if d.doc["kind"] != "ValidatingWebhookConfiguration" && d.doc["kind"] != "MutatingWebhookConfiguration" {
			continue
		}
		for _, w := range d.doc["webhooks"].([]any) {
			if policy := w.(map[string]any)["failurePolicy"]; policy != "Fail" {
				open = append(open, fmt.Sprintf("%s webhook %v has failurePolicy %v", d, w.(map[string]any)["name"], policy))
			}
		}
	}

	slices.Sort(open)
	assert.Empty(t, open, "Argo CD compares the rendered policy, and a controller rewrites a policy of Ignore to Fail once it is ready")
}

func TestManifests_AnAutoscalerNeedsTheMetricsAPIItReadsAndIstiodRunsOneReplicaWithout(t *testing.T) {
	var autoscalers []string
	servesMetrics := false
	for _, d := range renderedDocs(t) {
		switch d.doc["kind"] {
		case "HorizontalPodAutoscaler":
			autoscalers = append(autoscalers, d.String())
		case "APIService":
			servesMetrics = servesMetrics || testutil.Field(t, d.doc, "spec", "group") == "metrics.k8s.io"
		}
	}
	istiod := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "istiod", components.Default().Served).Bytes), "Deployment")[0]

	assert.True(t, servesMetrics || len(autoscalers) == 0, "the set renders %v and registers no APIService for metrics.k8s.io, so each reads Degraded", autoscalers)
	if !servesMetrics {
		assert.EqualValues(t, 1, istiod["spec"].(map[string]any)["replicas"], "without a replica count the Deployment is left to an autoscaler the cluster cannot serve")
	}
}

func TestManifests_DeclareNoEmptyLabelsOrAnnotationsMap(t *testing.T) {
	var empty []string
	for _, d := range renderedDocs(t) {
		metadata, _ := d.doc["metadata"].(map[string]any)
		for _, key := range []string{"labels", "annotations"} {
			if m, ok := metadata[key].(map[string]any); ok && len(m) == 0 {
				empty = append(empty, fmt.Sprintf("%s declares empty %s", d, key))
			}
		}
	}

	slices.Sort(empty)
	assert.Empty(t, empty, "the API server stores no empty map, so the live object never matches the committed one")
}

func TestDropEmptyMetadataMaps(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"drops a map written on two lines", "metadata:\n  labels:\n    {}\n  annotations:\n    {}\n  name: a\n", "metadata:\n  name: a\n"},
		{"drops a map written inline in each document", "---\nmetadata:\n  labels: {}\n  name: a\n---\nmetadata:\n  name: b\n  annotations: {}\nspec: {}\n", "---\nmetadata:\n  name: a\n---\nmetadata:\n  name: b\nspec: {}\n"},
		{"keeps a filled map", "metadata:\n  labels:\n    app: a\n  annotations:\n    {}\n  name: a\n", "metadata:\n  labels:\n    app: a\n  name: a\n"},
		{"keeps an empty map below the top-level metadata", "metadata:\n  name: a\nspec:\n  labels: {}\n  template:\n    metadata:\n      labels:\n        {}\n      annotations: {}\n  properties:\n    labels: {}\ndata:\n  script: |\n    metadata:\n      labels: {}\n", ""},
		{"keeps a null map before the next document", "metadata:\n  name: a\n  labels:\n---\nkind: B\n", ""},
		{"keeps a null map at the end of the file", "metadata:\n  name: a\n  labels:\n", ""},
		{"keeps a null map before another key", "metadata:\n  annotations:\n  name: a\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "rendered.yaml")
			require.NoError(t, os.WriteFile(file, []byte(tc.in), 0o644))

			got, err := testutil.RunBash(t, `. "$1" && drop_empty_metadata_maps "$2" && cat "$2"`, os.Environ(), "scripts/lib/empty-maps.sh", file)

			require.NoError(t, err, got)
			if tc.want == "" {
				tc.want = tc.in
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

var podRefusingPolicies = []string{"mitstack-disallow-host-path", "mitstack-require-resource-limits", "mitstack-restrict-image-registries"}

// renderedPod is a pod spec the kit renders in a document, or in a pod template a ConfigMap of the kit holds, with the namespace it starts in.
type renderedPod struct {
	where     string
	namespace string
	spec      map[string]any
}

func renderedPods(t *testing.T) []renderedPod {
	t.Helper()
	var pods []renderedPod
	add := func(source string, doc map[string]any, namespace string) {
		found := map[string]map[string]any{}
		podSpecs(doc, "", found)
		for path, spec := range found {
			if _, ok := spec["containers"].([]any); !ok {
				continue
			}
			pods = append(pods, renderedPod{where: fmt.Sprintf("%s: %v %v%s", source, doc["kind"], testutil.Field(t, doc, "metadata", "name"), path), namespace: namespace, spec: spec})
		}
	}
	for _, d := range renderedDocs(t) {
		if d.doc["kind"] == "CustomResourceDefinition" {
			continue
		}
		namespace, _ := testutil.Field(t, d.doc, "metadata", "namespace").(string)
		add(d.source, d.doc, namespace)
		if data, ok := d.doc["data"].(map[string]any); ok {
			for key, value := range data {
				if text, ok := value.(string); ok && strings.Contains(text, "containers:") {
					for _, embedded := range testutil.Documents(t, []byte(text)) {
						embedded["metadata"] = map[string]any{"name": d.source + " " + key}
						add(d.source, embedded, namespace)
					}
				}
			}
		}
	}
	return pods
}

func podPolicy(t *testing.T, name string) map[string]any {
	t.Helper()
	return testutil.Documents(t, testutil.Manifest(t, name, components.Default().Inline).Bytes)[0]
}

func policySkips(t *testing.T, policy map[string]any) []string {
	t.Helper()
	var skipped []string
	for _, v := range testutil.Field(t, policy, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any) {
		skipped = append(skipped, v.(string))
	}
	return skipped
}

func refusals(t *testing.T, pod renderedPod) []string {
	t.Helper()
	var refused []string
	for _, name := range podRefusingPolicies {
		if !testutil.Admits(t, podPolicy(t, name), map[string]any{"spec": pod.spec}, map[string]any{"metadata": map[string]any{"name": pod.namespace}}) {
			refused = append(refused, name)
		}
	}
	return refused
}

func TestManifests_NoPodOutsideTheSkippedNamespacesIsRefusedByTheHostPathLimitsOrRegistryPolicies(t *testing.T) {
	skipped := policySkips(t, podPolicy(t, podRefusingPolicies[0]))
	var refused []string

	for _, pod := range renderedPods(t) {
		if slices.Contains(skipped, pod.namespace) {
			continue
		}
		for _, policy := range refusals(t, pod) {
			refused = append(refused, fmt.Sprintf("%s in %q is refused by %s", pod.where, pod.namespace, policy))
		}
	}

	slices.Sort(refused)
	assert.Empty(t, refused, "a pod the kit renders would stop at its next restart")
}

func TestManifests_EveryNamespaceTheNewPolicySkipsBeyondTheLabelPolicysHoldsAPodTheyWouldRefuse(t *testing.T) {
	label := policySkips(t, podPolicy(t, "mitstack-require-app-label"))
	refusedIn := map[string][]string{}
	for _, pod := range renderedPods(t) {
		if refused := refusals(t, pod); len(refused) > 0 {
			refusedIn[pod.namespace] = append(refusedIn[pod.namespace], pod.where)
		}
	}

	for _, namespace := range policySkips(t, podPolicy(t, podRefusingPolicies[0])) {
		if slices.Contains(label, namespace) {
			continue
		}
		assert.NotEmpty(t, refusedIn[namespace], "the policies skip %s, but no pod the kit renders there would be refused", namespace)
	}
}

func TestManifests_TheSecurityScanRunsInKubeSystemWhichEveryPodPolicySkips(t *testing.T) {
	var scan []renderedPod
	for _, pod := range renderedPods(t) {
		if strings.HasPrefix(pod.where, "report: CronJob mitstack-security-scan.") {
			scan = append(scan, pod)
		}
	}
	require.Len(t, scan, 1, "the security scan's pod spec")

	for _, name := range podRefusingPolicies {
		assert.Contains(t, policySkips(t, podPolicy(t, name)), scan[0].namespace, "%s judges the security scan's pod in %s", name, scan[0].namespace)
	}
	assert.Equal(t, "kube-system", scan[0].namespace)
}

// quoted lists the single-quoted strings of a policy variable's CEL list, in order.
func quoted(t *testing.T, policy map[string]any, variable string) []string {
	t.Helper()
	for _, v := range testutil.Field(t, policy, "spec", "variables").([]any) {
		if testutil.Field(t, v, "name") != variable {
			continue
		}
		var found []string
		for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(testutil.Field(t, v, "expression").(string), -1) {
			found = append(found, m[1])
		}
		return found
	}
	require.Failf(t, "no variable", "policy %v declares no variable %s", testutil.Field(t, policy, "metadata", "name"), variable)
	return nil
}

func TestManifests_ThePodPoliciesSkipTheSameNamespacesAndNoneThatHoldsAnApp(t *testing.T) {
	label := policySkips(t, podPolicy(t, "mitstack-require-app-label"))
	var first []string
	for _, name := range podRefusingPolicies {
		skipped := policySkips(t, podPolicy(t, name))
		if first == nil {
			first = skipped
		}

		assert.Equal(t, first, skipped, "%s skips other namespaces than %s", name, podRefusingPolicies[0])
		assert.Subset(t, skipped, label, "%s must leave alone every namespace the label policy does", name)
		for _, namespace := range skipped {
			assert.False(t, strings.HasPrefix(namespace, "app-"), "%s skips %s, which holds an app", name, namespace)
			assert.NotEqual(t, "mitstack-admission-probe", namespace, "%s skips the namespace its own fixtures are tried in", name)
		}
	}
}

func TestManifests_TheReadmeNamesEachSkippedNamespaceWithWhatRunsThereAndWhy(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	rows := map[string][]string{}
	inTable := false
	for _, line := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(line, "| Namespace | What runs there | Why the pod policies skip it |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if strings.HasPrefix(cells[0], "`") {
			rows[strings.Trim(cells[0], "`")] = cells[1:]
		}
	}

	skipped := policySkips(t, podPolicy(t, podRefusingPolicies[0]))

	var documented []string
	for namespace, cells := range rows {
		documented = append(documented, namespace)
		require.Len(t, cells, 2, "the row for %s", namespace)
		assert.NotEmpty(t, strings.TrimSpace(cells[0]), "%s: what runs there", namespace)
		assert.NotEmpty(t, strings.TrimSpace(cells[1]), "%s: why the policies skip it", namespace)
	}
	assert.ElementsMatch(t, skipped, documented, "the README's table of skipped namespaces differs from the policies' list")
}

func TestManifests_EveryRefusalOpensWithTheNameOfTheRuleThePlatformLooksFor(t *testing.T) {
	for _, name := range podRefusingPolicies {
		for _, v := range testutil.Field(t, podPolicy(t, name), "spec", "validations").([]any) {
			assert.True(t, strings.HasPrefix(testutil.Field(t, v, "message").(string), name+": "), "%s refuses with a message that does not open with its name", name)
		}
	}
}

func TestManifests_RestrictImageRegistriesNamesExactlyTheImagesTheKitsOperatorsStart(t *testing.T) {
	cnpg := string(testutil.Manifest(t, "cloudnative-pg", components.Default().Served, components.Default().Inline).Bytes)
	hosting, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	certManager := string(testutil.Manifest(t, "cert-manager", hosting.Served, hosting.Inline).Bytes)
	var want []string
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*POSTGRES_IMAGE_NAME: (\S+)$`),
		regexp.MustCompile(`(?m)^\s*PGBOUNCER_IMAGE_NAME: (\S+)$`),
		regexp.MustCompile(`(?m)^\s*- name: OPERATOR_IMAGE_NAME\n\s+value: "?([^"\s]+)"?$`),
	} {
		found := re.FindStringSubmatch(cnpg)
		require.NotNil(t, found, "the CloudNativePG manifest sets no image for %s", re)
		want = append(want, found[1])
	}
	solver := regexp.MustCompile(`--acme-http01-solver-image=(\S+)`).FindStringSubmatch(certManager)
	require.NotNil(t, solver, "cert-manager sets no solver image")
	want = append(want, solver[1])

	got := quoted(t, podPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")

	assert.ElementsMatch(t, want, got, "the images an operator starts in an app namespace differ from the policy's exact list")
}

func TestManifests_PodPoliciesJudgeWhatAPodMountsSetsAndPulls(t *testing.T) {
	const (
		mitstack = "ghcr.io/mitstack-eu/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		ecr      = "839001574838.dkr.ecr.eu-central-1.amazonaws.com/mitstack-plain@sha256:1602f9e6f55dc68469e142ae6069499a597e74d0ed5425adea9ab9400e9061ff"
		zero     = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	)
	pinned := quoted(t, podPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")
	postgres := pinned[slices.IndexFunc(pinned, func(s string) bool { return strings.Contains(s, "postgresql:") })]
	limits := map[string]any{"limits": map[string]any{"cpu": "100m", "memory": "64Mi"}}
	container := func(image string, resources map[string]any) map[string]any {
		return map[string]any{"name": "c", "image": image, "resources": resources}
	}
	pod := func(containers ...any) map[string]any {
		return map[string]any{"spec": map[string]any{"containers": containers}}
	}
	with := func(p map[string]any, field string, value any) map[string]any {
		p["spec"].(map[string]any)[field] = value
		return p
	}
	namespace := map[string]any{"metadata": map[string]any{"name": "app-shop"}}

	for _, tc := range []struct {
		name   string
		policy string
		pod    map[string]any
		want   bool
	}{
		{"a pod that mounts an emptyDir is admitted", "mitstack-disallow-host-path", with(pod(container(mitstack, limits)), "volumes", []any{map[string]any{"name": "v", "emptyDir": map[string]any{}}}), true},
		{"a pod that mounts a host path is refused", "mitstack-disallow-host-path", with(pod(container(mitstack, limits)), "volumes", []any{map[string]any{"name": "v", "hostPath": map[string]any{"path": "/var"}}}), false},
		{"a pod with no volumes is admitted by the host path rule", "mitstack-disallow-host-path", pod(container(mitstack, limits)), true},
		{"a container with a CPU and a memory limit is admitted", "mitstack-require-resource-limits", pod(container(mitstack, limits)), true},
		{"a container with no resources is refused", "mitstack-require-resource-limits", pod(container(mitstack, map[string]any{})), false},
		{"a container with a CPU limit and no memory limit is refused", "mitstack-require-resource-limits", pod(container(mitstack, map[string]any{"limits": map[string]any{"cpu": "1"}})), false},
		{"a container with requests alone is refused, since a LimitRange fills a Pod and not its Deployment", "mitstack-require-resource-limits", pod(container(mitstack, map[string]any{"requests": map[string]any{"cpu": "1", "memory": "1Gi"}})), false},
		{"an init container with no limits is refused", "mitstack-require-resource-limits", with(pod(container(mitstack, limits)), "initContainers", []any{container(mitstack, map[string]any{})}), false},
		{"an image of mitstack's ghcr.io organisation is admitted", "mitstack-restrict-image-registries", pod(container(mitstack, limits)), true},
		{"an image of mitstack's ECR registry is admitted", "mitstack-restrict-image-registries", pod(container(ecr, limits)), true},
		{"an image from Docker Hub is refused", "mitstack-restrict-image-registries", pod(container("docker.io/library/busybox"+zero, limits)), false},
		{"an image of a ghcr.io organisation that only begins with mitstack-eu is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/mitstack-eu-fork/app"+zero, limits)), false},
		{"an init container from another registry is refused", "mitstack-restrict-image-registries", with(pod(container(mitstack, limits)), "initContainers", []any{container("quay.io/x/y:1", limits)}), false},
		{"an ephemeral container from another registry is refused", "mitstack-restrict-image-registries", with(pod(container(mitstack, limits)), "ephemeralContainers", []any{container("quay.io/x/y:1", limits)}), false},
		{"the Postgres image CloudNativePG starts, by its pinned digest, is admitted", "mitstack-restrict-image-registries", pod(container(postgres, limits)), true},
		{"the same Postgres image by tag alone is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie", limits)), false},
		{"the same Postgres image at another digest is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie"+zero, limits)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, testutil.Admits(t, podPolicy(t, tc.policy), tc.pod, namespace))
		})
	}
}
