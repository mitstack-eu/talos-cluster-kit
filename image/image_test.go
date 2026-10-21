package image_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// fakeImager plays docker running imager: like imager, it prints the whole embedded configuration to both
// streams, then writes an image into /out and exits with FAKE_IMAGER_EXIT.
const fakeImager = `
in="" out="" profile=""
while [ "$#" -gt 0 ]; do
  case $1 in
    -v)
      case $2 in
        *:/in:ro) in=${2%:/in:ro} ;;
        *:/out) out=${2%:/out} ;;
      esac
      shift 2 ;;
    ghcr.io/siderolabs/imager:*) profile=$2; shift 2 ;;
    *) shift ;;
  esac
done
echo "$in" > "$FAKE_IMAGER_RECORD"
cat "$in/config.yaml"
cat "$in/config.yaml" >&2
[ "$FAKE_IMAGER_EXIT" = 0 ] || exit "$FAKE_IMAGER_EXIT"
echo built > "$out/$profile-amd64.raw.zst"
`

func TestRun_KeepsEveryKeyOfTheEmbeddedConfigurationOutOfTheBuildOutput(t *testing.T) {
	cases := []struct {
		name     string
		exitCode string
		wantErr  string
	}{
		{name: "a build that succeeds", exitCode: "0"},
		{name: "a build that fails", exitCode: "3", wantErr: "exit status 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := nodeconfig.Render(testutil.NewCluster(t), nodeconfig.Node{})
			require.NoError(t, err)
			tools := testutil.NewTools(t, map[string]string{"docker": fakeImager})
			record := filepath.Join(t.TempDir(), "in-dir")
			t.Setenv("PATH", tools.Dir+":"+os.Getenv("PATH"))
			t.Setenv("FAKE_IMAGER_EXIT", tc.exitCode)
			t.Setenv("FAKE_IMAGER_RECORD", record)
			var log bytes.Buffer

			path, err := image.Run(context.Background(), image.Build{Profile: nodeconfig.HCloud, Config: config, OutDir: t.TempDir()}, &log)

			output := log.String()
			if err != nil {
				output += err.Error()
			}
			for _, secret := range testutil.SecretValues(t) {
				assert.NotContains(t, output, secret, "the build output holds a key of the fixture configuration")
			}
			inDir, readErr := os.ReadFile(record)
			require.NoError(t, readErr)
			assert.NoDirExists(t, strings.TrimSpace(string(inDir)), "the configuration outlives the build")
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "hcloud-amd64.raw.zst", filepath.Base(path))
			assert.Contains(t, output, "hcloud-amd64.raw.zst")
		})
	}
}

func TestPinnedImager_BuildsTheTalosReleaseTheComponentSetPins(t *testing.T) {
	ref, err := components.ParseImage(image.PinnedImager)

	require.NoError(t, err)
	assert.Equal(t, components.PinnedVersions.Talos, ref.Tag)
}

func TestRun_RefusesAnImagerNotPinnedByDigest(t *testing.T) {
	build := image.Build{Profile: nodeconfig.Metal, Config: []byte("version: v1alpha1"), OutDir: t.TempDir(), Imager: "ghcr.io/siderolabs/imager:v1.14.4"}

	_, err := image.Run(context.Background(), build, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghcr.io/siderolabs/imager:v1.14.4")
}
