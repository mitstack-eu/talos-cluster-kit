package robotclustertest_test

// Block drives the kit's block guards over a cluster of one with no dedicated server, settled on the fakes.

import (
	_ "embed"
	"testing"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/verify/kittest"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/stretchr/testify/require"
)

var (
	//go:embed testdata/cloud-only-manifest.yaml
	cloudOnlyManifest []byte
	//go:embed testdata/type-snapshot.json
	typeSnapshot []byte
)

func cloudOnly(e *robotclustertest.Env) {
	e.Config.DedicatedServerOptional, e.Config.DeclaredSize, e.Config.ClusterType = true, 1, "github.runner-cluster"
	e.Params["controlPlaneSize"], e.Params["cloudPoolServerTypes"], e.Params["cloudPoolLocations"] = "1", "cx43", "fsn1"
	e.Params["cloudPoolMin"], e.Params["cloudPoolMax"], e.Params["cloudPoolIdleMinutes"] = "1", "4", "15"
}

func TestBlock_HoldsTheKitsBlockGuardsOverAClusterOfOneWithNoDedicatedServer(t *testing.T) {
	e := robotclustertest.New(t, "sub_0b10c4d1e2f3", cloudOnly)
	m, err := contract.ParseManifest(cloudOnlyManifest)
	require.NoError(t, err)
	types, err := contract.ParseTypeSnapshot(typeSnapshot)
	require.NoError(t, err)
	observers, all := map[string]handler.Observer{}, e.Cluster.Observers()
	for _, r := range m.Block.Spec.Resources {
		observers[r.Name] = all[r.Name]
	}

	kittest.AssertBlock(t, e.Block(handler.Config{Resource: e.Cluster, Manifest: cloudOnlyManifest, Observers: observers, Types: types,
		JWKSURL: "https://api.mitstack.test/.well-known/jwks.json"}))
}
