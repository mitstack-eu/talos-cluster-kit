package robotcluster_test

// A Cloud pool keeps its minimum of steady members, which carry no burst label, marks each member above it burst, and
// holds rather than grows for pods whose placement refuses every burst member.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster/robotclustertest"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func poolSize(min, max string) robotclustertest.Option {
	return robotclustertest.WithParams(map[string]string{"cloudPoolMin": min, "cloudPoolMax": max})
}

// burstMembers lists the member keys of the pool's burst members, in key order.
func burstMembers(e *robotclustertest.Env) []string {
	var keys []string
	for _, w := range e.ByRole(controlplane.RoleCloudWorker) {
		if pools.IsBurst(w) {
			keys = append(keys, w.Labels[pools.KeyMember])
		}
	}
	slices.Sort(keys)
	return keys
}

func settleWith(e *robotclustertest.Env, min, max string) {
	e.T.Helper()
	e.Params["cloudPoolMin"], e.Params["cloudPoolMax"] = min, max
	obs, err := e.Settle()
	require.NoError(e.T, err, "the subscribe with a pool of %s to %s: %s", min, max, obs.StatusDetail)
	e.EnsureOrWait("the subscribe that carries the release")
}

func TestPool_UpgradedFromASetSizeToScaleMarksTheNewestMembersBurstAndBackToASetSizeMarksEveryMemberSteady(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, poolSize("3", "3"))
	settleWith(e, "3", "3")
	require.Empty(t, burstMembers(e), "a pool of a set size holds no burst member")
	ids := controlplane.IDs(e.ByRole(controlplane.RoleCloudWorker), controlplane.ServerID)

	settleWith(e, "1", "4")

	assert.Equal(t, []string{"fsn1-2", "fsn1-3"}, burstMembers(e), "the two newest members, above the minimum of 1")
	assert.ElementsMatch(t, ids, controlplane.IDs(e.ByRole(controlplane.RoleCloudWorker), controlplane.ServerID), "no member is replaced")
	burstNodes := e.LabellerBurstNodes()
	for _, w := range e.ByRole(controlplane.RoleCloudWorker) {
		if pools.IsBurst(w) {
			assert.Contains(t, burstNodes, w.Name, "the labeller names the burst member")
		} else {
			assert.NotContains(t, burstNodes, w.Name, "the labeller leaves the steady member")
		}
	}

	settleWith(e, "3", "3")

	assert.Empty(t, burstMembers(e), "a pool back at a set size holds no burst member")
	assert.Empty(t, e.LabellerBurstNodes(), "the labeller names no burst node")
	assert.ElementsMatch(t, ids, controlplane.IDs(e.ByRole(controlplane.RoleCloudWorker), controlplane.ServerID), "no member is replaced")
}

func TestPool_ReplacesALostSteadyMemberWithASteadyOneAndKeepsItsBurstMember(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly, poolSize("3", "3"))
	settleWith(e, "3", "3")
	settleWith(e, "2", "4")
	require.Equal(t, []string{"fsn1-3"}, burstMembers(e), "the member above the minimum of 2")
	before := e.ByRole(controlplane.RoleCloudWorker)
	lost := before[slices.IndexFunc(before, func(w controlplane.Server) bool { return w.Labels[pools.KeyMember] == "fsn1-1" })]

	require.NoError(t, e.Cloud.DeleteServer(context.Background(), lost.ID), "the delete outside mitstack")
	settleWith(e, "2", "4")

	after := e.ByRole(controlplane.RoleCloudWorker)
	require.Len(t, after, 3, "the pool back at its two steady members and its burst member")
	assert.Equal(t, []string{"fsn1-3"}, burstMembers(e), "the burst member keeps its label")
	for _, w := range after {
		if !slices.ContainsFunc(before, func(b controlplane.Server) bool { return b.ID == w.ID }) {
			assert.False(t, pools.IsBurst(w), "the member created for the lost one is steady")
		}
	}
}

// componentWaiting is a pod of the kit's own that waits with the node affinity the render pass gives it.
var componentWaiting = map[string]any{
	"namespace": "argocd", "name": "argocd-repo-server-7d9f", "phase": "Pending", "unschedulable": true,
	"placement": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{
		map[string]any{"key": pools.KeyBurst, "operator": "DoesNotExist"},
	}}}},
}

func TestPool_HoldsWhilePodsWaitWhosePlacementRefusesEveryBurstMemberAndReadsUnhealthyNamingThem(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	settleWith(e, "1", "4")
	ids := controlplane.IDs(e.ByRole(controlplane.RoleCloudWorker), controlplane.ServerID)

	e.WaitingPods = []map[string]any{componentWaiting}
	for range 5 {
		e.Now = e.Now.Add(time.Minute)
		e.Progress()
		e.EnsureOrWait("a subscribe while the pod waits")
	}
	obs := e.Observe()

	assert.ElementsMatch(t, ids, controlplane.IDs(e.ByRole(controlplane.RoleCloudWorker), controlplane.ServerID), "the pool adds no member")
	require.NotNil(t, obs.Unhealthy, "the cluster names the hold: %s", obs.StatusDetail)
	assert.Contains(t, obs.Unhealthy.Cause, "cloudPoolMin", "the cause names the input that adds steady members")
	assert.Contains(t, obs.Unhealthy.Detail, "argocd/argocd-repo-server-7d9f", "the detail names the waiting pod")
	assert.Contains(t, obs.Unhealthy.Detail, "refuses every burst member", "the detail says why the pool holds")
}

// labellerRun is the pod of the node labeller's run scheduled at minute, as the node report carries a finished one.
func labellerRun(minute, phase, message string) map[string]any {
	return map[string]any{"namespace": "mitstack-node", "name": "mitstack-node-topology-" + minute + "-x7k2q", "phase": phase,
		"containers": []any{map[string]any{"name": "label", "message": message}}}
}

func TestPool_ReadsUnhealthyNamingTheBudgetAndThePodWhileTheLabellersNewestRunFailedOnABudgetHold(t *testing.T) {
	e := robotclustertest.New(t, cloudOnlySub, cloudOnly)
	settleWith(e, "1", "4")
	held := "label-nodes: pod argocd/argocd-server-0 stays on burst node fsn1-3 until a later run evicts it: " +
		"The disruption budget argocd-server needs 1 healthy pods and has 1 currently"

	e.WaitingPods = []map[string]any{labellerRun("29400000", "Succeeded", ""),
		labellerRun("29400005", "Failed", "til its disruption budget allows: Cannot evict pod\n"+held+"\n")}
	e.Now = e.Now.Add(time.Minute)
	e.Progress()
	obs := e.Observe()

	require.NotNil(t, obs.Unhealthy, "the cluster names the labeller's failed run: %s", obs.StatusDetail)
	assert.Contains(t, obs.Unhealthy.Cause, "node labeller")
	assert.Equal(t, held, obs.Unhealthy.Detail, "the detail is the run's last line, naming the pod, its burst member and the budget")

	e.WaitingPods = append(e.WaitingPods, labellerRun("29400010", "Succeeded", ""))
	e.Now = e.Now.Add(time.Minute)
	e.Progress()
	assert.Nil(t, e.Observe().Unhealthy, "a later run that succeeded clears it")
}
