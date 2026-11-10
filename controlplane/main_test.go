package controlplane_test

import (
	"os"
	"testing"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

func TestMain(m *testing.M) {
	controlplane.ShareMintedSecrets()
	os.Exit(m.Run())
}
