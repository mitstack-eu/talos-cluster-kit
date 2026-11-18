package robotcluster_test

// A Cloud pool keeps its minimum of steady members, which carry no burst label, marks each member above it burst, and
// holds rather than grows for pods whose placement refuses every burst member.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func poolSize(min, max string) envOption {
	return withParams(map[string]string{"cloudPoolMin": min, "cloudPoolMax": max})
}

// burstMembers lists the member keys of the pool's burst members, in key order.
func (e *env) burstMembers() []string {
	var keys []string
	for _, w := range e.byRole(controlplane.RoleCloudWorker) {
		if pools.IsBurst(w) {
			keys = append(keys, w.Labels[pools.KeyMember])
		}
	}
	slices.Sort(keys)
	return keys
}

func (e *env) settleWith(min, max string) {
	e.t.Helper()
	e.params["cloudPoolMin"], e.params["cloudPoolMax"] = min, max
	obs, err := e.settle()
	require.NoError(e.t, err, "the subscribe with a pool of %s to %s: %s", min, max, obs.StatusDetail)
	e.ensureOrWait("the subscribe that carries the release")
}

func TestPool_UpgradedFromASetSizeToScaleMarksTheNewestMembersBurstAndBackToASetSizeMarksEveryMemberSteady(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly, poolSize("3", "3"))
	e.settleWith("3", "3")
	require.Empty(t, e.burstMembers(), "a pool of a set size holds no burst member")
	ids := controlplane.IDs(e.byRole(controlplane.RoleCloudWorker), controlplane.ServerID)

	e.settleWith("1", "4")

	assert.Equal(t, []string{"fsn1-2", "fsn1-3"}, e.burstMembers(), "the two newest members, above the minimum of 1")
	assert.ElementsMatch(t, ids, controlplane.IDs(e.byRole(controlplane.RoleCloudWorker), controlplane.ServerID), "no member is replaced")
	burstNodes := e.labellerBurstNodes()
	for _, w := range e.byRole(controlplane.RoleCloudWorker) {
		if pools.IsBurst(w) {
			assert.Contains(t, burstNodes, w.Name, "the labeller names the burst member")
		} else {
			assert.NotContains(t, burstNodes, w.Name, "the labeller leaves the steady member")
		}
	}

	e.settleWith("3", "3")

	assert.Empty(t, e.burstMembers(), "a pool back at a set size holds no burst member")
	assert.Empty(t, e.labellerBurstNodes(), "the labeller names no burst node")
	assert.ElementsMatch(t, ids, controlplane.IDs(e.byRole(controlplane.RoleCloudWorker), controlplane.ServerID), "no member is replaced")
}

func TestPool_ReplacesALostSteadyMemberWithASteadyOneAndKeepsItsBurstMember(t *testing.T) {
	e := newEnv(t, cloudOnlySub, cloudOnly, poolSize("3", "3"))
	e.settleWith("3", "3")
	e.settleWith("2", "4")
	require.Equal(t, []string{"fsn1-3"}, e.burstMembers(), "the member above the minimum of 2")
	before := e.byRole(controlplane.RoleCloudWorker)
	lost := before[slices.IndexFunc(before, func(w controlplane.Server) bool { return w.Labels[pools.KeyMember] == "fsn1-1" })]

	require.NoError(t, e.cloud.DeleteServer(context.Background(), lost.ID), "the delete outside mitstack")
	e.settleWith("2", "4")

	after := e.byRole(controlplane.RoleCloudWorker)
	require.Len(t, after, 3, "the pool back at its two steady members and its burst member")
	assert.Equal(t, []string{"fsn1-3"}, e.burstMembers(), "the burst member keeps its label")
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
	e := newEnv(t, cloudOnlySub, cloudOnly)
	e.settleWith("1", "4")
	ids := controlplane.IDs(e.byRole(controlplane.RoleCloudWorker), controlplane.ServerID)

	e.waitingPods = []map[string]any{componentWaiting}
	for range 5 {
		e.now = e.now.Add(time.Minute)
		e.progress()
		e.ensureOrWait("a subscribe while the pod waits")
	}
	obs := e.observe()

	assert.ElementsMatch(t, ids, controlplane.IDs(e.byRole(controlplane.RoleCloudWorker), controlplane.ServerID), "the pool adds no member")
	require.NotNil(t, obs.Unhealthy, "the cluster names the hold: %s", obs.StatusDetail)
	assert.Contains(t, obs.Unhealthy.Cause, "cloudPoolMin", "the cause names the input that adds steady members")
	assert.Contains(t, obs.Unhealthy.Detail, "argocd/argocd-repo-server-7d9f", "the detail names the waiting pod")
	assert.Contains(t, obs.Unhealthy.Detail, "refuses every burst member", "the detail says why the pool holds")
}
