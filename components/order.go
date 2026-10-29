package components

import (
	"fmt"
	"slices"
)

var (
	meshDataplane         = []string{"cilium", "cilium-policy-crd", "istio-base", "istiod", "istio-cni", "ztunnel"}
	clusterReporting      = []string{"mitstack-node-report", "mitstack-cluster-tokens", "mitstack-node-logs"}
	componentsAndPolicies = []string{
		"argocd", "cert-manager", "cloudnative-pg", "cloudnative-pg-namespace", "kyverno", "local-path-provisioner",
		"local-path-provisioner-namespace", "mitstack-ambient-probes", "mitstack-cluster-application",
		"mitstack-cluster-repository", "mitstack-database-slices", "mitstack-generate-default-deny", "mitstack-hosting-flows",
		"mitstack-hosting-mesh", "mitstack-local-path-flows", "mitstack-mesh-argocd", "mitstack-mesh-cloudnative-pg",
		"mitstack-metadata-block", "mitstack-openbao-deploy-key", "mitstack-openbao-init", "mitstack-openbao-seal-key",
		"mitstack-openbao-storage", "mitstack-require-app-label", "mitstack-require-mesh", "mitstack-require-strict-mtls",
		"mitstack-sealed-secrets-key", "mitstack-strict-mtls", "openbao", "sealed-secrets", "traefik",
	}
	webhookValidated = []string{"external-secrets", "trust-manager", "mitstack-hosting-tls"}
)

// applyOrder puts each webhook's pod and the mesh it runs in before what it validates, since Talos abandons a pass there.
var applyOrder = slices.Concat(meshDataplane, clusterReporting, componentsAndPolicies, webhookValidated)

// TalosName is the name a node applies m under and the gateway serves it at, its place in applyOrder first.
func (m Manifest) TalosName() string {
	i := slices.Index(applyOrder, m.Name)
	if i < 0 {
		panic(fmt.Sprintf("manifest %s has no place in the apply order, which any render in the kit's tests reaches first", m.Name))
	}
	return fmt.Sprintf("%02d-%s", i, m.Name)
}
