package scaling_test

// A Cloud pool that scales grows by the pods no node can take, up to its maximum, and loses a member that ran no pod
// but the cluster's own for its idle window, down to its minimum.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/scaling"
)

var start = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

const window = 15 * time.Minute

// scaled is a pool of between lowest and highest workers over fsn1 and nbg1 that drops a member idle for 15 minutes.
func scaled(lowest, highest int) pools.CloudPool {
	return pools.CloudPool{ServerTypes: []string{"cpx32"}, Locations: []string{"fsn1", "nbg1"}, Min: lowest, Max: highest, IdleWindow: window}
}

func member(id int64, key string, created time.Time) controlplane.Server {
	return controlplane.Server{ID: id, Name: "shop-cloud-worker-" + key, Location: key[:4], Created: created,
		Labels: map[string]string{pools.KeyMember: key}}
}

type pod struct {
	Namespace     string  `json:"namespace"`
	Name          string  `json:"name"`
	Node          *string `json:"node"`
	Phase         string  `json:"phase"`
	OwnerKind     *string `json:"ownerKind"`
	Unschedulable bool    `json:"unschedulable"`
}

func ptr(s string) *string { return &s }

func waiting(name string) pod {
	return pod{Namespace: "ci", Name: name, Phase: "Pending", OwnerKind: ptr("Job"), Unschedulable: true}
}

func job(name string, on controlplane.Server) pod {
	return pod{Namespace: "ci", Name: name, Node: ptr(on.Name), Phase: "Running", OwnerKind: ptr("Job")}
}

// daemons are the pods every node runs, which leave a node idle: a DaemonSet's, a static pod's, and a finished Job's.
func daemons(on controlplane.Server) []pod {
	return []pod{
		{Namespace: "kube-system", Name: "cilium-" + on.Name, Node: ptr(on.Name), Phase: "Running", OwnerKind: ptr("DaemonSet")},
		{Namespace: "kube-system", Name: "static-" + on.Name, Node: ptr(on.Name), Phase: "Running", OwnerKind: ptr("Node")},
		{Namespace: "ci", Name: "done-" + on.Name, Node: ptr(on.Name), Phase: "Succeeded", OwnerKind: ptr("Job")},
	}
}

// read is a node report made at, listing each member Ready with the pods it runs and the pods that wait.
func read(t *testing.T, at time.Time, members []controlplane.Server, pods ...pod) report.Read {
	t.Helper()
	nodes := []map[string]any{{"name": "node-fsn1", "ready": true}}
	for _, m := range members {
		nodes = append(nodes, map[string]any{"name": m.Name, "ready": true})
		pods = append(pods, daemons(m)...)
	}
	body, err := json.Marshal(map[string]any{"madeAt": at, "nodes": nodes, "pods": pods})
	require.NoError(t, err)
	return report.Read{Body: body, At: at}
}

// idleSince is a report every five minutes from since to until, in which the members run only the cluster's own pods.
func idleSince(t *testing.T, since, until time.Time, members ...controlplane.Server) []report.Read {
	t.Helper()
	var reads []report.Read
	for at := since; !at.After(until); at = at.Add(report.Interval) {
		reads = append(reads, read(t, at, members))
	}
	return reads
}

func TestDecide_PodsThatWaitAddOneMemberForEachUpToTheMaximumAndNeverPastIt(t *testing.T) {
	joined := member(1, "fsn1-1", start)
	cases := []struct {
		name      string
		members   []controlplane.Server
		pods      []pod
		want      int
		atMaximum bool
	}{
		{"an empty pool of at most 3 with two pods waiting grows to 2", nil, []pod{waiting("a"), waiting("b")}, 2, false},
		{"an empty pool of at most 3 with five pods waiting grows to 3 and no more", nil, []pod{waiting("a"), waiting("b"), waiting("c"), waiting("d"), waiting("e")}, 3, false},
		{"a pool of one busy member with one pod waiting grows to 2", []controlplane.Server{joined}, []pod{job("busy", joined), waiting("a")}, 2, false},
		{"a pool with no pod waiting keeps its size", []controlplane.Server{joined}, []pod{job("busy", joined)}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := []report.Read{read(t, start.Add(5*time.Minute), tc.members, tc.pods...)}

			got, err := scaling.Decide(scaled(0, 3), tc.members, reads)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Want, "the members the pool wants")
			assert.Nil(t, got.Remove, "the member the pool removes")
			assert.Equal(t, tc.atMaximum, got.AtMaximum, "whether pods wait at the maximum")
		})
	}
}

func TestDecide_APoolAtItsMaximumWithPodsWaitingCreatesNoMemberAndReadsHeldAtItsMaximum(t *testing.T) {
	members := []controlplane.Server{member(1, "fsn1-1", start), member(2, "nbg1-1", start), member(3, "fsn1-2", start)}
	pods := []pod{job("a", members[0]), job("b", members[1]), job("c", members[2]), waiting("d"), waiting("e")}

	got, err := scaling.Decide(scaled(0, 3), members, []report.Read{read(t, start.Add(5*time.Minute), members, pods...)})

	require.NoError(t, err)
	assert.Equal(t, 3, got.Want, "the members the pool wants")
	assert.True(t, got.AtMaximum, "whether pods wait at the maximum")
	assert.Equal(t, "2 pods wait for a node, and the Cloud pool holds its maximum of 3", got.Detail)
}

func TestDecide_AddsNoMemberWhileAMemberCreatedSinceTheNewestReportHasNotBeenReadReady(t *testing.T) {
	joining := member(1, "fsn1-1", start.Add(6*time.Minute))
	reads := []report.Read{read(t, start.Add(5*time.Minute), nil, waiting("a"), waiting("b"))}

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{joining}, reads)

	require.NoError(t, err)
	assert.Equal(t, 1, got.Want, "the members the pool wants while fsn1-1 joins")
	assert.Nil(t, got.Remove, "the member the pool removes")
}

