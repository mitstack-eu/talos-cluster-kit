package pools

import (
	"cmp"
	"context"
	"slices"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// Measured is what a running node of each server type offers pods, by the type's name.
type Measured map[string]report.Request

// MeasuredBy is the least each server type of members offers pods, in each of CPU and memory, by the serving,
// uncordoned nodes read reports of the members whose removal has not begun, and nil when no such node reports both.
func MeasuredBy(members []controlplane.Server, read report.Read) (Measured, error) {
	offers, err := read.Allocatable()
	if err != nil {
		return nil, err
	}
	var measured Measured
	for _, m := range members {
		room, ok := offers[m.Name]
		if _, leaving := m.Labels[controlplane.KeyRemovalBegan]; leaving || !ok {
			continue
		}
		if measured == nil {
			measured = Measured{}
		}
		if least, ok := measured[m.ServerType]; ok {
			room = report.Request{MilliCPU: min(room.MilliCPU, least.MilliCPU), Memory: min(room.Memory, least.Memory)}
		}
		measured[m.ServerType] = room
	}
	return measured, nil
}

// BurstNodeLabels are the labels the node of a burst member created in loc carries once it joins.
func BurstNodeLabels(loc string) map[string]string {
	labels := controlplane.TopologyLabels(loc)
	labels[KeyBurst] = components.BurstValue
	for _, prefix := range []string{"kubernetes.io/", "beta.kubernetes.io/"} {
		labels[prefix+"os"], labels[prefix+"arch"] = "linux", image.Arch
	}
	return labels
}

// Holds reports whether a burst member of one of pool's types, created in one of its locations, holds a waiting pod,
// reading the cluster, each location's server types and the release's DaemonSets once.
func Holds(ctx context.Context, plane controlplane.Plane, pool CloudPool) func(report.WaitingPod, Measured) (bool, error) {
	var sizer *sizing
	return func(w report.WaitingPod, measured Measured) (bool, error) {
		if sizer == nil {
			st, err := plane.Read(ctx)
			if err != nil {
				return false, err
			}
			var diskGB float64
			if worker, ok := st.WorkerSnapshot(); ok {
				diskGB = worker.DiskGB
			}
			sizer = newSizing(plane, diskGB)
		}
		for _, loc := range pool.Locations {
			serverType, err := sizer.smallest(ctx, pool, loc, w, measured)
			if err != nil || serverType != "" {
				return serverType != "", err
			}
		}
		return false, nil
	}
}

// sizing judges which server type holds a pod on a new burst member, reading each location's server types and the
// release's DaemonSets once.
type sizing struct {
	plane       controlplane.Plane
	diskGB      float64
	orderable   *controlplane.OrderableTypes
	daemons     []report.WaitingPod
	daemonsRead bool
}

func newSizing(plane controlplane.Plane, diskGB float64) *sizing {
	return &sizing{plane: plane, diskGB: diskGB, orderable: &controlplane.OrderableTypes{API: plane.API}}
}

// smallest is the pool's type with the fewest vCPUs, then the least memory, then listed first, that Hetzner can order
// in loc now with a disk holding the worker image and room for w beside the DaemonSets a burst member there runs, by
// what measured names for it or else its published size less what Talos reserves, and empty when none is or w refuses
// a burst member there.
func (s *sizing) smallest(ctx context.Context, pool CloudPool, loc string, w report.WaitingPod, measured Measured) (string, error) {
	labels := BurstNodeLabels(loc)
	if !w.Admits(labels) {
		return "", nil
	}
	offered, err := s.orderable.Offered(ctx, loc)
	if err != nil {
		return "", err
	}
	taken, err := s.daemonsOn(labels)
	if err != nil {
		return "", err
	}
	var holding []controlplane.ServerType
	for _, name := range pool.ServerTypes {
		i := slices.IndexFunc(offered, func(t controlplane.ServerType) bool { return t.Name == name })
		if i < 0 || offered[i].DiskGB < s.diskGB {
			continue
		}
		room, ok := measured[name]
		if !ok {
			room = offered[i].Offers()
		}
		if room.Minus(taken).Holds(w.Request()) {
			holding = append(holding, offered[i])
		}
	}
	if len(holding) == 0 {
		return "", nil
	}
	return slices.MinFunc(holding, func(a, b controlplane.ServerType) int {
		return cmp.Or(cmp.Compare(a.Cores, b.Cores), cmp.Compare(a.MemoryGB, b.MemoryGB))
	}).Name, nil
}

// daemonsOn is what the release's DaemonSets whose placement admits a node of labels request of it.
func (s *sizing) daemonsOn(labels map[string]string) (report.Request, error) {
	if !s.daemonsRead {
		daemons, err := s.plane.DaemonSetPods()
		if err != nil {
			return report.Request{}, err
		}
		s.daemons, s.daemonsRead = daemons, true
	}
	var taken report.Request
	for _, d := range s.daemons {
		if d.Admits(labels) {
			taken = taken.Plus(d.Request())
		}
	}
	return taken, nil
}
