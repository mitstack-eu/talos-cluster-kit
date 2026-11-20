package robotcluster_test

// Within one request the cluster lists each kind of Cloud resource once, and its worker install reads the Cloud afresh.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/helpers/reqcache"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	onceSub    = "sub_0a1b2c3d4e5f"
	onceServer = 3041840
)

func onDedicatedServer(t *testing.T, opts ...robotclustertest.Option) *robotclustertest.Env {
	t.Helper()
	e := robotclustertest.New(t, onceSub, append([]robotclustertest.Option{robotclustertest.WithParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(onceServer),
	})}, opts...)...)
	e.Robot.AddServer(robottest.Server{Number: onceServer, IP: "203.0.113.51", DC: "FSN1-DC14"})
	return e
}

func TestValidate_ReadsEachCloudKindOnce(t *testing.T) {
	e := onDedicatedServer(t)
	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	ctx := reqcache.With(context.Background())
	before := e.Cloud.Reads()

	for name, o := range e.Cluster.Observers() {
		_, err := o.ObserveResource(ctx, contract.ResourceRef{Name: name, Type: name, Desired: e.Desired()})
		require.NoError(t, err, "the observe of %s", name)
	}

	made := e.Cloud.Reads().Since(before)
	for _, list := range []string{"ListServers", "ListLoadBalancers", "ListNetworks", "ListFirewalls", "ListSnapshots"} {
		assert.Equal(t, 1, made[list], "the %s calls of one validate, among its reads %v", list, made)
	}
}

func TestWorkerInstall_ReadsTheCloudAfreshThoughItsRequestKeptAList(t *testing.T) {
	var probed, afresh bool
	var probeErr error
	e := onDedicatedServer(t, func(env *robotclustertest.Env) {
		install := env.Config.InstallWorker
		env.Config.InstallWorker = func(ctx context.Context, in robotcluster.WorkerInstall) error {
			probed = true
			afresh, probeErr = readsAfresh(ctx, env)
			return install(ctx, in)
		}
	})

	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	require.True(t, probed, "the dedicated server's install ran")
	require.NoError(t, probeErr, "the install's reads")
	assert.True(t, afresh, "the install's second read shows the label the Cloud took after its first")
}

// readsAfresh reads the plane twice in ctx through a request's reads, labelling the network on the Cloud in between.
func readsAfresh(ctx context.Context, e *robotclustertest.Env) (bool, error) {
	reads := controlplane.NewRequestReads()
	p := e.Plane()
	p.API = controlplane.ReadOncePerRequest(e.Cloud, func(context.Context) *controlplane.RequestReads { return reads })
	first, err := p.Read(ctx)
	if err != nil {
		return false, err
	}
	network, ok := first.Network()
	if !ok {
		return false, errors.New("the install's first read holds no cluster network")
	}
	labels := maps.Clone(network.Labels)
	labels["probe"] = "taken"
	if err := e.Cloud.SetNetworkLabels(context.Background(), network.ID, labels); err != nil {
		return false, err
	}
	second, err := p.Read(ctx)
	if err != nil {
		return false, err
	}
	relabelled, _ := second.Network()
	return relabelled.Labels["probe"] == "taken", e.Cloud.SetNetworkLabels(context.Background(), network.ID, network.Labels)
}
