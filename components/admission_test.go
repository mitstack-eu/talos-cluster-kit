package components_test

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	sum := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(data)), data...))
	return hex.EncodeToString(sum[:])
}

func TestCloudNativePG_IsClusterBasesV040sComponentByteForByte(t *testing.T) {
	assert.Equal(t, "790c97885e59c3eaddb627eb9d5f6847e86f0e0e", gitBlob(manifest(t, "cloudnative-pg").Bytes))
	assert.Equal(t, "e232215537d185178fe8dbf9d1b81a49351a2e06", gitBlob(manifest(t, "cloudnative-pg-namespace").Bytes))
}
