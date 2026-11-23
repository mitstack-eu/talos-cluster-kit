package robotcluster_test

// The robot cluster module builds against the kit and operator modules of its own commit, as a block that requires all
// three at one release builds them.

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A build with -trimpath records module paths in place of directories, so this reads a build without it.
func TestModule_BuildsAgainstTheKitAndTheOperatorModuleInTheDirectoriesBesideIt(t *testing.T) {
	for _, tc := range []struct {
		module, packageDir string
		fn                 any
	}{
		{"the kit", "../../controlplane", controlplane.NewRequestReads},
		{"the operator module", "..", operator.AppName},
	} {
		t.Run(tc.module, func(t *testing.T) {
			want, err := filepath.Abs(tc.packageDir)
			require.NoError(t, err)
			pc := reflect.ValueOf(tc.fn).Pointer()
			file, _ := runtime.FuncForPC(pc).FileLine(pc)
			assert.Equal(t, want, filepath.Dir(file), "the directory the build compiled a package of %s from", tc.module)
		})
	}
}
