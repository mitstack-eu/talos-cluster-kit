package robotcluster

// The image server's wait for the gateway reads the Cloud on every check, so it sees what changed since its request read.

import (
	"context"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/stretchr/testify/require"
)

func TestAwaitGateway_SeesTheGatewayTakeItsAddressAfterItsRequestKeptAList(t *testing.T) {
	cloud := cloudtest.New(time.Now)
	reads := controlplane.NewRequestReads()
	p := controlplane.Plane{
		API:   controlplane.ReadOncePerRequest(cloud, func(context.Context) *controlplane.RequestReads { return reads }),
		Block: "test-block", Subscription: "sub_0a1b2c3d4e5f",
	}
	ctx := context.Background()
	network, err := cloud.CreateNetwork(ctx, controlplane.NetworkSpec{Name: "network", IPRange: controlplane.NetworkIPRange, Labels: p.Labels("network")})
	require.NoError(t, err)
	firewall, err := cloud.CreateFirewall(ctx, controlplane.FirewallSpec{Name: "firewall", Labels: p.Labels("firewall")})
	require.NoError(t, err)
	gateways, err := cloud.CreateServers(ctx, controlplane.ServerSpec{
		Name: "gateway", ServerType: controlplane.DefaultServerType, Location: controlplane.GatewayLocation,
		Labels: p.Labels(controlplane.RoleGateway),
	})
	require.NoError(t, err)
	_, err = p.Read(ctx)
	require.NoError(t, err, "the request's read before the gateway takes its address")
	require.NoError(t, cloud.AttachServerToNetwork(ctx, gateways[0].ID, network.ID, controlplane.GatewayAddress))

	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	got, err := imageCloud{plane: p, network: network.ID, poll: time.Millisecond}.awaitGateway(wait)

	require.NoError(t, err, "the wait for a gateway that took its address after the request's read")
	require.Equal(t, firewall.ID, got, "the firewall the image server is created in")
}