func TestDecide_AMemberIdlePastTheWindowIsRemovedDownToTheMinimum(t *testing.T) {
	idle := member(1, "fsn1-1", start)
	reads := idleSince(t, start.Add(5*time.Minute), start.Add(25*time.Minute), idle)

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{idle}, reads)

	require.NoError(t, err)
	require.NotNil(t, got.Remove, "the member the pool removes")
	assert.Equal(t, idle.ID, got.Remove.ID, "the member the pool removes")
	assert.Equal(t, 0, got.Want, "the members the pool wants")
	assert.Equal(t, "Cloud worker fsn1-1 has run no pod but the cluster's own for 20m0s, past the pool's idle window of 15m0s, so the pool shrinks from 1 to 0", got.Detail)
}

func TestDecide_AMemberIdleForLessThanTheWindowStays(t *testing.T) {
	m := member(1, "fsn1-1", start)
	reads := append([]report.Read{read(t, start.Add(5*time.Minute), []controlplane.Server{m}, job("a", m))},
		idleSince(t, start.Add(10*time.Minute), start.Add(20*time.Minute), m)...)

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{m}, reads)

	require.NoError(t, err)
	assert.Nil(t, got.Remove, "a member idle for 10 minutes of a 15-minute window")
	assert.Equal(t, 1, got.Want, "the members the pool wants")
}

func TestDecide_AMemberRunningAPodIsNeverIdle(t *testing.T) {
	m := member(1, "fsn1-1", start)
	var reads []report.Read
	for at := start.Add(5 * time.Minute); !at.After(start.Add(time.Hour)); at = at.Add(report.Interval) {
		reads = append(reads, read(t, at, []controlplane.Server{m}, job("long", m)))
	}

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{m}, reads)

	require.NoError(t, err)
	assert.Nil(t, got.Remove, "a member that runs a Job's pod in every report")
}

func TestDecide_ThePoolNeverFallsBelowItsMinimum(t *testing.T) {
	idle := member(1, "fsn1-1", start)
	cases := []struct {
		name    string
		pool    pools.CloudPool
		members []controlplane.Server
		want    int
	}{
		{"a pool of at least 1 keeps its one idle member", scaled(1, 3), []controlplane.Server{idle}, 1},
		{"a pool of at least 2 holding none grows to 2", scaled(2, 3), nil, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := idleSince(t, start.Add(5*time.Minute), start.Add(time.Hour), tc.members...)

			got, err := scaling.Decide(tc.pool, tc.members, reads)

			require.NoError(t, err)
			assert.Nil(t, got.Remove, "the member the pool removes")
			assert.Equal(t, tc.want, got.Want, "the members the pool wants")
		})
	}
}

func TestDecide_RemovesNoMemberWhilePodsWait(t *testing.T) {
	idle := member(1, "fsn1-1", start)
	reads := idleSince(t, start.Add(5*time.Minute), start.Add(25*time.Minute), idle)
	reads = append(reads, read(t, start.Add(30*time.Minute), []controlplane.Server{idle}, waiting("a")))

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{idle}, reads)

	require.NoError(t, err)
	assert.Nil(t, got.Remove, "the member the pool removes while a pod waits")
	assert.Equal(t, 2, got.Want, "the members the pool wants")
}

func TestDecide_OfSeveralIdleMembersRemovesTheNewestOfTheLocationHoldingTheMost(t *testing.T) {
	members := []controlplane.Server{member(1, "fsn1-1", start), member(2, "nbg1-1", start.Add(time.Minute)), member(3, "fsn1-2", start.Add(2*time.Minute))}
	reads := idleSince(t, start.Add(5*time.Minute), start.Add(30*time.Minute), members...)

	got, err := scaling.Decide(scaled(0, 3), members, reads)

	require.NoError(t, err)
	require.NotNil(t, got.Remove, "the member the pool removes")
	assert.Equal(t, "fsn1-2", got.Remove.Labels[pools.KeyMember], "the newest member of fsn1, which holds two")
	assert.Equal(t, 2, got.Want, "the members the pool wants")
}

func TestDecide_APoolOfASetSizeNeitherGrowsNorShrinksNorReadsHeld(t *testing.T) {
	members := []controlplane.Server{member(1, "fsn1-1", start), member(2, "nbg1-1", start)}
	reads := idleSince(t, start.Add(5*time.Minute), start.Add(time.Hour), members...)
	reads = append(reads, read(t, start.Add(65*time.Minute), members, waiting("a")))
	set := scaled(2, 2)
	set.IdleWindow = 0

	got, err := scaling.Decide(set, members, reads)

	require.NoError(t, err)
	assert.Equal(t, scaling.Decision{Want: 2}, got, "a pool of two whose members idle while a pod waits")
}

func TestDecide_WithNoReportKeepsThePoolWithinItsBounds(t *testing.T) {
	got, err := scaling.Decide(scaled(1, 3), nil, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, got.Want, "the members a pool of at least 1 wants before any report")
	assert.Nil(t, got.Remove, "the member the pool removes")
}

func TestDecide_FailsOnAReportThatIsNotTheNodeReportsJSON(t *testing.T) {
	_, err := scaling.Decide(scaled(0, 3), nil, []report.Read{{Body: []byte("not json"), At: start}})

	require.Error(t, err)
	assert.ErrorContains(t, err, start.Format(time.RFC3339), "the error names the report it could not read")
}
