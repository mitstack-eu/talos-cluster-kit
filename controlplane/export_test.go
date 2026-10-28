package controlplane

// The tests sit in controlplane_test, since they share the cloudtest fake a block uses, which imports this package.

import (
	"testing"

	"github.com/mitstack-eu/talos-cluster-kit/components"
)

const (
	BuilderConfigPath        = builderConfigPath
	BuilderScriptPath        = builderScriptPath
	BootstrapTalosconfigPath = bootstrapTalosconfigPath
	BootstrapUnit            = bootstrapUnit
	SubnetPrefix             = subnetPrefix
	RouterAddress            = routerAddress
	AdminTalosconfigTTL      = adminTalosconfigTTL
)

var (
	BuildImageScript         = buildImageScript
	BootstrapFirstNodeScript = bootstrapFirstNodeScript
	ClusterTokensScript      = clusterTokensScript
	BuilderUserData          = builderUserData
	KitRelease               = kitRelease
)

func SwapToolsImage(t testing.TB, image string) {
	t.Helper()
	old := toolsImage
	toolsImage = func() string { return image }
	t.Cleanup(func() { toolsImage = old })
}

func (p Plane) ClusterComponents(enrolmentToken string) (components.Set, error) {
	return p.clusterComponents(enrolmentToken)
}
