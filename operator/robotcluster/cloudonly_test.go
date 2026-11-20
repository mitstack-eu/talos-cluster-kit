package robotcluster_test

// A request naming no dedicated server runs on Hetzner Cloud alone, with no Robot call, on an operator that makes the
// server optional, and is refused on one that requires it.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cloudOnlySub = "sub_0c10d0a1b2c3"

func cloudOnly(e *robotclustertest.Env) {
	e.Config.DedicatedServerOptional, e.Config.DeclaredSize = true, 1
	e.Params["controlPlaneSize"] = "1"
	e.Params["cloudPoolServerTypes"], e.Params["cloudPoolLocations"] = "cpx32", "fsn1"
	e.Params["cloudPoolMin"], e.Params["cloudPoolMax"], e.Params["cloudPoolIdleMinutes"] = "1", "4", "15"
}

func noRobotLogin(e *robotclustertest.Env) { e.NoRobotLogin = true }

func refusedAs(t *testing.T, err error, class string) *handler.Refused {
	t.Helper()
	var refused *handler.Refused
	require.ErrorAs(t, err, &refused, "the call's answer")
	assert.Equal(t, class, refused.Class, "the refusal's class: %s", refused.Detail)
	return refused
}

func TestCloudOnlyCluster_BuildsTheWorkerImageAndOnePoolWorkerTheIngressTargetsWithNoRobotCall(t *testing.T) {
	steps := robotclustertest.CaptureSteps(t)
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)

	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	assert.Equal(t, contract.Present, obs.State, obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "its ingress load balancer the Cloud pool's workers", "the ingress load balancer's targets")
	assert.NotContains(t, obs.StatusDetail, "10.0.1.2", "the ingress load balancer targets no Robot worker")
	assert.Len(t, e.ByRole(controlplane.RoleNode), 1, "the control plane")
	assert.Len(t, e.ByRole(controlplane.RoleCloudWorker), 1, "the pool's workers at its minimum")
	assert.Contains(t, steps.Steps(), "power on the worker builder (took)", "the worker image is built at install")
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
	assert.Zero(t, e.Installed(), "the dedicated server's installs")
	for _, c := range e.Created {
		assert.NotContains(t, []string{robotcluster.ResourceDedicatedServer, robotcluster.ResourceVSwitch, robotcluster.ResourceRobotFirewall},
			c.Name, "a resource the cluster reports")
	}
}

func TestCloudOnlyCluster_OfAnOperatorsOwnConfigSettlesOnTheFakesUnderItsBlockID(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, robotclustertest.WithOperator(robotcluster.Config{
		Block: "test-runner-cluster", Boundary: "test-boundary", InstallWorker: installNothing, DeclaredSize: 1, DedicatedServerOptional: true,
	}))

	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	assert.Equal(t, contract.Present, obs.State, obs.StatusDetail)
	for _, s := range e.ByRole(controlplane.RoleCloudWorker) {
		assert.Equal(t, "test-runner-cluster", s.Labels[controlplane.KeyBlock], "pool worker %s's block label", s.Name)
	}
	assert.Len(t, e.ByRole(controlplane.RoleCloudWorker), 1, "the pool's workers at its minimum")
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_OfAnOperatorNamingItsClusterTypeReportsAndObservesTheClusterUnderThatName(t *testing.T) {
	const runnerCluster = "github.runner-cluster"
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, robotclustertest.WithOperator(robotcluster.Config{
		Block: "test-runner-cluster", Boundary: "test-boundary", InstallWorker: installNothing, DeclaredSize: 1, DedicatedServerOptional: true,
		ClusterType: runnerCluster,
	}))

	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	assert.Contains(t, e.Created, contract.CreatedResource{Name: runnerCluster, Type: runnerCluster, Identity: map[string]string{"subscriptionId": cloudOnlySub}},
		"the cluster the subscribe reports")
	for _, c := range e.Created {
		assert.NotEqual(t, robotcluster.ResourceCluster, c.Name, "a resource the subscribe reports")
	}
	observers := e.Cluster.Observers()
	assert.NotContains(t, observers, robotcluster.ResourceCluster, "the observers the cluster offers")
	require.Contains(t, observers, runnerCluster, "the observers the cluster offers")
	read, err := observers[runnerCluster].ObserveResource(context.Background(), contract.ResourceRef{Name: runnerCluster, Type: runnerCluster, Desired: e.Desired()})
	require.NoError(t, err, "the cluster's observe")
	assert.Equal(t, contract.Present, read.State, read.StatusDetail)
	assert.Equal(t, cloudOnlySub, read.Outputs["subscriptionId"], "the subscription the cluster names")
}

