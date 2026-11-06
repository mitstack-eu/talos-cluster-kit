package pools_test

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func vswitch(t *testing.T, address string) nodeconfig.VSwitch {
	t.Helper()
	mac, err := net.ParseMAC("9c:6b:00:55:d8:f5")
	require.NoError(t, err)
	return nodeconfig.VSwitch{MAC: mac, VLAN: 4000, Address: netip.MustParsePrefix(address)}
}

// etcdAuthorityKey is the key that lets a node issue etcd certificates, which only a control-plane node holds.
func etcdAuthorityKey(t *testing.T, config []byte) any {
	t.Helper()
	cluster, _ := testutil.Documents(t, config)[0]["cluster"].(map[string]any)
	etcd, _ := cluster["etcd"].(map[string]any)
	ca, _ := etcd["ca"].(map[string]any)
	return ca["key"]
}

func TestDedicated_RendersAWorkerConfigurationOnItsVSwitchWithNoEtcdAuthorityKey(t *testing.T) {
	cluster := testutil.NewCluster(t, testutil.WithLocations(3))
	controlPlane, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	require.NotEmpty(t, etcdAuthorityKey(t, controlPlane), "the search misses the key a control-plane node holds")
	entry := pools.Dedicated{ServerNumber: 3041839, RobotAccount: robotAccount}

	got, err := entry.RenderWorker(cluster, vswitch(t, "10.0.9.2/24"))

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	assert.Equal(t, "worker", testutil.Field(t, docs[0], "machine", "type"), "the machine type")
	assert.Empty(t, etcdAuthorityKey(t, got), "the worker could issue etcd certificates")
	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "HostnameConfig", "hostname": "robot-3041839"}}, testutil.Kind(docs, "HostnameConfig"))
	assert.Len(t, testutil.Kind(docs, "VLANConfig"), 1, "the worker's vSwitch")
}

func TestDedicated_TwoEntriesRenderWorkersNamedByTheirServerNumbers(t *testing.T) {
	cluster := testutil.NewCluster(t)
	hostnames := map[string]bool{}
	for i, number := range []int64{3041839, 3052000} {
		entry := pools.Dedicated{ServerNumber: number, RobotAccount: robotAccount}

		got, err := entry.RenderWorker(cluster, vswitch(t, []string{"10.0.9.2/24", "10.0.9.3/24"}[i]))

		require.NoError(t, err)
		hostnames[testutil.Kind(testutil.Documents(t, got), "HostnameConfig")[0]["hostname"].(string)] = true
	}

	assert.Equal(t, map[string]bool{"robot-3041839": true, "robot-3052000": true}, hostnames)
}

func TestDedicated_RefusesAVSwitchAddressOutsideTheClusterNetwork(t *testing.T) {
	entry := pools.Dedicated{ServerNumber: 3041839, RobotAccount: robotAccount}

	_, err := entry.RenderWorker(testutil.NewCluster(t), vswitch(t, "192.168.9.2/24"))

	assert.ErrorContains(t, err, "outside the cluster network")
}
