package components_test

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func manifest(t *testing.T, name string) components.Manifest {
	t.Helper()
	set := components.Default()
	return testutil.Manifest(t, name, set.Served, set.Inline)
}

func namedDocument(t *testing.T, m components.Manifest, kind, name string) map[string]any {
	t.Helper()
	docs := testutil.Kind(testutil.Documents(t, m.Bytes), kind)
	i := slices.IndexFunc(docs, func(doc map[string]any) bool { return testutil.Field(t, doc, "metadata", "name") == name })
	require.GreaterOrEqual(t, i, 0, "manifest %s has no %s %s", m.Name, kind, name)
	return docs[i]
}

func TestAdmission_ExemptsTheSameSystemNamespacesInEveryPolicyAndKyvernosWebhooks(t *testing.T) {
	label := testutil.Documents(t, manifest(t, "mitstack-require-app-label").Bytes)[0]
	generate := testutil.Documents(t, manifest(t, "mitstack-generate-default-deny").Bytes)[0]
	kyvernoConfig := namedDocument(t, manifest(t, "kyverno"), "ConfigMap", "kyverno")

	labelExempt := testutil.Field(t, label, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any)
	var generateExempt []string
	for _, m := range regexp.MustCompile(`'([a-z0-9-]+)'`).FindAllStringSubmatch(testutil.Field(t, generate, "spec", "matchConditions", "0", "expression").(string), -1) {
		generateExempt = append(generateExempt, m[1])
	}
	var webhooks struct {
		NamespaceSelector struct{ MatchExpressions []struct{ Values []string } }
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.Field(t, kyvernoConfig, "data", "webhooks").(string)), &webhooks))

	assert.Equal(t, fmt.Sprint(labelExempt), fmt.Sprint(generateExempt), "the default-deny generator exempts other namespaces than the label policy")
	assert.Equal(t, fmt.Sprint(labelExempt), fmt.Sprint(webhooks.NamespaceSelector.MatchExpressions[0].Values), "Kyverno's webhooks exempt other namespaces than the label policy")
	for _, policy := range []string{"mitstack-require-mesh", "mitstack-require-strict-mtls"} {
		doc := testutil.Documents(t, manifest(t, policy).Bytes)[0]
		exempt := testutil.Field(t, doc, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values")
		assert.Equal(t, labelExempt, exempt, "%s exempts other namespaces than the label policy", policy)
	}
	assert.Subset(t, labelExempt, []string{"kube-system", "argocd", "kyverno", "cnpg-system", "istio-system", nodeconfig.TalosAPINamespace})
}

var sliceEndpoints = map[string]testutil.CiliumEndpoint{
	"instance": testutil.Endpoint("app-pg", map[string]string{"mitstack.dev/database-slice": "true", "mitstack.dev/subscription": "sub-pg"},
		map[string]string{"cnpg.io/cluster": "shared", "cnpg.io/podRole": "instance", "mitstack.dev/app": "postgres"}),
	"job": testutil.Endpoint("app-pg", map[string]string{"mitstack.dev/database-slice": "true", "mitstack.dev/subscription": "sub-pg"},
		map[string]string{"cnpg.io/cluster": "shared", "cnpg.io/jobRole": "initdb", "mitstack.dev/app": "postgres"}),
	"app": testutil.Endpoint("app-a1", map[string]string{"mitstack.dev/subscription": "sub-a1"}, map[string]string{"mitstack.dev/app": "shop"}),
	"instance outside a database slice": testutil.Endpoint("app-b2", map[string]string{"mitstack.dev/subscription": "sub-b2"},
		map[string]string{"cnpg.io/cluster": "own", "cnpg.io/podRole": "instance", "mitstack.dev/app": "own"}),
	"operator": testutil.Endpoint("cnpg-system", nil, map[string]string{"app.kubernetes.io/name": "cloudnative-pg", "mitstack.dev/app": "cloudnative-pg"}),
}

