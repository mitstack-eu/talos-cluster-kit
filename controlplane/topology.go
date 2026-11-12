package controlplane

import (
	"slices"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/topology"
)

// TopologyLabels are the zone and region labels of a node in loc, the ones the labeller sets on each Cloud node.
func TopologyLabels(loc string) map[string]string {
	return topology.Labels(loc, NetworkZone)
}

// topologyManifests names each control-plane node by its location, since the build renders the labeller before any
// server exists, then each of poolWorkers by the name and location Hetzner records, in name order, so the labeller
// renders the same bytes however Hetzner lists them.
func (p Plane) topologyManifests(poolWorkers []Server) ([]byte, error) {
	nodes := make([]topology.Node, 0, len(Locations)+len(poolWorkers))
	for _, loc := range Locations {
		nodes = append(nodes, topology.Node{Name: p.LocatedName(RoleNode, loc), Zone: loc})
	}
	byName := slices.SortedFunc(slices.Values(poolWorkers), func(a, b Server) int { return strings.Compare(a.Name, b.Name) })
	for _, worker := range byName {
		nodes = append(nodes, topology.Node{Name: worker.Name, Zone: worker.Location})
	}
	return topology.Manifests(topology.Inputs{Region: NetworkZone, Nodes: nodes, ToolsImage: toolsImage()})
}
