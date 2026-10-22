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

// manifest returns the default set's manifest of that name, served or inline.
func manifest(t *testing.T, name string) components.Manifest {
	t.Helper()
	set := components.Default()
	i := slices.IndexFunc(slices.Concat(set.Served, set.Inline), func(m components.Manifest) bool { return m.Name == name })
	require.GreaterOrEqual(t, i, 0, "the default set has no manifest %s", name)
	return slices.Concat(set.Served, set.Inline)[i]
}

func namedDocument(t *testing.T, m components.Manifest, kind, name string) map[string]any {
	t.Helper()
	docs := testutil.Kind(testutil.Documents(t, m.Bytes), kind)
	i := slices.IndexFunc(docs, func(doc map[string]any) bool { return testutil.Field(t, doc, "metadata", "name") == name })
	require.GreaterOrEqual(t, i, 0, "manifest %s has no %s %s", m.Name, kind, name)
	return docs[i]
}

func TestAdmission_ExemptsTheSameSystemNamespacesInBothPoliciesAndKyvernosWebhooks(t *testing.T) {
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
	assert.Subset(t, labelExempt, []string{"kube-system", "argocd", "kyverno", "cnpg-system", nodeconfig.TalosAPINamespace})
}

// ciliumEndpoint is a pod as Cilium labels it: its own labels under k8s:, its namespace's under io.cilium.k8s.namespace.labels.
type ciliumEndpoint map[string]string

func endpoint(namespace string, namespaceLabels, podLabels map[string]string) ciliumEndpoint {
	e := ciliumEndpoint{"k8s:io.kubernetes.pod.namespace": namespace}
	for k, v := range namespaceLabels {
		e["k8s:io.cilium.k8s.namespace.labels."+k] = v
	}
	for k, v := range podLabels {
		e["k8s:"+k] = v
	}
	return e
}

// selectedBy evaluates matchLabels and the Exists operator, the only selector forms the policy uses.
func (e ciliumEndpoint) selectedBy(t *testing.T, selector any) bool {
	t.Helper()
	labels, _ := testutil.Field(t, selector, "matchLabels").(map[string]any)
	for k, v := range labels {
		if e[k] != v {
			return false
		}
	}
	expressions, _ := testutil.Field(t, selector, "matchExpressions").([]any)
	for _, x := range expressions {
		require.Equal(t, "Exists", testutil.Field(t, x, "operator"), "the policy uses an operator this test does not evaluate")
		if _, has := e[testutil.Field(t, x, "key").(string)]; !has {
			return false
		}
	}
	return true
}

var sliceEndpoints = map[string]ciliumEndpoint{
	"instance": endpoint("app-pg", map[string]string{"mitstack.dev/database-slice": "true", "mitstack.dev/subscription": "sub-pg"},
		map[string]string{"cnpg.io/cluster": "shared", "cnpg.io/podRole": "instance", "mitstack.dev/app": "postgres"}),
	"job": endpoint("app-pg", map[string]string{"mitstack.dev/database-slice": "true", "mitstack.dev/subscription": "sub-pg"},
		map[string]string{"cnpg.io/cluster": "shared", "cnpg.io/jobRole": "initdb", "mitstack.dev/app": "postgres"}),
	"app": endpoint("app-a1", map[string]string{"mitstack.dev/subscription": "sub-a1"}, map[string]string{"mitstack.dev/app": "shop"}),
	"instance outside a database slice": endpoint("app-b2", map[string]string{"mitstack.dev/subscription": "sub-b2"},
		map[string]string{"cnpg.io/cluster": "own", "cnpg.io/podRole": "instance", "mitstack.dev/app": "own"}),
	"operator": endpoint("cnpg-system", nil, map[string]string{"app.kubernetes.io/name": "cloudnative-pg", "mitstack.dev/app": "cloudnative-pg"}),
}

// grants lists what the policy allows the pod named, as "<direction> with <peer> on <port>" or "<direction> to entity <entity>".
func grants(t *testing.T, policy map[string]any, pod string) []string {
	t.Helper()
	var got []string
	for _, spec := range policy["specs"].([]any) {
		if !sliceEndpoints[pod].selectedBy(t, testutil.Field(t, spec, "endpointSelector")) {
			continue
		}
		for direction, peers := range map[string]string{"egress": "toEndpoints", "ingress": "fromEndpoints"} {
			rules, _ := testutil.Field(t, spec, direction).([]any)
			for _, rule := range rules {
				r := rule.(map[string]any)
				var ports string
				if _, ok := r["toPorts"]; ok {
					p := testutil.Field(t, r, "toPorts", "0", "ports", "0").(map[string]any)
					ports = fmt.Sprintf("%v/%v", p["port"], p["protocol"])
				}
				entities, _ := r["toEntities"].([]any)
				for _, entity := range entities {
					got = append(got, fmt.Sprintf("%s to entity %v", direction, entity))
				}
				selectors, _ := r[peers].([]any)
				for _, s := range selectors {
					for peer, e := range sliceEndpoints {
						if e.selectedBy(t, s) {
							got = append(got, fmt.Sprintf("%s with %s on %s", direction, peer, ports))
						}
					}
				}
			}
		}
	}
	slices.Sort(got)
	return slices.Compact(got)
}

func TestDatabaseSlicesPolicy_GrantsEachPodExactlyItsAllows(t *testing.T) {
	policy := namedDocument(t, manifest(t, "mitstack-database-slices"), "CiliumClusterwideNetworkPolicy", "mitstack-database-slices")
	for pod, want := range map[string][]string{
		"instance": {
			"egress to entity kube-apiserver", "egress with instance on 5432/TCP", "ingress with app on 5432/TCP",
			"ingress with instance on 5432/TCP", "ingress with instance outside a database slice on 5432/TCP",
			"ingress with job on 5432/TCP", "ingress with operator on 8000/TCP",
		},
		"job":                               {"egress to entity kube-apiserver", "egress with instance on 5432/TCP"},
		"app":                               {"egress with instance on 5432/TCP"},
		"instance outside a database slice": {"egress with instance on 5432/TCP"},
		"operator":                          nil,
	} {
		t.Run(pod, func(t *testing.T) {
			assert.Equal(t, want, grants(t, policy, pod))
		})
	}
}

// gitBlob is the object id git gives a file's bytes, which `git rev-parse <tag>:<path>` answers.
func gitBlob(data []byte) string {
	sum := sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(data)), data...))
	return hex.EncodeToString(sum[:])
}

func TestCloudNativePG_IsClusterBasesV040sComponentByteForByte(t *testing.T) {
	assert.Equal(t, "790c97885e59c3eaddb627eb9d5f6847e86f0e0e", gitBlob(manifest(t, "cloudnative-pg").Bytes))
	assert.Equal(t, "e232215537d185178fe8dbf9d1b81a49351a2e06", gitBlob(manifest(t, "cloudnative-pg-namespace").Bytes))
}
