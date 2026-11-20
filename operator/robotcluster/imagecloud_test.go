package robotcluster

// The image server's wait for the gateway reads the Cloud on every check, and only the gateway once it has the firewall.

import (
	"context"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/stretchr/testify/assert"
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

func TestAwaitGateway_ReadsOnlyTheGatewayServer(t *testing.T) {
	cloud, p, network, gateway := gatewayWithoutItsAddress(t)
	firewall, err := cloud.CreateFirewall(context.Background(), controlplane.FirewallSpec{Name: "firewall", Labels: p.Labels("firewall")})
	require.NoError(t, err)
	p.API = &changingCloud{API: cloud, beforeServers: func(call int) {
		if call == 3 {
			require.NoError(t, cloud.AttachServerToNetwork(context.Background(), gateway, network, controlplane.GatewayAddress))
		}
	}}
	const poll = 20 * time.Millisecond
	before, start := cloud.Reads(), time.Now()

	wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := imageCloud{plane: p, network: network, poll: poll}.awaitGateway(wait)

	require.NoError(t, err, "the wait for a gateway that takes its address on the third check")
	assert.Equal(t, firewall.ID, got, "the firewall the image server is created in")
	assert.Equal(t, cloudtest.Reads{"ListFirewalls": 1, "ListServers": 3}, cloud.Reads().Since(before),
		"the Cloud calls of a wait that finds the firewall once and checks the gateway three times")
	assert.GreaterOrEqual(t, time.Since(start), 2*poll, "the wait between its three checks")
}

func TestAwaitGateway_WaitsForAFirewallMadeAfterItsFirstCheck(t *testing.T) {
	cloud, p, network, gateway := gatewayWithoutItsAddress(t)
	require.NoError(t, cloud.AttachServerToNetwork(context.Background(), gateway, network, controlplane.GatewayAddress))
	var firewall controlplane.Firewall
	p.API = &changingCloud{API: cloud, beforeFirewalls: func(call int) {
		if call == 2 {
			var err error
			firewall, err = cloud.CreateFirewall(context.Background(), controlplane.FirewallSpec{Name: "firewall", Labels: p.Labels("firewall")})
			require.NoError(t, err)
		}
	}}

	wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := imageCloud{plane: p, network: network, poll: time.Millisecond}.awaitGateway(wait)

	require.NoError(t, err, "the wait for a firewall made after its first check")
	assert.Equal(t, firewall.ID, got, "the firewall the image server is created in")
}

// gatewayWithoutItsAddress is a Cloud holding a subscription's network and its gateway, which holds no address on it yet.
func gatewayWithoutItsAddress(t *testing.T) (*cloudtest.Fake, controlplane.Plane, int64, int64) {
	t.Helper()
	cloud := cloudtest.New(time.Now)
	p := controlplane.Plane{API: cloud, Block: "test-block", Subscription: "sub_0a1b2c3d4e5f"}
	network, err := cloud.CreateNetwork(context.Background(), controlplane.NetworkSpec{Name: "network", IPRange: controlplane.NetworkIPRange, Labels: p.Labels("network")})
	require.NoError(t, err)
	gateways, err := cloud.CreateServers(context.Background(), controlplane.ServerSpec{
		Name: "gateway", ServerType: controlplane.DefaultServerType, Location: controlplane.GatewayLocation,
		Labels: p.Labels(controlplane.RoleGateway),
	})
	require.NoError(t, err)
	return cloud, p, network.ID, gateways[0].ID
}

// changingCloud changes the Cloud before a numbered list call, as the Cloud moving between a wait's checks.
type changingCloud struct {
	controlplane.API
	beforeServers, beforeFirewalls func(call int)
	servers, firewalls             int
}

func (c *changingCloud) ListServers(ctx context.Context, selector string) ([]controlplane.Server, error) {
	c.servers++
	if c.beforeServers != nil {
		c.beforeServers(c.servers)
	}
	return c.API.ListServers(ctx, selector)
}

func (c *changingCloud) ListFirewalls(ctx context.Context, selector string) ([]controlplane.Firewall, error) {
	c.firewalls++
	if c.beforeFirewalls != nil {
		c.beforeFirewalls(c.firewalls)
	}
	return c.API.ListFirewalls(ctx, selector)
}