func TestDatabaseSlicesPolicy_GrantsEachPodExactlyItsAllows(t *testing.T) {
	policy := namedDocument(t, manifest(t, "mitstack-database-slices"), "CiliumClusterwideNetworkPolicy", "mitstack-database-slices")
	for pod, want := range map[string][]string{
		"instance": {
			"egress to entity kube-apiserver", "egress with instance on 5432/TCP 15008/TCP", "ingress with app on 5432/TCP 15008/TCP",
			"ingress with instance on 5432/TCP 15008/TCP", "ingress with instance outside a database slice on 5432/TCP 15008/TCP",
			"ingress with job on 5432/TCP 15008/TCP", "ingress with operator on 8000/TCP 15008/TCP",
		},
		"job":                               {"egress to entity kube-apiserver", "egress with instance on 5432/TCP 15008/TCP"},
		"app":                               {"egress with instance on 5432/TCP 15008/TCP"},
		"instance outside a database slice": {"egress with instance on 5432/TCP 15008/TCP"},
		"operator":                          nil,
	} {
		t.Run(pod, func(t *testing.T) {
			assert.Equal(t, want, testutil.Grants(t, policy, sliceEndpoints, pod))
		})
	}
}

func TestLocalPathProvisioner_LabelsEveryPodItStartsAndOpensTheAPIServerToItsNamespaceAlone(t *testing.T) {
	label := testutil.Documents(t, manifest(t, "mitstack-require-app-label").Bytes)[0]
	exempt := testutil.Field(t, label, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any)
	provisioner := manifest(t, "local-path-provisioner")
	namespace := testutil.Documents(t, manifest(t, "local-path-provisioner-namespace").Bytes)[0]
	policy := namedDocument(t, manifest(t, "mitstack-local-path-flows"), "CiliumClusterwideNetworkPolicy", "mitstack-local-path-flows")
	pods := map[string]testutil.CiliumEndpoint{"a pod in local-path-storage": testutil.Endpoint("local-path-storage", nil, nil), "an app pod": sliceEndpoints["app"]}

	template := testutil.Field(t, namedDocument(t, provisioner, "Deployment", "local-path-provisioner"), "spec", "template")
	helper := testutil.Documents(t, []byte(testutil.Field(t, namedDocument(t, provisioner, "ConfigMap", "local-path-config"), "data", "helperPod.yaml").(string)))[0]

	for pod, labels := range map[string]any{"the provisioner": testutil.Field(t, template, "metadata", "labels"), "a helper pod": testutil.Field(t, helper, "metadata", "labels")} {
		assert.NotEmpty(t, labels.(map[string]any)["mitstack.dev/app"], "the app-label policy refuses %s", pod)
	}
	assert.NotContains(t, exempt, "local-path-storage", "the app-label policy skips the namespace")
	assert.NotContains(t, string(manifest(t, "mitstack-generate-default-deny").Bytes), "local-path-storage", "the namespace gets no mitstack-default-deny")
	assert.Equal(t, "privileged", testutil.Field(t, namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce"), "Talos's default level, baseline, refuses a helper pod's hostPath volume")
	assert.Equal(t, []string{"egress to entity kube-apiserver"}, testutil.Grants(t, policy, pods, "a pod in local-path-storage"))
	assert.Empty(t, testutil.Grants(t, policy, pods, "an app pod"), "the policy opens the API server beyond local-path-storage")
}

func gitBlob(data []byte) string {
	return fmt.Sprintf("%x", sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(data)), data...)))
}

