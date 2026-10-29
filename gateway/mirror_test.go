//go:build mirror

// The gateway's own skopeo copies trust-manager's OCI index into the gateway's own registry with its digest kept.
package gateway_test

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestPrepareScript_MirrorsTrustManagersIndexIntoTheGatewaysRegistryWithItsDigest(t *testing.T) {
	image := hostingImage(t, "quay.io/jetstack/trust-manager")
	startGatewayRegistry(t)

	run := prepare(t, map[string]string{}, sha256Of("bytes of https://example.test/argocd.yaml"), image.Ref())

	require.NoError(t, run.err, run.out)
	assert.FileExists(t, run.completionFile())
	assert.Equal(t, image.Digest, servedDigest(t, image), "the digest the mirror serves for "+image.Ref())
}

func hostingImage(t *testing.T, name string) components.Image {
	t.Helper()
	for _, ref := range testutil.HostingImages {
		if image, err := components.ParseImage(ref); err == nil && image.Name == name {
			return image
		}
	}
	require.Fail(t, "the hosting set pulls no "+name)
	return components.Image{}
}

// startGatewayRegistry runs the registry image the gateway runs, on 127.0.0.1:5000 as prepare expects.
func startGatewayRegistry(t *testing.T) {
	t.Helper()
	fields := strings.Fields(execStart(t, writtenFile(t, "/etc/systemd/system/mitstack-registry.service")))
	name := "mitstack-mirror-check"
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--network", "host",
		"-e", "REGISTRY_HTTP_ADDR=127.0.0.1:5000", fields[len(fields)-1]).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://127.0.0.1:5000/v2/")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, time.Minute, time.Second, "the gateway's registry never answered")
}

// servedDigest is the digest the local registry serves for the image's tag, as a node asking for any manifest type sees.
func servedDigest(t *testing.T, image components.Image) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, "http://127.0.0.1:5000/v2/"+image.Name+"/manifests/"+image.Tag, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the mirror holds no %s", image.Ref())
	return resp.Header.Get("Docker-Content-Digest")
}
