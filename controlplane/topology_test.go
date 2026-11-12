package controlplane_test

// Every Cloud node is labelled with the location it runs in, and a block's worker takes the same two labels.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/mitstack-eu/talos-cluster-kit/topology"
)

const labellerName = "mitstack-node-topology"

var (
	labellerJob  = testutil.ObjectRef{Group: "batch", Kind: "CronJob", Namespace: "mitstack-node", Name: labellerName}
	labellerRole = testutil.ObjectRef{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: labellerName}
)

// everyNodeJoined is a kubectl that finds each node it is asked for and accepts each label.
const everyNodeJoined = `
case "$1 $2" in
  "get node") echo "node/$3" ;;
  "label node") ;;
  *) exit 1 ;;
esac
`

// poolWorkers creates the servers a Cloud pool of size over locations lacks, as a block's upgrade does, and returns
// the pool's members.
func (h *harness) poolWorkers(size int, locations ...string) []controlplane.Server {
	h.t.Helper()
	pool := pools.CloudPool{ServerTypes: []string{controlplane.DefaultServerType}, Locations: locations, Min: size, Max: size}
	plan, err := pools.PlanCloud(context.Background(), h.plane, pool, func(controlplane.Server) bool { return false })
	require.NoError(h.t, err)
	require.NoError(h.t, plan.Apply(context.Background(), h.cloud))
	return h.byRole(pools.RoleCloudWorker)
}

// controlPlaneNodes are the labeller's entries for the three control-plane nodes, in the order of their locations.
func (h *harness) controlPlaneNodes() []string {
	var nodes []string
	for _, loc := range controlplane.Locations {
		nodes = append(nodes, h.plane.LocatedName(controlplane.RoleNode, loc)+"="+loc)
	}
	return nodes
}

