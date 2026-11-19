package robotclustertest_test

// Block drives the kit's block guards over a cluster of one with no dedicated server, settled on the fakes.

import (
	"os"
	"testing"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/verify/kittest"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/stretchr/testify/require"
)

func cloudOnly(e *robotclustertest.Env) {
	e.Config.DedicatedServerOptional, e.Config.DeclaredSize, e.Config.ClusterType = true, 1, "github.runner-cluster"
	e.Params["controlPlaneSize"], e.Params["cloudPoolServerTypes"], e.Params["cloudPoolLocations"] = "1", "cx43", "fsn1"
	e.Params["cloudPoolMin"], e.Params["cloudPoolMax"], e.Params["cloudPoolIdleMinutes"] = "1", "4", "15"
}

func TestBlock_HoldsTheKitsBlockGuardsOverAClusterOfOneWithNoDedicatedServer(t *testing.T) {
	e := robotclustertest.New(t, "sub_0b10c4d1e2f3", cloudOnly)
	manifest, err := os.ReadFile("testdata/cloud-only-manifest.yaml")
	require.NoError(t, err)
	m, err := contract.ParseManifest(manifest)
	require.NoError(t, err)
	snapshot, err := os.ReadFile("testdata/type-snapshot.json")
	require.NoError(t, err)
	types, err := contract.ParseTypeSnapshot(snapshot)
	require.NoError(t, err)
	observers, all := map[string]handler.Observer{}, e.Cluster.Observers()
	for _, r := range m.Block.Spec.Resources {
		observers[r.Name] = all[r.Name]
	}

	kittest.AssertBlock(t, e.Block(handler.Config{Resource: e.Cluster, Manifest: manifest, Observers: observers, Types: types,
		JWKSURL: "https://api.mitstack.test/.well-known/jwks.json"}))
}
