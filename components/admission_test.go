package components_test

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
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

// manifest returns the default set's manifest of that name, served or inline.
func manifest(t *testing.T, name string) components.Manifest {
	t.Helper()
	set := components.Default()
	for _, m := range slices.Concat(set.Served, set.Inline) {
		if m.Name == name {
			return m
		}
	}
	require.Failf(t, "no such manifest", "the default set has no manifest %s", name)
	return components.Manifest{}
}

func namedDocument(t *testing.T, m components.Manifest, kind, name string) map[string]any {
	t.Helper()
	for _, doc := range testutil.Kind(testutil.Documents(t, m.Bytes), kind) {
		if testutil.Field(t, doc, "metadata", "name") == name {
			return doc
		}
	}
	require.Failf(t, "no such document", "manifest %s has no %s %s", m.Name, kind, name)
	return nil
}

func stringList(t *testing.T, list any) []string {
	t.Helper()
	items, ok := list.([]any)
	require.True(t, ok, "%v is not a list", list)
	var out []string
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}

func TestAdmission_ExemptsTheSameSystemNamespacesInBothPoliciesAndKyvernosWebhooks(t *testing.T) {
	label := testutil.Documents(t, manifest(t, "mitstack-require-app-label").Bytes)[0]
	generate := testutil.Documents(t, manifest(t, "mitstack-generate-default-deny").Bytes)[0]
	kyvernoConfig := namedDocument(t, manifest(t, "kyverno"), "ConfigMap", "kyverno")

	labelExempt := stringList(t, testutil.Field(t, label, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values"))
	condition := testutil.Field(t, generate, "spec", "matchConditions", "0", "expression").(string)
	var generateExempt []string
	for _, m := range regexp.MustCompile(`'([a-z0-9-]+)'`).FindAllStringSubmatch(condition, -1) {
		generateExempt = append(generateExempt, m[1])
	}
	var webhooks struct {
		NamespaceSelector struct {
			MatchExpressions []struct{ Values []string } `json:"matchExpressions"`
		} `json:"namespaceSelector"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.Field(t, kyvernoConfig, "data", "webhooks").(string)), &webhooks))
	webhookExempt := webhooks.NamespaceSelector.MatchExpressions[0].Values

	assert.Equal(t, labelExempt, generateExempt, "the default-deny generator exempts other namespaces than the label policy")
	assert.Equal(t, labelExempt, webhookExempt, "Kyverno's webhooks exempt other namespaces than the label policy")
	for _, ns := range []string{"kube-system", "argocd", "kyverno", "cnpg-system", nodeconfig.TalosAPINamespace} {
		assert.Contains(t, labelExempt, ns, "a Kyverno outage or a default deny would stop %s's pods", ns)
	}
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

func (e ciliumEndpoint) selectedBy(t *testing.T, selector any) bool {
	t.Helper()
	require.NotNil(t, selector, "a rule names a nil selector")
	labels, _ := testutil.Field(t, selector, "matchLabels").(map[string]any)
	for k, v := range labels {
		if got, ok := e[k]; !ok || got != v {
			return false
		}
	}
	expressions, _ := testutil.Field(t, selector, "matchExpressions").([]any)
	for _, x := range expressions {
		_, has := e[testutil.Field(t, x, "key").(string)]
		switch op := testutil.Field(t, x, "operator"); op {
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		default:
			require.Failf(t, "unevaluated operator", "the policy uses matchExpressions operator %v, which this test does not evaluate", op)
		}
	}
	return true
}

var databaseSliceEndpoints = map[string]ciliumEndpoint{
	"instance": endpoint("app-pg", map[string]string{"mitstack.dev/database-slice": "true", "mitstack.dev/subscription": "sub-pg"},
		map[string]string{"cnpg.io/cluster": "shared", "cnpg.io/podRole": "instance", "mitstack.dev/app": "postgres"}),
	"job": endpoint("app-pg", map[string]string{"mitstack.dev/database-slice": "true", "mitstack.dev/subscription": "sub-pg"},
		map[string]string{"cnpg.io/cluster": "shared", "cnpg.io/jobRole": "initdb", "mitstack.dev/app": "postgres"}),
	"app": endpoint("app-a1", map[string]string{"mitstack.dev/subscription": "sub-a1"}, map[string]string{"mitstack.dev/app": "shop"}),
	"instance outside a database slice": endpoint("app-b2", map[string]string{"mitstack.dev/subscription": "sub-b2"},
		map[string]string{"cnpg.io/cluster": "own", "cnpg.io/podRole": "instance", "mitstack.dev/app": "own"}),
	"operator": endpoint("cnpg-system", map[string]string{"pod-security.kubernetes.io/enforce": "restricted"},
		map[string]string{"app.kubernetes.io/name": "cloudnative-pg", "app.kubernetes.io/instance": "cloudnative-pg", "mitstack.dev/app": "cloudnative-pg"}),
	"system pod": endpoint("kube-system", nil, map[string]string{"k8s-app": "kube-dns"}),
}

func rulePorts(t *testing.T, rule any) string {
	t.Helper()
	var ports []string
	toPorts, _ := testutil.Field(t, rule, "toPorts").([]any)
	for _, tp := range toPorts {
		list, _ := testutil.Field(t, tp, "ports").([]any)
		for _, p := range list {
			ports = append(ports, fmt.Sprintf("%v/%v", testutil.Field(t, p, "port"), testutil.Field(t, p, "protocol")))
		}
	}
	if len(ports) == 0 {
		return "any port"
	}
	return strings.Join(ports, ",")
}

// grants lists what the policy allows the endpoint named, as "egress to <peer> on <ports>" and "ingress from <peer> on <ports>".
func grants(t *testing.T, policy map[string]any, name string) []string {
	t.Helper()
	specs, ok := policy["specs"].([]any)
	require.True(t, ok && len(specs) > 0, "the policy has no specs list")
	var got []string
	for _, spec := range specs {
		if !databaseSliceEndpoints[name].selectedBy(t, testutil.Field(t, spec, "endpointSelector")) {
			continue
		}
		for direction, peers := range map[string]string{"egress": "toEndpoints", "ingress": "fromEndpoints"} {
			rules, _ := testutil.Field(t, spec, direction).([]any)
			for _, rule := range rules {
				ports := rulePorts(t, rule)
				entities, _ := testutil.Field(t, rule, "toEntities").([]any)
				for _, e := range entities {
					got = append(got, fmt.Sprintf("%s to entity %v on %s", direction, e, ports))
				}
				selectors, _ := testutil.Field(t, rule, peers).([]any)
				for _, s := range selectors {
					for _, peer := range slices.Sorted(maps.Keys(databaseSliceEndpoints)) {
						if databaseSliceEndpoints[peer].selectedBy(t, s) {
							got = append(got, fmt.Sprintf("%s with %s on %s", direction, peer, ports))
						}
					}
				}
				for _, key := range []string{"fromEntities", "toCIDR", "toCIDRSet", "fromCIDR", "fromCIDRSet", "toFQDNs", "toServices"} {
					assert.Nil(t, testutil.Field(t, rule, key), "a %s rule uses %s, which this test does not evaluate", direction, key)
				}
			}
		}
	}
	slices.Sort(got)
	return slices.Compact(got)
}

func TestDatabaseSlicesPolicy_GrantsEachPodExactlyItsAllows(t *testing.T) {
	policy := namedDocument(t, manifest(t, "mitstack-database-slices"), "CiliumClusterwideNetworkPolicy", "mitstack-database-slices")
	for _, tc := range []struct {
		name, pod string
		want      []string
	}{
		{"a Postgres instance reaches the API server and the other instances, and admits the operator on 8000 and every slice on 5432", "instance", []string{
			"egress to entity kube-apiserver on any port",
			"egress with instance on 5432/TCP",
			"ingress with app on 5432/TCP",
			"ingress with instance on 5432/TCP",
			"ingress with instance outside a database slice on 5432/TCP",
			"ingress with job on 5432/TCP",
			"ingress with operator on 8000/TCP",
		}},
		{"a CloudNativePG Job reaches the API server and the instances on 5432", "job", []string{
			"egress to entity kube-apiserver on any port",
			"egress with instance on 5432/TCP",
		}},
		{"a pod in an app slice reaches only the database slice's instances, on 5432", "app", []string{
			"egress with instance on 5432/TCP",
		}},
		{"a pod outside a database slice gains none of the database slice's allows", "instance outside a database slice", []string{
			"egress with instance on 5432/TCP",
		}},
		{"the operator in cnpg-system is selected by no rule", "operator", nil},
		{"a system pod is selected by no rule", "system pod", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := grants(t, policy, tc.pod)

			assert.Equal(t, tc.want, got, "the policy grants the %s pod other allows", tc.pod)
		})
	}
}

// gitBlob is the object id git gives a file's bytes, which `git rev-parse <tag>:<path>` answers.
func gitBlob(data []byte) string {
	sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...))
	return hex.EncodeToString(sum[:])
}

func TestCloudNativePG_IsClusterBasesV040sComponentByteForByte(t *testing.T) {
	// The blobs `git rev-parse v0.4.0:cloudnative-pg/<file>` answers in mitstack-eu/cluster-bases.
	want := map[string]string{
		"cloudnative-pg":           "790c97885e59c3eaddb627eb9d5f6847e86f0e0e",
		"cloudnative-pg-namespace": "e232215537d185178fe8dbf9d1b81a49351a2e06",
	}

	for name, blob := range want {
		assert.Equal(t, blob, gitBlob(manifest(t, name).Bytes), "manifest %s is not cluster-bases v0.4.0's file", name)
	}
}
