package pools

import (
	"strconv"

	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

// Hostname names the worker for its Robot server, since a dedicated node has no metadata service to name it.
func (d Dedicated) Hostname() string { return "robot-" + strconv.FormatInt(d.ServerNumber, 10) }

// RenderWorker renders the worker's machine configuration from the cluster's secrets, which hold no etcd authority key
// for a worker. The worker joins through its card on the vSwitch.
func (d Dedicated) RenderWorker(c nodeconfig.Cluster, vswitch nodeconfig.VSwitch) ([]byte, error) {
	c.Profile = nodeconfig.Metal
	return nodeconfig.RenderWorker(c, nodeconfig.Node{Hostname: d.Hostname(), VSwitch: &vswitch})
}