func TestCloudOnlyCluster_ReadsItsRobotResourcesAbsentWithNoRobotCall(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	_, err := e.Settle()
	require.NoError(t, err)

	for _, name := range []string{robotcluster.ResourceDedicatedServer, robotcluster.ResourceVSwitch, robotcluster.ResourceRobotFirewall} {
		obs, err := e.Cluster.Observers()[name].ObserveResource(context.Background(), contract.ResourceRef{Desired: e.Desired()})
		require.NoError(t, err, name)
		assert.Equal(t, contract.Absent, obs.State, "%s: %s", name, obs.StatusDetail)
	}
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_BuildsAndIsRemovedOnAConnectionWithNoRobotLogin(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, noRobotLogin)

	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe, which must not refuse missing-robot-access: %s", obs.StatusDetail)
	assert.Equal(t, contract.Present, obs.State, obs.StatusDetail)

	require.NoError(t, e.Delete(), "the unsubscribe, which must not refuse robot-left-in-place")
	assert.Zero(t, e.Cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_RemovalDeletesEveryCloudResourceAndTheRepositoryWithNoRobotCall(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	_, err := e.Settle()
	require.NoError(t, err)

	require.NoError(t, e.Delete(), "the unsubscribe")

	assert.Zero(t, e.Cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.False(t, e.GitHub.Exists(e.Repo()), "the cluster repository after the unsubscribe")
	assert.Equal(t, contract.Absent, e.Observe().State, "the cluster after the unsubscribe")
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
	assert.NotContains(t, e.EventsSoFar(), "releasing the dedicated server and cancelling its vSwitch", "the unsubscribe's progress")
}

func TestCloudOnlyCluster_CreatesAPoolWorkerBackUpToTheMinimumOnceOneIsDeletedOutsideMitstack(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	_, err := e.Settle()
	require.NoError(t, err)
	gone := e.ByRole(controlplane.RoleCloudWorker)
	require.Len(t, gone, 1)

	require.NoError(t, e.Cloud.DeleteServer(context.Background(), gone[0].ID), "the delete outside mitstack")
	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe after the delete: %s", obs.StatusDetail)
	back := e.ByRole(controlplane.RoleCloudWorker)
	require.Len(t, back, 1, "the pool's workers back at its minimum")
	assert.NotEqual(t, gone[0].ID, back[0].ID, "the worker is a new server")
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyCluster_FailsNamingTheWorkerImageBuildAndIsRemovedWithEveryServerItMade(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	e.WorkerBuildHangs = true

	_, err := e.Settle()

	refused := refusedAs(t, err, "worker-image-build-failed")
	assert.Contains(t, refused.Detail, "worker", "the refusal names the failed step")
	require.NoError(t, e.Delete(), "the unsubscribe")
	assert.Zero(t, e.Cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
}

func TestCloudOnlyRequest_WithNoSteadyPoolWorkerIsRefusedNamingCloudPoolMinBeforeAnyChange(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"a pool of 0 to 4": {"cloudPoolMin": "0"},
		"no pool":          {"cloudPoolServerTypes": "", "cloudPoolLocations": "", "cloudPoolMin": "", "cloudPoolMax": "", "cloudPoolIdleMinutes": ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := robotclustertest.New(t, cloudOnlySub, cloudOnly, robotclustertest.WithParams(params))

			ensureErr := e.Cluster.Ensure(context.Background(), e.Desired())
			_, planErr := e.Cluster.PlanReplacements(context.Background(), e.Desired())

			for call, err := range map[string]error{"the subscribe": ensureErr, "the dry run": planErr} {
				refused := refusedAs(t, err, "invalid-param")
				assert.Contains(t, refused.Detail, `params["cloudPoolMin"]`, "%s names the input", call)
				assert.Contains(t, refused.Detail, "at least 1", "%s names the minimum", call)
			}
			assert.Zero(t, e.Cloud.Writes(), "a refused subscribe changes nothing in Hetzner Cloud")
			assert.Empty(t, e.Robot.Requests(), "the Robot fake's calls")
		})
	}
}

func TestDedicatedServerRequired_RefusesARequestNamingNoServerWithTodaysMessage(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, func(e *robotclustertest.Env) { e.Config.DedicatedServerOptional = false })

	err := e.Cluster.Ensure(context.Background(), e.Desired())

	refused := refusedAs(t, err, "missing-param")
	assert.Equal(t, `the dedicated server's number is required: params["robotServerNumber"] is empty, and the cluster's worker runs on that Robot server`,
		refused.Detail)
	assert.Zero(t, e.Cloud.Writes(), "a refused subscribe changes nothing in Hetzner Cloud")
}

func TestDedicatedServerCluster_OnAnOperatorWithTheServerOptionalTakesTheSameGolden(t *testing.T) {
	assertDedicatedServerGolden(t, func(e *robotclustertest.Env) { e.Config.DedicatedServerOptional = true })
}

func TestCloudOnlyRequest_OnAClusterBuiltOnADedicatedServerIsRefusedAndLeavesTheServerInPlace(t *testing.T) {
	e := robotclustertest.New(t, productionSub, robotclustertest.WithParams(map[string]string{
		robotcluster.ParamRobotServer: fmt.Sprint(productionServer), "serverType": "cx23,cpx22,cpx32",
		"cloudPoolServerTypes": "cpx32", "cloudPoolLocations": "fsn1", "cloudPoolMin": "1", "cloudPoolMax": "1",
	}), func(e *robotclustertest.Env) { e.Config.DedicatedServerOptional = true })
	e.Robot.AddServer(robottest.Server{Number: productionServer, IP: "203.0.113.50", DC: "FSN1-DC14"})
	_, err := e.Settle()
	require.NoError(t, err)
	e.Params[robotcluster.ParamRobotServer] = ""
	requests := len(e.Robot.Requests())

	refused := refusedAs(t, e.Cluster.Ensure(context.Background(), e.Desired()), "dedicated-server-removed")
	assert.Contains(t, refused.Detail, `params["robotServerNumber"]`, "the refusal names the input")
	assert.Equal(t, contract.Unknown, e.Observe().State, "the cluster")
	for _, name := range []string{robotcluster.ResourceDedicatedServer, robotcluster.ResourceVSwitch, robotcluster.ResourceRobotFirewall} {
		obs, err := e.Cluster.Observers()[name].ObserveResource(context.Background(), contract.ResourceRef{Desired: e.Desired()})
		require.NoError(t, err, name)
		assert.Equal(t, contract.Unknown, obs.State, "%s: %s", name, obs.StatusDetail)
	}
	refusedAs(t, e.Delete(), "robot-left-in-place")
	assert.Zero(t, e.Cloud.Count(), "the Cloud resources left after the unsubscribe")
	assert.Len(t, e.Robot.Requests(), requests, "the Robot fake's calls after the number was dropped")
}

func TestCloudOnlyCluster_HasEveryComponentApplicationSyncedWithItsObjectsAppliedAtTheRootApplicationsHTTPSAddress(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)

	obs, err := e.Settle()

	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	root, err := url.Parse(e.RootAddress())
	require.NoError(t, err, "the root Application's address")
	assert.Equal(t, "https", root.Scheme, "the scheme a cluster without hosting reads its repository over")
	for file, content := range componentApplicationFiles(t, e) {
		assert.Equal(t, e.RootAddress(), repoURLOf(t, content), "the address %s reads the repository at", file)
	}
	assertEverySyncedWithItsObjects(t, componentApplications(t, e))
}

func TestCloudOnlyCluster_UpgradeMovesEveryComponentApplicationAtTheSSHAddressToTheRootApplicationsAddressInOneCommit(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	_, err := e.Settle()
	require.NoError(t, err)
	held := componentApplicationFiles(t, e)
	for file, content := range held {
		require.Contains(t, content, strconv.Quote(e.RootAddress()), "the address %s reads before the upgrade's stand-in", file)
		e.GitHub.Plant(e.Repo(), file, strings.Replace(content, strconv.Quote(e.RootAddress()), strconv.Quote(components.RepositorySSHURL(e.Repo())), 1))
	}
	e.Progress()
	for name, app := range componentApplications(t, e) {
		require.Equal(t, "Unknown", app.Sync, "%s's sync status at the SSH address", name)
	}
	before := len(e.GitHub.Landed(e.Repo()))

	obs, err := e.Settle()

	require.NoError(t, err, "the upgrade's subscribe: %s", obs.StatusDetail)
	var moves [][]string
	for _, commit := range e.GitHub.Landed(e.Repo())[before:] {
		if moved := slices.DeleteFunc(slices.Clone(commit), func(file string) bool { _, ok := held[file]; return !ok }); len(moved) > 0 {
			moves = append(moves, moved)
		}
	}
	assert.Equal(t, [][]string{slices.Sorted(maps.Keys(held))}, moves, "the commits that change a component Application")
	for file, content := range componentApplicationFiles(t, e) {
		assert.Equal(t, e.RootAddress(), repoURLOf(t, content), "the address %s reads the repository at after the upgrade", file)
	}
	assertEverySyncedWithItsObjects(t, componentApplications(t, e))
}

// reportedApplication is one Argo CD Application as the node report lists it.
type reportedApplication struct {
	Sync      string
	Resources []struct{ Kind, Name, Sync string }
}

// componentApplications is each component's Application the stand-in Argo CD reports, by name.
func componentApplications(t *testing.T, e *robotclustertest.Env) map[string]reportedApplication {
	t.Helper()
	var report struct {
		ArgoCD struct {
			Applications []struct {
				Name string
				reportedApplication
			}
		} `json:"argocd"`
	}
	require.NoError(t, json.Unmarshal(e.Argo.Report(), &report))
	apps := map[string]reportedApplication{}
	for _, app := range report.ArgoCD.Applications {
		if strings.HasPrefix(app.Name, rollout.ApplicationPrefix) {
			apps[app.Name] = app.reportedApplication
		}
	}
	require.NotEmpty(t, apps, "the component Applications Argo CD reports")
	return apps
}

func assertEverySyncedWithItsObjects(t *testing.T, apps map[string]reportedApplication) {
	t.Helper()
	for name, app := range apps {
		assert.Equal(t, "Synced", app.Sync, "%s's sync status", name)
		assert.NotEmpty(t, app.Resources, "the objects %s applied", name)
		for _, r := range app.Resources {
			assert.Equal(t, "Synced", r.Sync, "%s's %s %s", name, r.Kind, r.Name)
		}
	}
}

// componentApplicationFiles is each component Application's file the cluster repository holds, by its path.
func componentApplicationFiles(t *testing.T, e *robotclustertest.Env) map[string]string {
	t.Helper()
	files := map[string]string{}
	for file, content := range e.GitHub.RepoFiles(e.Repo()) {
		if strings.HasPrefix(path.Base(file), rollout.ApplicationPrefix) {
			files[file] = content
		}
	}
	require.NotEmpty(t, files, "the component Applications the cluster repository holds")
	return files
}

func repoURLOf(t *testing.T, application string) string {
	t.Helper()
	address, _ := testutil.Field(t, testutil.Documents(t, []byte(application))[0], "spec", "source", "repoURL").(string)
	require.NotEmpty(t, address, "the address the Application reads")
	return address
}
