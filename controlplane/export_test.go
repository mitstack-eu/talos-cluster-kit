package controlplane

// The tests sit in controlplane_test, since they share the cloudtest fake a block uses, which imports this package.

import (
	"context"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
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
	ServingFile              = servingFile
	GatewayAddrs             = gatewayAddrs
)

func SwapToolsImage(t testing.TB, image string) {
	t.Helper()
	old := toolsImage
	toolsImage = func() string { return image }
	t.Cleanup(func() { toolsImage = old })
}

func BuilderUserData(machineConfig []byte) (string, error) {
	return builderUserData("builder", machineConfig, nil, nil)
}

// WithGatewayCA is p holding a newly minted gateway CA, which a plane that installed nothing holds none of.
func (p Plane) WithGatewayCA() (Plane, error) {
	ca, err := gateway.NewCA(time.Now(), gatewayCAValidity, gatewayAddrs())
	if err != nil {
		return Plane{}, err
	}
	p.GatewayCA = &ca
	return p, nil
}

func (p Plane) ClusterComponents(enrolmentToken string) (components.Set, error) {
	if p.GatewayCA == nil {
		var err error
		if p, err = p.WithGatewayCA(); err != nil {
			return components.Set{}, err
		}
	}
	return p.clusterComponents(components.Default(), enrolmentToken)
}

// BuildSet is the component set an install applies, rendered under the plane's gateway CA.
func (p Plane) BuildSet(ctx context.Context, now time.Time, enrolmentToken string) (components.Set, error) {
	set, _, err := p.buildSet(ctx, now, enrolmentToken)
	return set, err
}
