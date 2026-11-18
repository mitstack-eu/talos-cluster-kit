package controlplane

import (
	"net/netip"
	"strconv"
)

const (
	// DefaultSize is the size of a plane given none, which every cluster built before the size was an input has.
	DefaultSize = 3
	// KeySize is the size the network records at its creation, which a cluster keeps for its whole life.
	KeySize = "mitstack.dev/control-plane-size"
)

// nodeLocations are the locations of a plane of each size, each holding a node and a gateway; one etcd member keeps no
// quorum through a loss, and three keep two.
var nodeLocations = map[int][]string{1: Locations[:1], 3: Locations}

func (p Plane) size() int {
	if p.Size == 0 {
		return DefaultSize
	}
	return p.Size
}

// NodeLocations are the locations holding one of the plane's nodes and a gateway each, empty for a size it cannot build.
func (p Plane) NodeLocations() []string { return nodeLocations[p.size()] }

func (p Plane) gatewayAddrs() []netip.Addr {
	out := make([]netip.Addr, 0, len(p.NodeLocations()))
	for _, loc := range p.NodeLocations() {
		out = append(out, netip.MustParseAddr(GatewayAddresses[loc]))
	}
	return out
}

// sizeRefusal refuses a size the plane cannot build, one server for a cluster that hosts the platform, whose OpenBao
// runs a member on each of three nodes, and a size other than the one the network recorded at subscribe.
func (p Plane) sizeRefusal(st State) error {
	size := p.size()
	if _, ok := nodeLocations[size]; !ok {
		return refuse("invalid-param", "a control plane runs 1 or 3 servers, and %d were asked for, so nothing was changed", size)
	}
	if size == 1 && p.Hosting != nil {
		return refuse("invalid-param",
			"a cluster that hosts the platform runs OpenBao with a member on each of three control-plane nodes, so it cannot run on one, and nothing was changed")
	}
	network, ok := st.Network()
	if !ok {
		return nil
	}
	if subscribed := recordedSize(network); subscribed != strconv.Itoa(size) {
		return refuse("control-plane-size-fixed",
			"the cluster was subscribed with controlPlaneSize %s, and a control plane keeps its size after subscribe, so nothing was changed; leave controlPlaneSize at %s",
			subscribed, subscribed)
	}
	return nil
}

// recordedSize is the size the network records, or DefaultSize for a network created before it recorded one.
func recordedSize(network Network) string {
	if size, ok := network.Labels[KeySize]; ok {
		return size
	}
	return strconv.Itoa(DefaultSize)
}
