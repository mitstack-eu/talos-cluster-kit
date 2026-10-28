package controlplane_test

// Every install also builds a worker image and keeps it as a snapshot, so a Cloud worker joins with no rebuild.

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func (h *harness) workerImages() []controlplane.Snapshot {
	h.t.Helper()
	snaps, err := h.cloud.ListSnapshots(context.Background(), h.plane.RoleSelector(controlplane.RoleWorkerImage))
	require.NoError(h.t, err)
	return snaps
}

func TestPlane_LeavesOneWorkerImageLabelledForTheSubscriptionAndNoWorkerBuilder(t *testing.T) {
	h := newHarness(t)

	h.built()

	images := h.workerImages()
	require.Len(t, images, 1, "worker images")
	assert.Equal(t, h.plane.Labels(controlplane.RoleWorkerImage), images[0].Labels, "the worker image's labels")
	assert.Equal(t, controlplane.SnapshotAvailable, images[0].Status, "the worker image's status")
	assert.Empty(t, h.byRole(controlplane.RoleWorkerBuilder), "a worker builder outlived its snapshot")
	assert.Equal(t, "fsn1", h.cloud.SpecFor(t, controlplane.RoleWorkerBuilder).Location, "the worker builder's location")
}

func TestPlane_TheWorkerImageHoldsNoneOfTheClustersCAKeysAndItsUserDataFitsHetznersLimit(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.converge())
	keys := map[string]string{}
	for _, doc := range builderConfiguration(t, h) {
		kind, _ := doc["kind"].(string)
		privateKeys(doc, kind, keys)
	}
	for bundle, field := range map[string]string{
		"certs.os.key": "machine.ca.key", "certs.k8s.key": "KubeAPIServerCAConfig.issuingCA.key",
		"certs.etcd.key": "cluster.etcd.ca.key", "certs.k8sserviceaccount.key": "KubeServiceAccountConfig.issuer.privateKey",
	} {
		require.NotEmpty(t, keys[field], "the control plane holds %s at %s; it holds %v", bundle, field, slices.Sorted(maps.Keys(keys)))
	}

	data := h.cloud.SpecFor(t, controlplane.RoleWorkerBuilder).UserData

	t.Logf("the worker builder's user data is %d bytes", len(data))
	assert.LessOrEqual(t, len(data), cloudtest.UserDataLimit, "the worker builder's user data is %d bytes; Hetzner accepts %d", len(data), cloudtest.UserDataLimit)
	config := decodeCloudConfig(t, data).file(t, controlplane.BuilderConfigPath).Content
	assert.Equal(t, "worker", testutil.Field(t, testutil.Documents(t, []byte(config))[0], "machine", "type"), "the worker image's machine type")
	for name, key := range keys {
		assert.False(t, strings.Contains(config, key), "the worker image holds the control plane's %s", name)
	}
}

func TestPlane_TheWorkerBuilderJoinsTheNetworkAtItsFixedAddressAndBuildsOnlyOnceTheGatewayRuns(t *testing.T) {
	h := newHarness(t)
	building := func() bool {
		builders := h.byRole(controlplane.RoleWorkerBuilder)
		return len(builders) == 1 && builders[0].Status == controlplane.ServerRunning
	}

	require.NoError(t, h.settle(building))

	require.True(t, building(), "the worker builder never started its build")
	assert.Equal(t, controlplane.WorkerBuilderAddress, h.byRole(controlplane.RoleWorkerBuilder)[0].PrivateIPs[h.network().ID], "the worker builder's address")
	gateways := h.byRole(controlplane.RoleGateway)
	require.Len(t, gateways, 1, "gateways")
	assert.Equal(t, controlplane.ServerRunning, gateways[0].Status, "the gateway while the worker builder builds")
}

func TestPlane_DeleteDeletesTheWorkerImage(t *testing.T) {
	h := newHarness(t)
	h.built()
	require.Len(t, h.workerImages(), 1, "worker images before the delete")

	require.NoError(t, h.plane.Delete(context.Background()))

	assert.Empty(t, h.workerImages(), "worker images after the delete")
}

// privateKeys records every non-empty key or privateKey field under node by its dotted path.
func privateKeys(node any, path string, out map[string]string) {
	switch v := node.(type) {
	case map[string]any:
		for name, child := range v {
			field := strings.TrimPrefix(path+"."+name, ".")
			if s, ok := child.(string); ok && (name == "key" || name == "privateKey") && s != "" {
				out[field] = s
			}
			privateKeys(child, field, out)
		}
	case []any:
		for _, child := range v {
			privateKeys(child, path, out)
		}
	}
}
