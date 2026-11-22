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

// oneLocation reports whether the plane runs one node beside one gateway, for which no other gateway stands in.
func (p Plane) oneLocation() bool { return len(p.NodeLocations()) == 1 }

func (p Plane) gatewayAddrs() []netip.Addr {
	locations := p.NodeLocations()
	out := make([]netip.Addr, 0, len(locations))
	for _, loc := range locations {
		out = append(out, netip.MustParseAddr(GatewayAddresses[loc]))
	}
	return out
}

// SizeRefusal judges a size from the state's networks alone, so a caller may pass a State holding nothing else.
func (p Plane) SizeRefusal(st State) error {
	size := p.size()
	if _, ok := nodeLocations[size]; !ok {
		return refuse("invalid-param", "a control plane runs 1 or 3 servers, and %d were asked for, so nothing was changed", size)
	}
	if size == 1 && p.Hosting != nil {
		return refuse("restore-drill-missing",
			"a cluster that hosts the platform runs OpenBao, whose data a single server holds alone; it runs on one server only once an ADR 0125 restore drill has passed for it, and none has, so nothing was changed")
	}
	if len(st.Networks) > 1 {
		return ambiguous(st.Duplicates())
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
