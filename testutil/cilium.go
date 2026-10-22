package testutil

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// CiliumEndpoint is a pod as Cilium labels it: its own labels under k8s:, its namespace's under io.cilium.k8s.namespace.labels.
type CiliumEndpoint map[string]string

func Endpoint(namespace string, namespaceLabels, podLabels map[string]string) CiliumEndpoint {
	e := CiliumEndpoint{"k8s:io.kubernetes.pod.namespace": namespace}
	for k, v := range namespaceLabels {
		e["k8s:io.cilium.k8s.namespace.labels."+k] = v
	}
	for k, v := range podLabels {
		e["k8s:"+k] = v
	}
	return e
}

// SelectedBy evaluates matchLabels and the Exists operator, the only selector forms the kit's policies use.
func (e CiliumEndpoint) SelectedBy(t *testing.T, selector any) bool {
	t.Helper()
	labels, _ := Field(t, selector, "matchLabels").(map[string]any)
	for k, v := range labels {
		if e[k] != v {
			return false
		}
	}
	expressions, _ := Field(t, selector, "matchExpressions").([]any)
	for _, x := range expressions {
		require.Equal(t, "Exists", Field(t, x, "operator"), "the policy uses an operator this test does not evaluate")
		if _, has := e[Field(t, x, "key").(string)]; !has {
			return false
		}
	}
	return true
}

func ports(t *testing.T, rule map[string]any) string {
	t.Helper()
	var got []string
	toPorts, _ := rule["toPorts"].([]any)
	for _, tp := range toPorts {
		for _, p := range Field(t, tp, "ports").([]any) {
			got = append(got, fmt.Sprintf("%v/%v", Field(t, p, "port"), Field(t, p, "protocol")))
		}
		if l7, _ := Field(t, tp, "rules").(map[string]any); l7["dns"] != nil {
			got = append(got, "through the DNS proxy")
		}
	}
	return strings.Join(got, " ")
}

// Grants lists what a Cilium policy allows endpoints[pod], as "<direction> with <peer> on <ports>", "<direction> to
// entity <entity>" or "egress to <DNS name> on <ports>".
func Grants(t *testing.T, policy map[string]any, endpoints map[string]CiliumEndpoint, pod string) []string {
	t.Helper()
	var got []string
	for _, spec := range policy["specs"].([]any) {
		if !endpoints[pod].SelectedBy(t, Field(t, spec, "endpointSelector")) {
			continue
		}
		for direction, peers := range map[string]string{"egress": "toEndpoints", "ingress": "fromEndpoints"} {
			rules, _ := Field(t, spec, direction).([]any)
			for _, rule := range rules {
				r := rule.(map[string]any)
				entities, _ := r["toEntities"].([]any)
				for _, entity := range entities {
					got = append(got, fmt.Sprintf("%s to entity %v", direction, entity))
				}
				names, _ := r["toFQDNs"].([]any)
				for _, name := range names {
					got = append(got, fmt.Sprintf("egress to %v on %s", Field(t, name, "matchName"), ports(t, r)))
				}
				selectors, _ := r[peers].([]any)
				for _, s := range selectors {
					for peer, e := range endpoints {
						if e.SelectedBy(t, s) {
							got = append(got, fmt.Sprintf("%s with %s on %s", direction, peer, ports(t, r)))
						}
					}
				}
			}
		}
	}
	slices.Sort(got)
	return slices.Compact(got)
}
