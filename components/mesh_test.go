package components_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const operatorPrincipal = "cluster.local/ns/cnpg-system/sa/cloudnative-pg"

func kyvernoExempt(t *testing.T, policy string) []any {
	t.Helper()
	doc := testutil.Documents(t, manifest(t, policy).Bytes)[0]
	return testutil.Field(t, doc, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any)
}

func TestMesh_EnrolsEveryPodOutsideTheNamespacesKyvernoLeavesAlone(t *testing.T) {
	config := namedDocument(t, manifest(t, "istio-cni"), "ConfigMap", "istio-cni-config")
	var selectors []map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(testutil.Field(t, config, "data", "AMBIENT_ENABLEMENT_SELECTOR").(string)), &selectors))

	excluded := strings.Split(testutil.Field(t, config, "data", "EXCLUDE_NAMESPACES").(string), ",")

	assert.Equal(t, []map[string]any{{"podSelector": map[string]any{}, "namespaceSelector": map[string]any{}}}, selectors,
		"a pod or a namespace could leave the mesh by a label")
	assert.Equal(t, "false", testutil.Field(t, config, "data", "AMBIENT_DNS_CAPTURE"), "ztunnel would answer lookups Cilium's DNS proxy must see")
	assert.ElementsMatch(t, []string{"kube-system", "istio-system", "kyverno"}, excluded)
	for _, policy := range []string{"mitstack-require-mesh", "mitstack-require-strict-mtls"} {
		assert.Subset(t, kyvernoExempt(t, policy), toAny(excluded), "%s checks a namespace the mesh skips", policy)
	}
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func TestMesh_RunsIstioFromTheMirrorWithoutASidecarInjectorAndBesideCiliumsChain(t *testing.T) {
	set := components.Default()
	images, err := mirror.List(set)
	require.NoError(t, err)
	istiod := manifest(t, "istiod")
	cilium := namedDocument(t, manifest(t, "cilium"), "ConfigMap", "cilium-config")
	namespace := testutil.Kind(testutil.Documents(t, manifest(t, "istio-base").Bytes), "Namespace")[0]

	for _, name := range []string{"docker.io/istio/pilot", "docker.io/istio/install-cni", "docker.io/istio/ztunnel"} {
		assert.True(t, slices.ContainsFunc(images, func(i components.Image) bool { return i.Name == name }), "the mirror list holds no %s", name)
	}
	assert.Equal(t, "istio-system", testutil.Field(t, namespace, "metadata", "name"))
	assert.Equal(t, "privileged", testutil.Field(t, namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce"),
		"Talos's default level, baseline, refuses ztunnel's and the CNI agent's capabilities")
	assert.Empty(t, slices.DeleteFunc(testutil.Kind(testutil.Documents(t, istiod.Bytes), "ConfigMap"), func(cm map[string]any) bool {
		return testutil.Field(t, cm, "metadata", "name") != "istio-sidecar-injector"
	}), "istiod could inject a sidecar, which takes a pod out of the ambient mesh")
	container := testutil.Field(t, namedDocument(t, istiod, "Deployment", "istiod"), "spec", "template", "spec", "containers", "0")
	assert.Equal(t, "true", testutil.ContainerEnv(t, container)["PILOT_ENABLE_AMBIENT"])
	assert.Equal(t, "false", testutil.Field(t, cilium, "data", "cni-exclusive"), "Cilium would delete the CNI configuration Istio chains")
	assert.Equal(t, "true", testutil.Field(t, cilium, "data", "bpf-lb-sock-hostns-only"), "Cilium's socket load balancing would bypass ztunnel")
}

// meshExceptions are the PeerAuthentications that let a port take a call without a workload identity, with those ports.
var meshExceptions = map[string][]int{
	"cnpg-system/mitstack-cloudnative-pg-webhook":       {9443},
	"cert-manager/mitstack-cert-manager-webhook":        {10250},
	"cert-manager/mitstack-trust-manager-webhook":       {6443},
	"external-secrets/mitstack-external-secrets-webhook": {10250},
	"traefik/mitstack-traefik-web":                      {8000, 8443},
}

func TestMesh_RefusesACallWithoutAWorkloadIdentityOnEveryPortButTheAPIServersAndTheWebs(t *testing.T) {
	set := hostingSet(t, testutil.HostingInputs(t))
	var peers, pods []map[string]any
	for _, m := range slices.Concat(set.Served, set.Inline) {
		docs := testutil.Documents(t, m.Bytes)
		peers = append(peers, testutil.Kind(docs, "PeerAuthentication")...)
		for _, kind := range []string{"Deployment", "DaemonSet", "StatefulSet"} {
			pods = append(pods, testutil.Kind(docs, kind)...)
		}
	}
	strict := testutil.Documents(t, manifest(t, "mitstack-strict-mtls").Bytes)[0]
	refusal := testutil.Field(t, testutil.Documents(t, manifest(t, "mitstack-require-strict-mtls").Bytes)[0], "spec", "matchConditions", "0", "expression").(string)

	assert.Equal(t, "istio-system", testutil.Field(t, strict, "metadata", "namespace"), "the mesh-wide policy lives in Istio's root namespace")
	assert.NotContains(t, testutil.Field(t, strict, "spec"), "selector")
	assert.Equal(t, "STRICT", testutil.Field(t, strict, "spec", "mtls", "mode"))
	got := map[string][]int{}
	for _, peer := range peers {
		key := fmt.Sprintf("%v/%v", testutil.Field(t, peer, "metadata", "namespace"), testutil.Field(t, peer, "metadata", "name"))
		if key == "istio-system/default" {
			continue
		}
		assert.Equal(t, "STRICT", testutil.Field(t, peer, "spec", "mtls", "mode"), key)
		// A port is a YAML integer key, which decodes into a map keyed by any.
		for port, mtls := range testutil.Field(t, peer, "spec", "portLevelMtls").(map[any]any) {
			require.Equal(t, "PERMISSIVE", testutil.Field(t, mtls, "mode"), key)
			p := port.(int)
			got[key] = append(got[key], p)
			assert.True(t, servesPort(t, pods, peer, p), "%s opens port %d, which no pod it selects listens on", key, p)
		}
		slices.Sort(got[key])
		if ns := testutil.Field(t, peer, "metadata", "namespace"); !slices.Contains(kyvernoExempt(t, "mitstack-require-strict-mtls"), ns) {
			assert.Contains(t, refusal, `"`+key+`"`, "Kyverno refuses the hosting set's own %s", key)
		}
	}
	assert.Equal(t, meshExceptions, got)
}

// servesPort reports whether a pod template peer's selector picks, in peer's namespace, has a container on port.
func servesPort(t *testing.T, workloads []map[string]any, peer map[string]any, port int) bool {
	t.Helper()
	want := testutil.Field(t, peer, "spec", "selector", "matchLabels").(map[string]any)
	for _, w := range workloads {
		if testutil.Field(t, w, "metadata", "namespace") != testutil.Field(t, peer, "metadata", "namespace") {
			continue
		}
		labels := testutil.Field(t, w, "spec", "template", "metadata", "labels").(map[string]any)
		if !matches(labels, want) {
			continue
		}
		for _, c := range testutil.Field(t, w, "spec", "template", "spec", "containers").([]any) {
			ports, _ := c.(map[string]any)["ports"].([]any)
			for _, p := range ports {
				if testutil.Field(t, p, "containerPort") == port {
					return true
				}
			}
		}
	}
	return false
}

func matches(labels, selector map[string]any) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// denied evaluates a DENY AuthorizationPolicy's rules, in the notPorts, ports and notPrincipals forms the kit uses, for
// a call from principal to port.
func denied(t *testing.T, policy map[string]any, principal string, port int) bool {
	t.Helper()
	require.Equal(t, "DENY", testutil.Field(t, policy, "spec", "action"))
	in := func(list any, value string) bool { return list != nil && slices.Contains(list.([]any), any(value)) }
	for _, rule := range testutil.Field(t, policy, "spec", "rules").([]any) {
		r := rule.(map[string]any)
		matched := true
		if to, ok := r["to"]; ok {
			op := testutil.Field(t, to, "0", "operation").(map[string]any)
			if ports, ok := op["ports"]; ok && !in(ports, fmt.Sprint(port)) {
				matched = false
			}
			if ports, ok := op["notPorts"]; ok && in(ports, fmt.Sprint(port)) {
				matched = false
			}
		}
		if from, ok := r["from"]; ok {
			if in(testutil.Field(t, from, "0", "source", "notPrincipals"), principal) {
				matched = false
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestMesh_KeepsTheOperatorAloneOnAnInstancesManagerPortAndEveryoneOffItsOtherPorts(t *testing.T) {
	policy := namedDocument(t, manifest(t, "mitstack-mesh-cloudnative-pg"), "AuthorizationPolicy", "mitstack-database-instance-ports")
	slice := "cluster.local/ns/app-a1/sa/default"

	assert.Equal(t, "istio-system", testutil.Field(t, policy, "metadata", "namespace"), "a policy outside Istio's root namespace binds only its own")
	assert.Equal(t, map[string]any{"cnpg.io/podRole": "instance"}, testutil.Field(t, policy, "spec", "selector", "matchLabels"))
	for _, c := range []struct {
		principal string
		port      int
		denied    bool
	}{
		{operatorPrincipal, 8000, false},
		{slice, 5432, false},
		{operatorPrincipal, 5432, false},
		{slice, 8000, true},
		{slice, 9187, true},
		{operatorPrincipal, 9187, true},
	} {
		assert.Equal(t, c.denied, denied(t, policy, c.principal, c.port), "a call from %s to port %d", c.principal, c.port)
	}
}

func TestMesh_AdmitsZtunnelsPortWhereverArgoCDsChartAdmitsACallBetweenItsPods(t *testing.T) {
	chart := testutil.Kind(testutil.Documents(t, manifest(t, "argocd").Bytes), "NetworkPolicy")
	mesh := testutil.Kind(testutil.Documents(t, manifest(t, "mitstack-mesh-argocd").Bytes), "NetworkPolicy")

	var checked int
	for _, policy := range chart {
		for _, rule := range testutil.Field(t, policy, "spec", "ingress").([]any) {
			from, _ := rule.(map[string]any)["from"].([]any)
			if len(from) == 0 || testutil.Field(t, from, "0", "namespaceSelector") != nil {
				continue
			}
			checked++
			selector := testutil.Field(t, policy, "spec", "podSelector")
			i := slices.IndexFunc(mesh, func(m map[string]any) bool {
				return fmt.Sprint(testutil.Field(t, m, "spec", "podSelector")) == fmt.Sprint(selector)
			})
			require.GreaterOrEqual(t, i, 0, "no mesh policy for %v", testutil.Field(t, policy, "metadata", "name"))
			meshRule := testutil.Field(t, mesh[i], "spec", "ingress", "0")
			assert.Equal(t, from, testutil.Field(t, meshRule, "from"), "%v", testutil.Field(t, policy, "metadata", "name"))
			assert.Equal(t, []any{map[string]any{"port": 15008, "protocol": "TCP"}}, testutil.Field(t, meshRule, "ports"))
		}
	}
	assert.Equal(t, len(mesh), checked, "a mesh policy matches no rule of the chart")
}
