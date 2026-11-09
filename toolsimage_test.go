//go:build toolsimage

// A gateway's preparation copies talosctl, kubectl and every manifest it serves out of the tools image the tools image
// CI job builds, read by its digest through Docker from the registry image the gateway runs, each with its pin.
package talosclusterkit

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

// toolsRepository is the gateway's local registry and the published package's repository under it.
const toolsRepository = "127.0.0.1:5000/ghcr.io/mitstack-eu/talos-cluster-kit-tools"

func TestToolsImage_AGatewaysPreparationCopiesTalosctlKubectlAndEveryServedManifestOutOfItByItsDigest(t *testing.T) {
	built := os.Getenv("TOOLS_IMAGE")
	require.NotEmpty(t, built, "TOOLS_IMAGE names the tools image to copy out of, as the tools image CI job builds it")
	set := components.HostingServed()
	toolsImage := pushedToGatewayRegistry(t, built, gatewayFiles(t, set, testutil.ToolsImage))
	files := gatewayFiles(t, set, toolsImage.Ref())
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	toolsEnv := write("tools.env", files["/etc/mitstack-gateway/tools.env"]+"GATEWAY_CONSOLE="+dir+"/tty1\nGATEWAY_ISSUE="+dir+"/issue\n")
	manifests := write("manifests.txt", files["/etc/mitstack-gateway/manifests.txt"])
	images := write("images.txt", "") // the push above stands in for the mirror's copy of the tools image
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o755))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "mitstack-gateway-tools").Run() })

	out, err := testutil.RunBash(t, files["/usr/local/bin/mitstack-gateway-prepare"], testutil.NewTools(t, map[string]string{"systemctl": ""}).Env("PREPARE_BIN_DIR="+bin),
		toolsEnv, images, manifests, filepath.Join(dir, "manifests"), filepath.Join(dir, "mirror-complete"))

	require.NoError(t, err, out)
	assert.Contains(t, out, "Unable to find image '"+toolsRepository+"@"+toolsImage.Digest+"' locally", "the preparation read the tools image from the gateway's registry")
	assert.FileExists(t, filepath.Join(dir, "mirror-complete"))
	assert.Equal(t, tools.Pin("TALOSCTL_SHA256"), fileSHA256(t, filepath.Join(bin, "talosctl")), "the sha256 of the talosctl installed")
	assert.Equal(t, tools.Pin("KUBECTL_SHA256"), fileSHA256(t, filepath.Join(bin, "kubectl")), "the sha256 of the kubectl installed")
	for _, m := range set.Served {
		assert.Equal(t, m.SHA256(), fileSHA256(t, filepath.Join(dir, "manifests", m.TalosName()+".yaml")), "the sha256 of the %s served", m.Name)
	}
}

// gatewayFiles is each file a gateway serving set writes, by its path, with toolsImage its tools image.
func gatewayFiles(t *testing.T, set components.Set, toolsImage string) map[string]string {
	t.Helper()
	image, err := components.ParseImage(toolsImage)
	require.NoError(t, err)
	list, err := gateway.NewList(set, image)
	require.NoError(t, err)
	rendered, err := gateway.Render(gateway.Inputs{Address: netip.MustParseAddr("10.0.0.2"), Network: netip.MustParsePrefix("10.0.0.0/16"),
		TLS: testutil.GatewayTLS(t), List: list, Tools: image, Gzipped: true})
	require.NoError(t, err)
	files := map[string]string{}
	for _, f := range testutil.Field(t, testutil.Documents(t, rendered)[0], "write_files").([]any) {
		file := f.(map[string]any)
		files[file["path"].(string)] = file["content"].(string)
	}
	return files
}

// pushedToGatewayRegistry starts the registry the gateway runs, pushes built to it and untags it locally, so the
// preparation's Docker reads the image from that registry by its digest.
func pushedToGatewayRegistry(t *testing.T, built string, files map[string]string) components.Image {
	t.Helper()
	testutil.StartGatewayRegistry(t, files["/etc/systemd/system/mitstack-registry.service"], "mitstack-tools-image-check")
	tag := toolsRepository + ":check"
	runDocker(t, "tag", built, tag)
	runDocker(t, "push", "--quiet", tag)
	digests, err := exec.Command("docker", "image", "inspect", "--format", "{{range .RepoDigests}}{{println .}}{{end}}", tag).Output()
	require.NoError(t, err)
	var pushed string
	for _, ref := range strings.Fields(string(digests)) {
		if strings.HasPrefix(ref, toolsRepository+"@") {
			pushed = ref
		}
	}
	require.NotEmpty(t, pushed, "the push names no digest under %s: %s", toolsRepository, digests)
	runDocker(t, "rmi", tag)
	if exec.Command("docker", "image", "inspect", pushed).Run() == nil {
		runDocker(t, "rmi", pushed)
	}
	image, err := components.ParseImage("ghcr.io/mitstack-eu/talos-cluster-kit-tools:check@" + strings.TrimPrefix(pushed, toolsRepository+"@"))
	require.NoError(t, err)
	return image
}

func runDocker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %v: %s", args, out)
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "no file at %s", path)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
