package scaling_test

// A Cloud pool that scales grows by the pods no node can take, up to its maximum, and loses a member that ran no pod
// but the cluster's own for its idle window, down to its minimum.

import (
	"encoding/json"
	"strconv"
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

func scaled(lowest, highest int) pools.CloudPool {
	return pools.CloudPool{ServerTypes: []string{"cpx32"}, Locations: []string{"fsn1", "nbg1"}, Min: lowest, Max: highest, IdleWindow: 15 * time.Minute}
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

// daemons are the pods that leave a node idle: a DaemonSet's, a static pod's, any in kube-system and a finished pod's.
func daemons(on controlplane.Server) []pod {
	return []pod{
		{Namespace: "kube-system", Name: "cilium-" + on.Name, Node: ptr(on.Name), Phase: "Running", OwnerKind: ptr("DaemonSet")},
		{Namespace: "kube-system", Name: "static-" + on.Name, Node: ptr(on.Name), Phase: "Running", OwnerKind: ptr("Node")},
		{Namespace: "kube-system", Name: "coredns-" + on.Name, Node: ptr(on.Name), Phase: "Running", OwnerKind: ptr("ReplicaSet")},
		{Namespace: "ci", Name: "done-" + on.Name, Node: ptr(on.Name), Phase: "Succeeded", OwnerKind: ptr("Job")},
	}
}

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

func every(t *testing.T, since, until time.Time, members []controlplane.Server, pods ...pod) []report.Read {
	t.Helper()
	var reads []report.Read
	for at := since; !at.After(until); at = at.Add(report.Interval) {
		reads = append(reads, read(t, at, members, pods...))
	}
	return reads
}

func TestDecide_PodsThatWaitAddOneMemberForEachUpToTheMaximumAndNeverPastIt(t *testing.T) {
	joined := member(1, "fsn1-1", start)
	full := []controlplane.Server{joined, member(2, "nbg1-1", start), member(3, "fsn1-2", start)}
	cases := []struct {
		name    string
		pool    pools.CloudPool
		members []controlplane.Server
		pods    []pod
		want    scaling.Decision
	}{
		{"an empty pool of at most 3 with two pods waiting grows to 2", scaled(0, 3), nil, []pod{waiting("a"), waiting("b")},
			scaling.Decision{Want: 2, Detail: "2 pods wait for a node, so the Cloud pool grows from 0 to 2 of at most 3"}},
		{"an empty pool of at most 3 with five pods waiting grows to 3 and no more", scaled(0, 3), nil, []pod{waiting("a"), waiting("b"), waiting("c"), waiting("d"), waiting("e")},
			scaling.Decision{Want: 3, Detail: "5 pods wait for a node, so the Cloud pool grows from 0 to 3 of at most 3"}},
		{"a pool of one busy member with one pod waiting grows to 2", scaled(0, 3), []controlplane.Server{joined}, []pod{job("busy", joined), waiting("a")},
			scaling.Decision{Want: 2, Detail: "1 pods wait for a node, so the Cloud pool grows from 1 to 2 of at most 3"}},
		{"a pool with no pod waiting keeps its size", scaled(0, 3), []controlplane.Server{joined}, []pod{job("busy", joined)}, scaling.Decision{Want: 1}},
		{"one waiting pod grows a pool of at least 3 holding none to its minimum", scaled(3, 6), nil, []pod{waiting("a")},
			scaling.Decision{Want: 3, Detail: "1 pods wait for a node, so the Cloud pool grows from 0 to 3 of at most 6"}},
		{"a pool at its maximum with pods waiting creates no member and reads held at its maximum", scaled(0, 3), full,
			[]pod{job("a", full[0]), job("b", full[1]), job("c", full[2]), waiting("d"), waiting("e")},
			scaling.Decision{Want: 3, AtMaximum: true, Detail: "2 pods wait for a node, and the Cloud pool holds its maximum of 3: ci/d, ci/e"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scaling.Decide(tc.pool, tc.members, []report.Read{read(t, start.Add(5*time.Minute), tc.members, tc.pods...)})

			require.NoError(t, err)
			assert.Equal(t, tc.want, got, "the pool's decision")
		})
	}
}

const unreadPods = `{"nodes": [], "pods": {"exitStatus": 1, "stderr": "the server was unable to return a response"}}`

func TestDecide_HoldsThePoolWhenNoRuleChangesIt(t *testing.T) {
	m, kept := member(1, "fsn1-1", start), member(1, "fsn1-1", start)
	kept.Labels[pools.KeyRemovalKept] = strconv.FormatInt(start.Add(20*time.Minute).Unix(), 10)
	pair := []controlplane.Server{member(1, "fsn1-1", start), member(2, "nbg1-1", start)}
	pair[0].Labels = controlplane.BeginRemoval(pair[0], start.Add(28*time.Minute), "cloud-pool")
	idle := func(members ...controlplane.Server) []report.Read {
		return every(t, start.Add(5*time.Minute), start.Add(30*time.Minute), members)
	}
	gap := idle(m)
	gap[4].Body = []byte(unreadPods)
	cases := []struct {
		name    string
		pool    pools.CloudPool
		members []controlplane.Server
		reads   []report.Read
		want    int
	}{
		{"a member created since the newest report, not yet read Ready, while pods wait", scaled(0, 3),
			[]controlplane.Server{member(1, "fsn1-1", start.Add(6*time.Minute))}, []report.Read{read(t, start.Add(5*time.Minute), nil, waiting("a"), waiting("b"))}, 1},
		{"a member idle for 10 minutes of a 15-minute window", scaled(0, 3), []controlplane.Server{m}, append([]report.Read{read(t, start.Add(5*time.Minute), []controlplane.Server{m}, job("a", m))},
			every(t, start.Add(10*time.Minute), start.Add(20*time.Minute), []controlplane.Server{m})...), 1},
		{"a member running a Job's pod in every report", scaled(0, 3), []controlplane.Server{m}, every(t, start.Add(5*time.Minute), start.Add(time.Hour), []controlplane.Server{m}, job("long", m)), 1},
		{"a member read idle for 25 minutes but not by the report at 25 minutes, which could not be read", scaled(0, 3), []controlplane.Server{m}, gap, 1},
		{"a member idle for 25 minutes and kept from its removal 10 minutes ago", scaled(0, 3), []controlplane.Server{kept}, idle(kept), 1},
		{"two idle members while the pool removes fsn1-1 for a lowered maximum", scaled(0, 3), pair, idle(pair...), 2},
		{"a pool of at least 1 before any report", scaled(1, 3), nil, nil, 1},
		{"a pool of at least 1 keeps its one idle member, never falling below its minimum", scaled(1, 3), []controlplane.Server{m}, idle(m), 1},
		{"a pool of at least 2 holding none grows to its minimum with no pod waiting", scaled(2, 3), nil, idle(), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scaling.Decide(tc.pool, tc.members, tc.reads)

			require.NoError(t, err)
			assert.Equal(t, scaling.Decision{Want: tc.want}, got, "the pool's decision")
		})
	}
}

func TestDecide_AMemberIdlePastTheWindowIsRemovedDownToTheMinimum(t *testing.T) {
	idle := member(1, "fsn1-1", start)
	reads := every(t, start.Add(5*time.Minute), start.Add(25*time.Minute), []controlplane.Server{idle})

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{idle}, reads)

	require.NoError(t, err)
	require.NotNil(t, got.Remove, "the member the pool removes")
	assert.Equal(t, idle.ID, got.Remove.ID, "the member the pool removes")
	assert.Equal(t, 0, got.Want, "the members the pool wants")
	assert.Equal(t, "Cloud worker fsn1-1 has run no pod but the cluster's own for 20m0s, past the pool's idle window of 15m0s, so the pool shrinks from 1 to 0", got.Detail)
}