func labellerContainer(t *testing.T, cronJob map[string]any) any {
	t.Helper()
	return testutil.Field(t, cronJob, "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
}

// releasedLabeller is the labeller's manifest as release carries it to a running cluster.
func releasedLabeller(t *testing.T, release rollout.Release) components.Manifest {
	t.Helper()
	return testutil.Manifest(t, labellerName, release.Components)
}

func releasedLabellerNodes(t *testing.T, release rollout.Release) string {
	t.Helper()
	cronJobs := testutil.Kind(testutil.Documents(t, releasedLabeller(t, release).Bytes), "CronJob")
	require.Len(t, cronJobs, 1, "the labeller's CronJobs")
	return testutil.ContainerEnv(t, labellerContainer(t, cronJobs[0]))["TOPOLOGY_NODES"]
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

// runClustersLabeller runs the labeller as the CronJob cluster's API serves runs it, with every node joined.
func runClustersLabeller(t *testing.T, cluster *testutil.Cluster) []string {
	t.Helper()
	cronJob := cluster.Object(labellerJob)
	require.NotNil(t, cronJob, "the cluster's labeller")
	c := labellerContainer(t, cronJob)
	require.Equal(t, []any{"bash", tools.ScriptDir + "/label-nodes.sh"}, testutil.Field(t, c, "command"), "the script the tools image carries")
	script, err := os.ReadFile(filepath.Join("..", "topology", "scripts", "label-nodes.sh"))
	require.NoError(t, err)
	fakes := testutil.NewTools(t, map[string]string{"kubectl": everyNodeJoined})
	var vars []string
	for name, value := range testutil.ContainerEnv(t, c) {
		vars = append(vars, name+"="+value)
	}
	out, err := testutil.RunBash(t, string(script), fakes.Env(vars...))
	require.NoError(t, err, "the labeller printed:\n%s", out)
	return fakes.Calls(t)
}

// mayPatch are the nodes the labeller's ClusterRole names.
func mayPatch(t *testing.T, role map[string]any) []string {
	t.Helper()
	var names []string
	for _, name := range testutil.Field(t, role, "rules", "0", "resourceNames").([]any) {
		names = append(names, name.(string))
	}
	return names
}

func labelCall(node, zone string) string {
	return "kubectl label node " + node + " --overwrite --field-manager=" + topology.FieldManager + " " +
		topology.ZoneLabel + "=" + zone + " " + topology.RegionLabel + "=" + controlplane.NetworkZone
}

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

// The pool lists nbg1 first, so Hetzner lists its worker ahead of the one in fsn1.
func TestRelease_TheLabellerNamesEachCloudPoolWorkerHetznerListsWithItsServersLocationInNameOrder(t *testing.T) {
	h := newHarness(t)
	h.built()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	first := h.poolWorkers(1, "nbg1", "fsn1")
	withOne := h.releaseOf(plane)
	both := h.poolWorkers(2, "nbg1", "fsn1")
	withTwo := h.releaseOf(plane)

	require.Len(t, first, 1, "the pool's members once it holds one")
	require.Len(t, both, 2, "the pool's members once it holds two")
	require.Equal(t, []string{"nbg1", "fsn1"}, []string{both[0].Location, both[1].Location}, "the members' locations as Hetzner lists them")
	nbg1, fsn1 := both[0].Name, both[1].Name
	assert.Equal(t, strings.Join(append(h.controlPlaneNodes(), nbg1+"=nbg1"), " "), releasedLabellerNodes(t, withOne),
		"the nodes the labeller names once the pool holds a worker in nbg1")
	assert.Equal(t, strings.Join(append(h.controlPlaneNodes(), fsn1+"=fsn1", nbg1+"=nbg1"), " "), releasedLabellerNodes(t, withTwo),
		"the nodes the labeller names once the pool holds a worker in fsn1 too")
	roles := testutil.Kind(testutil.Documents(t, releasedLabeller(t, withTwo).Bytes), "ClusterRole")
	require.Len(t, roles, 1, "the labeller's ClusterRoles")
	var labelled []string
	for _, entry := range strings.Fields(releasedLabellerNodes(t, withTwo)) {
		name, _, _ := strings.Cut(entry, "=")
		labelled = append(labelled, name)
	}
	assert.Equal(t, labelled, mayPatch(t, roles[0]), "the nodes the labeller may read and patch, against those it labels")
}

// The cluster was built, and took an earlier carriage, while its labeller named the control-plane nodes alone.
func TestRelease_AClusterThatAlreadyRunsACloudWorkerLabelsItWithItsZoneOnceArgoCDAppliesTheRelease(t *testing.T) {
	h := newHarness(t)
	h.built()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	built, err := plane.ClusterComponents("enrolment-token")
	require.NoError(t, err)
	cluster := testutil.BuildCluster(t, testutil.Manifest(t, labellerName, built.Inline).Bytes)
	carryLabeller(t, cluster, h.releaseOf(plane))
	worker := h.poolWorkers(1, "fsn1")[0]
	require.NotContains(t, strings.Join(runClustersLabeller(t, cluster), "\n"), worker.Name, "the calls of the labeller the cluster ran before the release")

	carryLabeller(t, cluster, h.releaseOf(plane))
	calls := runClustersLabeller(t, cluster)

	assert.Contains(t, calls, labelCall(worker.Name, "fsn1"), "the labeller's calls once Argo CD applied the release: %v", calls)
	for _, loc := range controlplane.Locations {
		assert.Contains(t, calls, labelCall(h.plane.LocatedName(controlplane.RoleNode, loc), loc), "the control-plane node in %s keeps its labels", loc)
	}
	assert.Contains(t, mayPatch(t, cluster.Object(labellerRole)), worker.Name, "the nodes the cluster's labeller may read and patch")
}

func TestTopologyLabels_NameAWorkersLocationAsItsZoneAndTheNetworkZoneAsItsRegion(t *testing.T) {
	assert.Equal(t, map[string]string{topology.ZoneLabel: "hel1", topology.RegionLabel: "eu-central"}, controlplane.TopologyLabels("hel1"))
	assert.Equal(t, "eu-central", controlplane.NetworkZone)
}
