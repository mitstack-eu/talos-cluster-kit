package talosclusterkit

import (
	"fmt"
	"maps"
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
	"github.com/mitstack-eu/talos-cluster-kit/tools"
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

// renderedPod is a pod spec the kit renders in a document, or in a pod template a ConfigMap of the kit holds, with the
// manifest and the object that hold it and the namespace it starts in.
type renderedPod struct {
	where     string
	source    string
	workload  string
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
			name := fmt.Sprint(testutil.Field(t, doc, "metadata", "name"))
			pods = append(pods, renderedPod{where: fmt.Sprintf("%s: %v %s%s", source, doc["kind"], name, path), source: source, workload: name, namespace: namespace, spec: spec})
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

const digestPolicy = "mitstack-require-image-digest"

func TestManifests_NoPodOutsideTheNamespacesTheLabelPolicySkipsNamesAnImageWithoutItsDigest(t *testing.T) {
	policy := podPolicy(t, digestPolicy)
	skipped := policySkips(t, policy)
	var refused []string

	for _, pod := range renderedPods(t) {
		if slices.Contains(skipped, pod.namespace) {
			continue
		}
		if !testutil.Admits(t, policy, map[string]any{"spec": pod.spec}, map[string]any{"metadata": map[string]any{"name": pod.namespace}}) {
			refused = append(refused, fmt.Sprintf("%s in %q", pod.where, pod.namespace))
		}
	}

	slices.Sort(refused)
	assert.Empty(t, refused, "a pod the kit renders would stop at its next restart")
	assert.Equal(t, policySkips(t, podPolicy(t, "mitstack-require-app-label")), skipped, "the digest policy skips other namespaces than the label policy and Kyverno's webhooks")
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

func readmeTable(t *testing.T, header string) [][]string {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	var rows [][]string
	inTable := false
	for _, line := range strings.Split(string(readme), "\n") {
		switch {
		case strings.HasPrefix(line, header):
			inTable = true
		case inTable && !strings.HasPrefix(line, "|"):
			return rows
		case inTable && !strings.HasPrefix(line, "|---"):
			rows = append(rows, strings.Split(strings.Trim(line, "| "), " | "))
		}
	}
	return rows
}

func TestManifests_TheReadmeNamesEachSkippedNamespaceWithWhatRunsThereAndWhy(t *testing.T) {
	rows := map[string][]string{}
	for _, cells := range readmeTable(t, "| Namespace | What runs there | Why the pod policies skip it |") {
		rows[strings.Trim(cells[0], "`")] = cells[1:]
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

func podContainers(t *testing.T, pod renderedPod) map[string]map[string]any {
	t.Helper()
	containers := map[string]map[string]any{}
	for _, list := range []string{"initContainers", "containers"} {
		for _, container := range listOf(pod.spec[list]) {
			containers[fmt.Sprintf("%s/%v", pod.workload, testutil.Field(t, container, "name"))] = container.(map[string]any)
		}
	}
	return containers
}

func containerFaults(name string, container map[string]any, want []string) []string {
	context, _ := container["securityContext"].(map[string]any)
	capabilities, _ := context["capabilities"].(map[string]any)
	var added, faults []string
	for _, capability := range listOf(capabilities["add"]) {
		added = append(added, capability.(string))
	}
	slices.Sort(added)
	if context["privileged"] == true {
		faults = append(faults, name+" runs privileged")
	}
	if !slices.Contains(listOf(capabilities["drop"]), any("ALL")) {
		faults = append(faults, name+" keeps the container runtime's default capabilities, since it does not drop ALL")
	}
	if !slices.Equal(added, want) {
		faults = append(faults, fmt.Sprintf("%s adds %v, and the README records %v", name, added, want))
	}
	return faults
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

// A component set that makes a Cilium container privileged fails this test, which names the container, and a cluster
// built from either set runs Cilium with only the capabilities the README records.
func TestManifests_NoCiliumContainerRunsPrivilegedOrAddsACapabilityBeyondTheReadmesList(t *testing.T) {
	documented := map[string][]string{}
	for _, cells := range readmeTable(t, "| Workload | Container | Capabilities it adds |") {
		var capabilities []string
		for _, m := range regexp.MustCompile("`([A-Z_]+)`").FindAllStringSubmatch(cells[2], -1) {
			capabilities = append(capabilities, m[1])
		}
		slices.Sort(capabilities)
		documented[strings.Trim(cells[0], "`")+"/"+strings.Trim(cells[1], "`")] = capabilities
	}
	rendered := map[string][]string{}
	var faults []string

	for _, pod := range renderedPods(t) {
		if !strings.HasSuffix(pod.source, "/cilium") {
			continue
		}
		for name, container := range podContainers(t, pod) {
			rendered[pod.source] = append(rendered[pod.source], name)
			for _, fault := range containerFaults(name, container, documented[name]) {
				faults = append(faults, pod.source+": "+fault)
			}
		}
	}

	slices.Sort(faults)
	assert.Empty(t, strings.Join(faults, "\n"), "Cilium holds more of the node than the README accounts for")
	for _, source := range []string{"default/cilium", "hosting/cilium"} {
		assert.ElementsMatch(t, slices.Collect(maps.Keys(documented)), rendered[source], "the README's table lists other Cilium containers than %s renders", source)
	}
}

func TestManifests_EveryRefusalOpensWithTheNameOfTheRuleThePlatformLooksFor(t *testing.T) {
	for _, name := range append(slices.Clone(podRefusingPolicies), digestPolicy) {
		for _, v := range testutil.Field(t, podPolicy(t, name), "spec", "validations").([]any) {
			assert.True(t, strings.HasPrefix(testutil.Field(t, v, "message").(string), name+": "), "%s refuses with a message that does not open with its name", name)
		}
	}
}

// pinnedBusybox is the one busybox reference the local path provisioner and the mesh probe both pin.
func pinnedBusybox(t *testing.T) string {
	t.Helper()
	set := components.Default()
	var refs []string
	for _, name := range []string{"local-path-provisioner", "mitstack-mesh-probe"} {
		found := regexp.MustCompile(`docker\.io/library/busybox[^"\s]*`).FindAllString(string(testutil.Manifest(t, name, set.Served, set.Inline).Bytes), -1)
		require.NotEmpty(t, found, "the %s manifest pins no busybox image", name)
		refs = append(refs, found...)
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(refs)))
	require.Len(t, distinct, 1, "the kit's manifests pin more than one busybox reference")
	return distinct[0]
}

func TestManifests_RestrictImageRegistriesNamesExactlyTheImagesTheKitPinsForPodsInAnAppNamespace(t *testing.T) {
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
	sidecar := regexp.MustCompile(`(?m)^\s*SIDECAR_IMAGE: "?([^"\s]+)"?$`).FindStringSubmatch(string(testutil.Manifest(t, "plugin-barman-cloud", hosting.Served).Bytes))
	require.NotNil(t, sidecar, "the Barman Cloud plugin sets no sidecar image")
	want = append(want, solver[1], sidecar[1], pinnedBusybox(t))

	got := quoted(t, podPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")

	assert.ElementsMatch(t, want, got, "the images the kit pins for pods in an app namespace differ from the policy's exact list")
}

func barmanCloudPlugin(t *testing.T) (operator, service, deployment map[string]any) {
	t.Helper()
	hosting, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	plugin := testutil.Documents(t, testutil.Manifest(t, "plugin-barman-cloud", hosting.Served).Bytes)
	operator = testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "cloudnative-pg", hosting.Served).Bytes), "Deployment")[0]
	return operator, testutil.Kind(plugin, "Service")[0], testutil.Kind(plugin, "Deployment")[0]
}

func TestManifests_TheBarmanCloudPluginServesInTheOperatorsNamespaceUnderTheNameAClusterAsksForItBy(t *testing.T) {
	operator, service, _ := barmanCloudPlugin(t)

	assert.Equal(t, testutil.Field(t, operator, "metadata", "namespace"), testutil.Field(t, service, "metadata", "namespace"), "CloudNativePG reads a plugin's Service only in the namespace its operator runs in")
	assert.Equal(t, "barman-cloud.cloudnative-pg.io", testutil.Field(t, service, "metadata", "labels", "cnpg.io/pluginName"), "the name a Cluster asks for the plugin by, which the node report lists")
}

func TestManifests_TheBarmanCloudPluginsPodSetsTheLimitsThePolicyAsksOfAnAppsPodThoughItsNamespaceIsSkipped(t *testing.T) {
	_, _, deployment := barmanCloudPlugin(t)

	pod := map[string]any{"spec": testutil.Field(t, deployment, "spec", "template", "spec")}

	assert.True(t, testutil.Admits(t, podPolicy(t, "mitstack-require-resource-limits"), pod, map[string]any{"metadata": map[string]any{"name": "app-shop"}}), "the plugin's container sets no CPU or no memory limit")
}

func TestManifests_PodPoliciesJudgeWhatAPodMountsSetsAndPulls(t *testing.T) {
	const (
		mitstack = "ghcr.io/mitstack-eu/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		ecr      = "839001574838.dkr.ecr.eu-central-1.amazonaws.com/mitstack-plain@sha256:1602f9e6f55dc68469e142ae6069499a597e74d0ed5425adea9ab9400e9061ff"
		zero     = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	)
	pinned := quoted(t, podPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")
	postgres := pinned[slices.IndexFunc(pinned, func(s string) bool { return strings.Contains(s, "postgresql:") })]
	sidecarAt := slices.IndexFunc(pinned, func(s string) bool { return strings.Contains(s, "plugin-barman-cloud-sidecar:") })
	require.GreaterOrEqual(t, sidecarAt, 0, "the policy pins no Barman Cloud sidecar image")
	sidecar := pinned[sidecarAt]
	sidecarTag, _, _ := strings.Cut(sidecar, "@")
	busybox := pinnedBusybox(t)
	busyboxTag, busyboxDigest, _ := strings.Cut(busybox, "@")
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
		{"the busybox image the kit pins, by its tag and digest, is admitted", "mitstack-restrict-image-registries", pod(container(busybox, limits)), true},
		{"the same busybox image by tag alone is refused", "mitstack-restrict-image-registries", pod(container(busyboxTag, limits)), false},
		{"the same busybox tag at another digest is refused", "mitstack-restrict-image-registries", pod(container(busyboxTag+zero, limits)), false},
		{"the same busybox digest under another tag is refused", "mitstack-restrict-image-registries", pod(container("docker.io/library/busybox:1.36.1@"+busyboxDigest, limits)), false},
		{"another Docker Hub image at the busybox digest is refused", "mitstack-restrict-image-registries", pod(container("docker.io/library/alpine:3.22.0@"+busyboxDigest, limits)), false},
		{"the pinned busybox image in an init container is admitted", "mitstack-restrict-image-registries", with(pod(container(mitstack, limits)), "initContainers", []any{container(busybox, limits)}), true},
		{"the same Postgres image at another digest is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie"+zero, limits)), false},
		{"the Barman Cloud sidecar the plugin adds to a Postgres pod as an init container, by its pinned digest, is admitted", "mitstack-restrict-image-registries", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecar, limits)}), true},
		{"the same sidecar by tag alone is refused", "mitstack-restrict-image-registries", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecarTag, limits)}), false},
		{"the same sidecar at another digest is refused", "mitstack-restrict-image-registries", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecarTag+zero, limits)}), false},
		{"the plugin's own image, which runs only in the operator's namespace, is refused in an app namespace", "mitstack-restrict-image-registries", pod(container(strings.Replace(sidecar, "-sidecar:", ":", 1), limits)), false},
		{"a Postgres pod whose sidecar sets no limits is refused", "mitstack-require-resource-limits", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecar, map[string]any{})}), false},
		{"an image named by its digest alone is admitted by the digest rule", digestPolicy, pod(container(mitstack, limits)), true},
		{"an image named by its tag and its digest is admitted", digestPolicy, pod(container(busybox, limits)), true},
		{"the tools image this release pins is admitted", digestPolicy, pod(container(tools.Image, limits)), true},
		{"an image named by its tag alone is refused", digestPolicy, pod(container(busyboxTag, limits)), false},
		{"an image named with neither tag nor digest is refused", digestPolicy, pod(container("ghcr.io/mitstack-eu/app", limits)), false},
		{"an image whose digest is cut short is refused", digestPolicy, pod(container("ghcr.io/mitstack-eu/app@sha256:00000000", limits)), false},
		{"an image whose tag only reads like a digest is refused", digestPolicy, pod(container("ghcr.io/mitstack-eu/app:sha256-0000000000000000000000000000000000000000000000000000000000000000", limits)), false},
		{"an init container named by its tag is refused by the digest rule", digestPolicy, with(pod(container(mitstack, limits)), "initContainers", []any{container(busyboxTag, limits)}), false},
		{"an ephemeral container named by its tag is refused by the digest rule", digestPolicy, with(pod(container(mitstack, limits)), "ephemeralContainers", []any{container(busyboxTag, limits)}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, testutil.Admits(t, podPolicy(t, tc.policy), tc.pod, namespace))
		})
	}
}
