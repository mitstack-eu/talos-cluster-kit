// Package scaling decides how a Cloud pool whose minimum is below its maximum follows its cluster's load, from the
// node reports alone, so the block's operator, which alone holds the Hetzner token, changes the pool and nothing in the
// cluster does.
package scaling

import (
	"fmt"
	"slices"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// CauseAtMaximum is the resource-health cause of a pool that holds its maximum while pods wait for a node, as the
// cluster type's failures name it.
const CauseAtMaximum = "the Cloud pool holds its maximum while pods wait for a node; raise cloudPoolMax or lower the load"

// Decision is what a Cloud pool should hold now.
type Decision struct {
	// Want is how many members the pool should hold.
	Want int
	// Remove is the idle member to drain and delete, set only when Want is one below the members.
	Remove *controlplane.Server
	// AtMaximum is whether pods wait while the pool holds its maximum.
	AtMaximum bool
	// Detail says why the pool changes or is held, for the subscription's events and its resource health.
	Detail string
}

// Decide judges pool against its members and the node reports reads holds, in any order. Pods no node can take add a
// member each, from at least the minimum up to the maximum, once every member has been read Ready since its creation.
// With no pod waiting and every member Ready, one member that ran no pod but the cluster's own for the pool's idle
// window goes, down to the minimum. The pool holds while its own removal of a member is under way, and a pool of a set
// size neither grows nor shrinks here.
func Decide(pool pools.CloudPool, members []controlplane.Server, reads []report.Read) (Decision, error) {
	held := Decision{Want: min(max(len(members), pool.Min), pool.Max)}
	if !pool.Scales() {
		return Decision{Want: pool.Max}, nil
	}
	if len(reads) == 0 || slices.ContainsFunc(members, pools.RemovalUnderWay) {
		return held, nil
	}
	reads = slices.SortedFunc(slices.Values(reads), func(a, b report.Read) int { return a.At.Compare(b.At) })
	waiting, err := reads[len(reads)-1].Unschedulable()
	if err != nil {
		return Decision{}, err
	}
	looks := lookAt(members, reads)
	if !everyMemberReady(members, looks) {
		return held, nil
	}
	if len(waiting) > 0 {
		return grow(pool, len(members), len(waiting)), nil
	}
	return shrink(pool, members, looks, held), nil
}

func grow(pool pools.CloudPool, members, waiting int) Decision {
	if members >= pool.Max {
		return Decision{Want: pool.Max, AtMaximum: true,
			Detail: fmt.Sprintf("%d pods wait for a node, and the Cloud pool holds its maximum of %d", waiting, pool.Max)}
	}
	want := min(max(members+waiting, pool.Min), pool.Max)
	return Decision{Want: want, Detail: fmt.Sprintf("%d pods wait for a node, so the Cloud pool grows from %d to %d of at most %d", waiting, members, want, pool.Max)}
}

// shrink chooses, of the members idle for the window, the newest of the location holding the most members.
func shrink(pool pools.CloudPool, members []controlplane.Server, looks []look, held Decision) Decision {
	if len(members) <= pool.Min {
		return held
	}
	var idle []controlplane.Server
	spans := map[int64]time.Duration{}
	for _, m := range members {
		if span := idleSpan(m, looks); span >= pool.IdleWindow {
			idle, spans[m.ID] = append(idle, m), span
		}
	}
	if len(idle) == 0 {
		return held
	}
	leaving := pools.NewestOfFullest(members, idle, pool.Locations)
	return Decision{Want: len(members) - 1, Remove: &leaving, Detail: fmt.Sprintf(
		"Cloud worker %s has run no pod but the cluster's own for %s, past the pool's idle window of %s, so the pool shrinks from %d to %d",
		leaving.Labels[pools.KeyMember], spans[leaving.ID], pool.IdleWindow, len(members), len(members)-1)}
}

// look is what one node report read of the members. A report whose nodes or pods could not be read is unread, and
// shows no member joining and none idle, so one failed read in the cluster stalls no later decision.
type look struct {
	at     time.Time
	unread bool
	ready  map[string]bool
	busy   map[string][]string
}

func lookAt(members []controlplane.Server, reads []report.Read) []look {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	looks := make([]look, 0, len(reads))
	for _, r := range reads {
		nodes, err := r.Nodes(names)
		busy, perr := r.WorkloadPods()
		l := look{at: r.At, unread: err != nil || perr != nil, ready: map[string]bool{}, busy: busy}
		for _, n := range nodes {
			l.ready[n.Name] = n.Reading != nil && n.Reading.Serving
		}
		looks = append(looks, l)
	}
	return looks
}

// everyMemberReady is whether each member has been read since its creation and the newest such report reads it Ready,
// so no member still joins and none has failed when the pool decides.
func everyMemberReady(members []controlplane.Server, looks []look) bool {
	for _, m := range members {
		read, ready := false, false
		for _, l := range looks {
			if !l.unread && l.at.After(m.Created) {
				read, ready = true, l.ready[m.Name]
			}
		}
		if !read || !ready {
			return false
		}
	}
	return true
}

// idleSpan is how long m ran no pod but the cluster's own over the reports in a row that end with the newest, each
// made after m's creation and after a removal last kept it; a report that could not be read ends the row.
func idleSpan(m controlplane.Server, looks []look) time.Duration {
	from := m.Created
	if kept, ok := pools.KeptAt(m); ok && kept.After(from) {
		from = kept
	}
	newest := looks[len(looks)-1].at
	since := newest
	for i := len(looks) - 1; i >= 0 && looks[i].at.After(from) && !looks[i].unread && len(looks[i].busy[m.Name]) == 0; i-- {
		since = looks[i].at
	}
	return newest.Sub(since)
}
