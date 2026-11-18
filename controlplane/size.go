package controlplane

import (
	"net/netip"
	"slices"
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

// NodeLocations are the locations holding one of the plane's nodes and a gateway each, empty for a size it cannot build;
// each call gives a copy, so a caller's change cannot reach Locations.
func (p Plane) NodeLocations() []string { return slices.Clone(nodeLocations[p.size()]) }

func (p Plane) gatewayAddrs() []netip.Addr {
	locations := p.NodeLocations()
	out := make([]netip.Addr, 0, len(locations))
	for _, loc := range locations {
		out = append(out, netip.MustParseAddr(GatewayAddresses[loc]))
	}
	return out
}

// SizeRefusal refuses a size the plane cannot build, one server for a hosting cluster with no passed OpenBao restore
// drill, and a size other than its network recorded at subscribe, reading the state's networks and nothing else.
func (p Plane) SizeRefusal(st State) error {
	size := p.size()
	if _, ok := nodeLocations[size]; !ok {
		return refuse("invalid-param", "a control plane runs 1 or 3 servers, and %d were asked for, so nothing was changed", size)
	}
	if size == 1 && p.Hosting != nil {
		return refuse("restore-drill-missing",
			"a cluster that hosts the platform runs OpenBao, whose data a single server holds alone; it runs on one server only once an ADR 0125 restore drill has passed for it, and none has, so nothing was changed")
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
