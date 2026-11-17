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
// member each, up to the maximum, once every member has been read Ready since its creation. With no pod waiting and
// every member Ready, one member that ran no pod but the cluster's own for the pool's idle window goes, down to the
// minimum. A pool of a set size neither grows nor shrinks here.
func Decide(pool pools.CloudPool, members []controlplane.Server, reads []report.Read) (Decision, error) {
	held := Decision{Want: min(max(len(members), pool.Min), pool.Max)}
	if !pool.Scales() {
		return Decision{Want: pool.Max}, nil
	}
	if len(reads) == 0 {
		return held, nil
	}
	reads = slices.SortedFunc(slices.Values(reads), func(a, b report.Read) int { return a.At.Compare(b.At) })
	newest := reads[len(reads)-1]
	waiting, err := newest.Unschedulable()
	if err != nil {
		return Decision{}, err
	}
	ready, err := everyMemberReady(members, reads)
	if err != nil || !ready {
		return held, err
	}
	if len(waiting) > 0 {
		return grow(pool, len(members), len(waiting)), nil
	}
	return shrink(pool, members, reads, held)
}

func grow(pool pools.CloudPool, members, waiting int) Decision {
	if members >= pool.Max {
		return Decision{Want: pool.Max, AtMaximum: true,
			Detail: fmt.Sprintf("%d pods wait for a node, and the Cloud pool holds its maximum of %d", waiting, pool.Max)}
	}
	want := min(members+waiting, pool.Max)
	return Decision{Want: want, Detail: fmt.Sprintf("%d pods wait for a node, so the Cloud pool grows from %d to %d of at most %d", waiting, members, want, pool.Max)}
}

// shrink chooses, of the members idle for the window, the newest of the location holding the most members.
func shrink(pool pools.CloudPool, members []controlplane.Server, reads []report.Read, held Decision) (Decision, error) {
	if len(members) <= pool.Min {
		return held, nil
	}
	var idle []controlplane.Server
	spans := map[int64]time.Duration{}
	for _, m := range members {
		span, err := idleSpan(m, reads)
		if err != nil {
			return Decision{}, err
		}
		if span >= pool.IdleWindow {
			idle, spans[m.ID] = append(idle, m), span
		}
	}
	if len(idle) == 0 {
		return held, nil
	}
	leaving := newestOfFullestLocation(idle, members)
	return Decision{Want: len(members) - 1, Remove: &leaving, Detail: fmt.Sprintf(
		"Cloud worker %s has run no pod but the cluster's own for %s, past the pool's idle window of %s, so the pool shrinks from %d to %d",
		leaving.Labels[pools.KeyMember], spans[leaving.ID], pool.IdleWindow, len(members), len(members)-1)}, nil
}

// everyMemberReady is whether each member has been read Ready since its creation and the newest report reads it Ready,
// so no member still joins and none has failed when the pool decides.
func everyMemberReady(members []controlplane.Server, reads []report.Read) (bool, error) {
	for _, m := range members {
		readyAt, err := readyReads(m, reads)
		if err != nil {
			return false, err
		}
		if len(readyAt) == 0 || !readyAt[len(readyAt)-1] {
			return false, nil
		}
	}
	return true, nil
}

// readyReads is whether each report made after m's creation reads its node Ready, oldest first, a node it does not
// list included as not Ready.
func readyReads(m controlplane.Server, reads []report.Read) ([]bool, error) {
	var out []bool
	for _, r := range reads {
		if !r.At.After(m.Created) {
			continue
		}
		nodes, err := r.Nodes([]string{m.Name})
		if err != nil {
			return nil, err
		}
		out = append(out, nodes[0].Reading != nil && nodes[0].Reading.Serving)
	}
	return out, nil
}

// idleSpan is how long, from the oldest to the newest of the reports in a row that end with the newest, each made
// after m's creation, m ran no pod but the cluster's own; a member busy in the newest report is idle for none.
func idleSpan(m controlplane.Server, reads []report.Read) (time.Duration, error) {
	newest := reads[len(reads)-1].At
	since := newest
	for i := len(reads) - 1; i >= 0 && reads[i].At.After(m.Created); i-- {
		busy, err := reads[i].WorkloadPods(m.Name)
		if err != nil {
			return 0, err
		}
		if len(busy) > 0 {
			break
		}
		since = reads[i].At
	}
	return newest.Sub(since), nil
}

func newestOfFullestLocation(idle, members []controlplane.Server) controlplane.Server {
	count := func(loc string) int {
		return len(slices.DeleteFunc(slices.Clone(members), func(m controlplane.Server) bool { return m.Location != loc }))
	}
	return slices.MaxFunc(idle, func(a, b controlplane.Server) int {
		if c := count(a.Location) - count(b.Location); c != 0 {
			return c
		}
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
}