func TestCloudNativePG_IsClusterBasesV040sComponentWithPostgres16AsItsDefault(t *testing.T) {
	bases := strings.NewReplacer("# Copied from mitstack-eu/cluster-bases v0.4.0, its cloudnative-pg 0.29.1 chart render, with\n# POSTGRES_IMAGE_NAME set to the Postgres 16.15 image postgres-db-kube pins, and with the line\n# scripts/render-components.sh ends a document with when its last value is a block scalar.\n", "# Generated by scripts/render-cloudnative-pg.sh from the cloudnative-pg 0.29.1 chart.\n# Edit render/cloudnative-pg/values.yaml and render again, never this file.\n", "postgresql:16.15-system-trixie@sha256:8ca5699981c25db72e549e9fa69561bac233df15b9b1e3a6282847b0629bb5ea", "postgresql:18.6-system-trixie@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e",
		"# Talos trims each document before it decodes it, so this line keeps the value above whole.\n", "").Replace(string(manifest(t, "cloudnative-pg").Bytes))
	assert.Equal(t, "790c97885e59c3eaddb627eb9d5f6847e86f0e0e", gitBlob([]byte(bases)))
	assert.Equal(t, "e232215537d185178fe8dbf9d1b81a49351a2e06", gitBlob(manifest(t, "cloudnative-pg-namespace").Bytes))
}

var podRefusingPolicies = []string{"mitstack-disallow-host-path", "mitstack-require-resource-limits", "mitstack-restrict-image-registries"}

func admissionPolicy(t *testing.T, name string) map[string]any {
	t.Helper()
	return testutil.Documents(t, manifest(t, name).Bytes)[0]
}

func skippedNamespaces(t *testing.T, policy map[string]any) []string {
	t.Helper()
	var namespaces []string
	for _, v := range testutil.Field(t, policy, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any) {
		namespaces = append(namespaces, v.(string))
	}
	return namespaces
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

func TestAdmission_ThePodPoliciesSkipTheSameNamespacesAndNoneThatHoldsAnApp(t *testing.T) {
	label := skippedNamespaces(t, admissionPolicy(t, "mitstack-require-app-label"))
	var first []string
	for _, name := range podRefusingPolicies {
		skipped := skippedNamespaces(t, admissionPolicy(t, name))
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

func TestAdmission_TheReadmeNamesEachSkippedNamespaceWithWhatRunsThereAndWhy(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
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

	skipped := skippedNamespaces(t, admissionPolicy(t, podRefusingPolicies[0]))

	var documented []string
	for namespace, cells := range rows {
		documented = append(documented, namespace)
		require.Len(t, cells, 2, "the row for %s", namespace)
		assert.NotEmpty(t, strings.TrimSpace(cells[0]), "%s: what runs there", namespace)
		assert.NotEmpty(t, strings.TrimSpace(cells[1]), "%s: why the policies skip it", namespace)
	}
	assert.ElementsMatch(t, skipped, documented, "the README's table of skipped namespaces differs from the policies' list")
}

func TestAdmission_EveryRefusalOpensWithTheNameOfTheRuleThePlatformLooksFor(t *testing.T) {
	for _, name := range podRefusingPolicies {
		for _, v := range testutil.Field(t, admissionPolicy(t, name), "spec", "validations").([]any) {
			assert.True(t, strings.HasPrefix(testutil.Field(t, v, "message").(string), name+": "), "%s refuses with a message that does not open with its name", name)
		}
	}
}

func TestAdmission_RestrictImageRegistriesNamesExactlyTheImagesTheKitsOperatorsStart(t *testing.T) {
	cnpg := string(manifest(t, "cloudnative-pg").Bytes)
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

	got := quoted(t, admissionPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")

	assert.ElementsMatch(t, want, got, "the images an operator starts in an app namespace differ from the policy's exact list")
}

func TestAdmission_PodPoliciesJudgeWhatAPodMountsSetsAndPulls(t *testing.T) {
	const (
		mitstack = "ghcr.io/mitstack-eu/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		ecr      = "839001574838.dkr.ecr.eu-central-1.amazonaws.com/mitstack-plain@sha256:1602f9e6f55dc68469e142ae6069499a597e74d0ed5425adea9ab9400e9061ff"
		zero     = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	)
	pinned := quoted(t, admissionPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")
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
			assert.Equal(t, tc.want, testutil.Admits(t, admissionPolicy(t, tc.policy), tc.pod, namespace))
		})
	}
}
