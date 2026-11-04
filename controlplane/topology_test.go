package controlplane_test

// Every Cloud node is labelled with the location it runs in, and a block's worker takes the same two labels.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/topology"
)

func TestCluster_LabelsEachCloudNodeWithItsLocationAndTheNetworkZone(t *testing.T) {
	h := installedOnce(t)

	labeller := inlineManifests(t, builderConfiguration(t, h))["mitstack-node-topology"]

	cronJobs := testutil.Kind(labeller, "CronJob")
	require.Len(t, cronJobs, 1, "the builder's configuration carries the labeller inline")
	env := testutil.ContainerEnv(t, testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0"))
	var want []string
	for _, loc := range controlplane.Locations {
		want = append(want, h.plane.LocatedName(controlplane.RoleNode, loc)+"="+loc)
	}
	assert.Equal(t, strings.Join(want, " "), env["TOPOLOGY_NODES"], "each node by the name Hetzner gives its server")
	assert.Contains(t, env["TOPOLOGY_NODES"], h.plane.LocatedName(controlplane.RoleNode, "nbg1")+"=nbg1")
	assert.Equal(t, "eu-central", env["TOPOLOGY_REGION"])
}

func TestTopologyLabels_NameAWorkersLocationAsItsZoneAndTheNetworkZoneAsItsRegion(t *testing.T) {
	assert.Equal(t, map[string]string{topology.ZoneLabel: "hel1", topology.RegionLabel: "eu-central"}, controlplane.TopologyLabels("hel1"))
	assert.Equal(t, "eu-central", controlplane.NetworkZone)
}
