package controlplane

import "github.com/mitstack-eu/talos-cluster-kit/topology"

// TopologyLabels are the zone and region labels of a node in loc, the ones the labeller sets on each Cloud node.
func TopologyLabels(loc string) map[string]string {
	return topology.Labels(loc, NetworkZone)
}

func (p Plane) topologyManifests() ([]byte, error) {
	nodes := make([]topology.Node, 0, len(Locations))
	for _, loc := range Locations {
		nodes = append(nodes, topology.Node{Name: p.LocatedName(RoleNode, loc), Zone: loc})
	}
	return topology.Manifests(topology.Inputs{Region: NetworkZone, Nodes: nodes, ToolsImage: toolsImage()})
}