func TestDecide_RemovesNoMemberWhilePodsWaitAndGrowsPastAnOlderReportThatCouldNotBeRead(t *testing.T) {
	idle := member(1, "fsn1-1", start)
	reads := every(t, start.Add(5*time.Minute), start.Add(25*time.Minute), []controlplane.Server{idle})
	reads[2].Body = []byte(unreadPods)
	reads = append(reads, read(t, start.Add(30*time.Minute), []controlplane.Server{idle}, waiting("a")))

	got, err := scaling.Decide(scaled(0, 3), []controlplane.Server{idle}, reads)

	require.NoError(t, err)
	assert.Nil(t, got.Remove, "the member the pool removes while a pod waits")
	assert.Equal(t, 2, got.Want, "the members the pool wants")
}

func TestDecide_OfSeveralIdleMembersRemovesTheNewestOfTheLocationHoldingTheMost(t *testing.T) {
	members := []controlplane.Server{member(1, "fsn1-1", start), member(2, "nbg1-1", start.Add(time.Minute)), member(3, "fsn1-2", start.Add(2*time.Minute))}
	reads := every(t, start.Add(5*time.Minute), start.Add(30*time.Minute), members)

	got, err := scaling.Decide(scaled(0, 3), members, reads)

	require.NoError(t, err)
	require.NotNil(t, got.Remove, "the member the pool removes")
	assert.Equal(t, "fsn1-2", got.Remove.Labels[pools.KeyMember], "the newest member of fsn1, which holds two")
	assert.Equal(t, 2, got.Want, "the members the pool wants")
}

func TestDecide_APoolOfASetSizeNeitherGrowsNorShrinksNorReadsHeld(t *testing.T) {
	members := []controlplane.Server{member(1, "fsn1-1", start), member(2, "nbg1-1", start)}
	reads := every(t, start.Add(5*time.Minute), start.Add(time.Hour), members)
	reads = append(reads, read(t, start.Add(65*time.Minute), members, waiting("a")))

	got, err := scaling.Decide(scaled(2, 2), members, reads)

	require.NoError(t, err)
	assert.Equal(t, scaling.Decision{Want: 2}, got, "a pool of two whose members idle while a pod waits")
}

func TestDecide_FailsOnAReportThatIsNotTheNodeReportsJSON(t *testing.T) {
	_, err := scaling.Decide(scaled(0, 3), nil, []report.Read{{Body: []byte("not json"), At: start}})

	require.Error(t, err)
	assert.ErrorContains(t, err, start.Format(time.RFC3339), "the error names the report it could not read")
}
