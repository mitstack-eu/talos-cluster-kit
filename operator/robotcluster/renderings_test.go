package robotcluster_test

// Two releases that differ only in the address Argo CD reads the cluster repository at get a rendering each.

import (
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/github/landing"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderingOf_HashesTwoReleasesThatDifferOnlyInTheirAddressToTwoRenderings(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	repo := clusterrepo.For(robotclustertest.Owner, robotclustertest.App)
	https := rollout.Release{ToolsImage: testutil.ToolsImage, RepoURL: repo.URL(), Components: components.HostingPolicies()}
	ssh := https
	ssh.RepoURL = components.RepositorySSHURL(repo.FullName())

	first, err := e.Cluster.RenderingOf(https, repo.FullName())
	require.NoError(t, err, "the HTTPS release's rendering")
	second, err := e.Cluster.RenderingOf(ssh, repo.FullName())
	require.NoError(t, err, "the SSH release's rendering")

	assert.NotSame(t, first, second, "the rendering kept for each address")
	for name, held := range map[string]struct {
		release   rollout.Release
		rendering *robotcluster.Rendering
	}{"the HTTPS release": {https, first}, "the SSH release": {ssh, second}} {
		files, err := held.release.Files(repo.FullName())
		require.NoError(t, err, "%s's files", name)
		blobs := map[string]string{}
		for _, f := range files {
			blobs[f.Path] = landing.BlobHash(f.Bytes)
		}
		assert.Equal(t, blobs, held.rendering.Blobs(), "the blobs %s's rendering holds", name)
	}
}
