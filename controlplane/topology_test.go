package controlplane_test

// Every Cloud node is labelled with the location it runs in, and a block's worker takes the same two labels.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/topology"
)

const labellerName = "mitstack-node-topology"

var (
	labellerJob  = testutil.ObjectRef{Group: "batch", Kind: "CronJob", Namespace: "mitstack-node", Name: labellerName}
	labellerRole = testutil.ObjectRef{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: labellerName}
)

// poolWorkers grows a Cloud pool through the plan a block's upgrade applies, so the state holds servers Hetzner named.
func (h *harness) poolWorkers(size int, locations ...string) []controlplane.Server {
	h.t.Helper()
	pool := pools.CloudPool{ServerTypes: []string{controlplane.DefaultServerType}, Locations: locations, Min: size, Max: size}
	plan, err := pools.PlanCloud(context.Background(), h.plane, pool, func(controlplane.Server) bool { return false })
	require.NoError(h.t, err)
	require.NoError(h.t, plan.Apply(context.Background(), h.cloud))
	return h.byRole(pools.RoleCloudWorker)
}

func (h *harness) controlPlaneNodes() []string {
	var nodes []string
	for _, loc := range controlplane.Locations {
		nodes = append(nodes, h.plane.LocatedName(controlplane.RoleNode, loc)+"="+loc)
	}
	return nodes
}

func labelledNodes(t *testing.T, cronJob map[string]any) []string {
	t.Helper()
	container := testutil.Field(t, cronJob, "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
	return strings.Fields(testutil.ContainerEnv(t, container)["TOPOLOGY_NODES"])
}

func mayPatch(t *testing.T, role map[string]any) []string {
	t.Helper()
	var names []string
	for _, name := range testutil.Field(t, role, "rules", "0", "resourceNames").([]any) {
		names = append(names, name.(string))
	}
	return names
}

func releasedLabeller(t *testing.T, release rollout.Release) components.Manifest {
	t.Helper()
	return testutil.Manifest(t, labellerName, release.Components)
}

func only(t *testing.T, labeller components.Manifest, kind string) map[string]any {
	t.Helper()
	docs := testutil.Kind(testutil.Documents(t, labeller.Bytes), kind)
	require.Len(t, docs, 1, "the labeller's documents of kind %s", kind)
	return docs[0]
}

// carryLabeller commits the labeller's files of release to cluster's repository, as a block's carriage does, and has
// Argo CD apply them.
func carryLabeller(t *testing.T, cluster *testutil.Cluster, release rollout.Release) {
	t.Helper()
	files, err := rollout.Release{Components: []components.Manifest{releasedLabeller(t, release)}, ToolsImage: release.ToolsImage}.Files(testutil.InitRepository)
	require.NoError(t, err)
	committed := map[string][]byte{}
	for _, f := range files {
		committed[f.Path] = f.Bytes
	}
	cluster.Commit(committed)
	cluster.Sync()
}

func TestCluster_LabelsEachCloudNodeWithItsLocationAndTheNetworkZone(t *testing.T) {
	h := installedOnce(t)

	labeller := inlineManifests(t, builderConfiguration(t, h))["mitstack-node-topology"]

	cronJobs := testutil.Kind(labeller, "CronJob")
	require.Len(t, cronJobs, 1, "the builder's configuration carries the labeller inline")
	env := testutil.ContainerEnv(t, testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0"))
	assert.Equal(t, h.controlPlaneNodes(), labelledNodes(t, cronJobs[0]), "each node by the name Hetzner gives its server")
	assert.Contains(t, labelledNodes(t, cronJobs[0]), h.plane.LocatedName(controlplane.RoleNode, "nbg1")+"=nbg1")
	assert.Equal(t, "eu-central", env["TOPOLOGY_REGION"])
}

// The pool lists nbg1 first, so Hetzner lists its worker ahead of the one in fsn1.
func TestRelease_TheLabellerNamesEachCloudPoolWorkerHetznerListsWithItsServersLocationInNameOrder(t *testing.T) {
	h := newHarness(t)
	h.built()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	first := h.poolWorkers(1, "nbg1", "fsn1")
	withOne := releasedLabeller(t, h.releaseOf(plane))
	both := h.poolWorkers(2, "nbg1", "fsn1")
	withTwo := releasedLabeller(t, h.releaseOf(plane))

	require.Len(t, first, 1, "the pool's members once it holds one")
	require.Len(t, both, 2, "the pool's members once it holds two")
	require.Equal(t, []string{"nbg1", "fsn1"}, []string{both[0].Location, both[1].Location}, "the members' locations as Hetzner lists them")
	nbg1, fsn1 := both[0].Name, both[1].Name
	assert.Equal(t, append(h.controlPlaneNodes(), nbg1+"=nbg1"), labelledNodes(t, only(t, withOne, "CronJob")),
		"the nodes the labeller names once the pool holds a worker in nbg1")
	assert.Equal(t, append(h.controlPlaneNodes(), fsn1+"=fsn1", nbg1+"=nbg1"), labelledNodes(t, only(t, withTwo, "CronJob")),
		"the nodes the labeller names once the pool holds a worker in fsn1 too")
	var named []string
	for _, node := range labelledNodes(t, only(t, withTwo, "CronJob")) {
		name, _, _ := strings.Cut(node, "=")
		named = append(named, name)
	}
	assert.Equal(t, named, mayPatch(t, only(t, withTwo, "ClusterRole")), "the nodes the labeller may read and patch, against those it labels")
}

// The cluster starts from the labeller a build renders, as a running cluster's does, which names no pool worker.
func TestRelease_TheLabellerOfAClusterThatAlreadyRunsACloudWorkerNamesItWithItsZoneOnceArgoCDAppliesTheRelease(t *testing.T) {
	h := newHarness(t)
	h.built()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	built, err := plane.ClusterComponents("enrolment-token")
	require.NoError(t, err)
	cluster := testutil.BuildCluster(t, testutil.Manifest(t, labellerName, built.Inline).Bytes)
	carryLabeller(t, cluster, h.releaseOf(plane))
	worker := h.poolWorkers(1, "fsn1")[0]
	require.Equal(t, h.controlPlaneNodes(), labelledNodes(t, cluster.Object(labellerJob)), "the nodes the cluster's labeller named before the release")

	carryLabeller(t, cluster, h.releaseOf(plane))

	assert.Equal(t, append(h.controlPlaneNodes(), worker.Name+"=fsn1"), labelledNodes(t, cluster.Object(labellerJob)),
		"the nodes the cluster's labeller names once Argo CD applied the release")
	assert.Contains(t, mayPatch(t, cluster.Object(labellerRole)), worker.Name, "the nodes the cluster's labeller may read and patch")
}

func TestTopologyLabels_NameAWorkersLocationAsItsZoneAndTheNetworkZoneAsItsRegion(t *testing.T) {
	assert.Equal(t, map[string]string{topology.ZoneLabel: "hel1", topology.RegionLabel: "eu-central"}, controlplane.TopologyLabels("hel1"))
	assert.Equal(t, "eu-central", controlplane.NetworkZone)
}
