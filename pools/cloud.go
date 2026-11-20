package pools

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

const (
	// RoleCloudWorker labels each server of the Cloud pool, so a plan finds its members by label alone.
	RoleCloudWorker = controlplane.RoleCloudWorker
	// KeyMember labels a Cloud server with its member key, its location and a sequence number such as fsn1-1.
	KeyMember = "mitstack.dev/member"
	// KeyBurst labels each member above the pool's minimum, which idle removal may choose and the kit's components
	// stay off. The members up to the minimum are steady and carry no such label.
	KeyBurst = components.BurstLabel
)

// IsBurst reports whether srv is a burst member of the Cloud pool.
func IsBurst(srv controlplane.Server) bool { return srv.Labels[KeyBurst] == components.BurstValue }

// CloudPlan is what a Cloud pool lacks, where members leave one at a time through Mark, Drain until the node is gone
// from the cluster, and Delete.
type CloudPlan struct {
	Create []controlplane.ServerSpec
	// Mark holds each member whose removal begins, or who is kept again, with the labels that record it.
	Mark          []controlplane.Server
	Drain, Delete []controlplane.Server
	// Held names why a member whose removal began is not drained yet, which the block records as an event.
	Held []string
}

// removalPlan is the plan a Cloud worker's removal labels name.
const removalPlan = "cloud-pool"

// RemovalUnderWay reports whether the pool's own removal of s, for a lowered maximum or a dropped type or location, began.
func RemovalUnderWay(s controlplane.Server) bool {
	_, began := controlplane.RemovalBegan(s, removalPlan)
	return began
}

// Empty reports whether the pool already stands as wanted.
func (p CloudPlan) Empty() bool {
	return len(p.Create)+len(p.Mark)+len(p.Drain)+len(p.Delete)+len(p.Held) == 0
}

// Apply creates the planned servers, labels each in Mark and deletes each in Delete, leaving Drain to the block.
func (p CloudPlan) Apply(ctx context.Context, api controlplane.API) error {
	if len(p.Create) > 0 {
		if _, err := api.CreateServers(ctx, p.Create...); err != nil {
			return fmt.Errorf("creating the Cloud pool's servers: %w", err)
		}
	}
	for _, srv := range p.Mark {
		if err := api.SetServerLabels(ctx, srv.ID, srv.Labels); err != nil {
			return fmt.Errorf("labelling the removal of the Cloud pool's server %s: %w", srv.Name, err)
		}
	}
	for _, srv := range p.Delete {
		if err := api.DeleteServer(ctx, srv.ID); err != nil {
			return fmt.Errorf("deleting the Cloud pool's server %s: %w", srv.Name, err)
		}
	}
	return nil
}

// PlanCloud compares the servers Hetzner lists for the subscription's Cloud pool with the pool's size, Max. It creates
// a server only while the pool holds fewer than Max, a member leaving included, and none while a worker's replacement
// runs, which creates its successor itself. drained reports whether a member's node has left the cluster.
func PlanCloud(ctx context.Context, plane controlplane.Plane, pool CloudPool, drained func(controlplane.Server) bool) (CloudPlan, error) {
	st, err := plane.Read(ctx)
	if err != nil {
		return CloudPlan{}, err
	}
	if network, ok := st.Network(); ok && recordsReplacement(network) {
		return CloudPlan{}, nil
	}
	members := st.Servers[RoleCloudWorker]
	keep, leaving := splitMembers(members, pool)
	var plan CloudPlan
	for _, srv := range keep {
		if _, began := controlplane.RemovalBegan(srv, removalPlan); began {
			srv.Labels = controlplane.EndRemoval(srv)
			plan.Mark = append(plan.Mark, srv)
		}
	}
	removing := false
	slices.SortStableFunc(leaving, func(a, b controlplane.Server) int { return removalOrder(a) - removalOrder(b) })
	for _, srv := range leaving {
		switch began, ok := controlplane.RemovalBegan(srv, removalPlan); {
		case drained(srv):
			plan.Delete = append(plan.Delete, srv)
		case removing:
		case !ok:
			removing = true
			srv.Labels = controlplane.BeginRemoval(srv, now(plane), removalPlan)
			plan.Mark = append(plan.Mark, srv)
		default:
			removing = true
			held, err := heldDrain(ctx, plane, members, srv, began)
			switch {
			case err != nil:
				return CloudPlan{}, err
			case held != "":
				plan.Held = append(plan.Held, held)
			default:
				plan.Drain = append(plan.Drain, srv)
			}
		}
	}
	room := pool.Max - len(members)
	if room <= 0 {
		return plan, nil
	}
	site, err := newSite(plane, st)
	if err != nil {
		return CloudPlan{}, err
	}
	taken := memberKeys(members)
	steady := len(slices.DeleteFunc(slices.Clone(keep), IsBurst))
	for range room {
		loc, serverType, err := site.place(ctx, pool, keep)
		if err != nil {
			return CloudPlan{}, err
		}
		key := nextKey(loc, taken)
		taken[key] = true
		spec := site.spec(plane, serverType, loc, key)
		if steady < pool.Min {
			steady++
		} else {
			spec.Labels[KeyBurst] = components.BurstValue
		}
		plan.Create = append(plan.Create, spec)
		keep = append(keep, controlplane.Server{Location: loc})
	}
	return plan, nil
}

// Reclassify returns each member whose burst label must change, with the labels it then carries, so the pool holds
// Min steady members, or only steady ones at a set size, and a member whose removal is under way keeps its labels.
func Reclassify(pool CloudPool, members []controlplane.Server) []controlplane.Server {
	var steady, burst []controlplane.Server
	for _, srv := range members {
		switch {
		case RemovalUnderWay(srv):
		case IsBurst(srv):
			burst = append(burst, srv)
		default:
			steady = append(steady, srv)
		}
	}
	want := len(steady) + len(burst)
	if pool.Scales() {
		want = pool.Min
	}
	var changed []controlplane.Server
	for len(steady) > want {
		newest := NewestOfFullest(steady, steady, pool.Locations)
		steady = slices.DeleteFunc(steady, func(srv controlplane.Server) bool { return srv.ID == newest.ID })
		newest.Labels = maps.Clone(newest.Labels)
		newest.Labels[KeyBurst] = components.BurstValue
		changed = append(changed, newest)
	}
	slices.SortFunc(burst, func(a, b controlplane.Server) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	room := max(0, pool.Max-len(members))
	for _, srv := range burst[:min(len(burst), max(0, want-len(steady)-room))] {
		srv.Labels = maps.Clone(srv.Labels)
		delete(srv.Labels, KeyBurst)
		changed = append(changed, srv)
	}
	return changed
}

// splitMembers leaves out each member the pool no longer lists a type or location for, then the newest member of the
// location holding the most while the pool holds more than Max.
func splitMembers(members []controlplane.Server, pool CloudPool) (keep, leaving []controlplane.Server) {
	for _, srv := range members {
		if slices.Contains(pool.Locations, srv.Location) && slices.Contains(pool.ServerTypes, srv.ServerType) {
			keep = append(keep, srv)
		} else {
			leaving = append(leaving, srv)
		}
	}
	for len(keep) > pool.Max {
		newest := NewestOfFullest(keep, keep, pool.Locations)
		keep = slices.DeleteFunc(keep, func(srv controlplane.Server) bool { return srv.ID == newest.ID })
		leaving = append(leaving, newest)
	}
	return keep, leaving
}

func count(servers []controlplane.Server, loc string) int {
	return len(slices.DeleteFunc(slices.Clone(servers), func(srv controlplane.Server) bool { return srv.Location != loc }))
}

// NewestOfFullest is the newest of candidates in the location where members hold the most, the later one listed on a
// tie, so a pool that loses a member keeps the rest spread over its locations.
func NewestOfFullest(members, candidates []controlplane.Server, locations []string) controlplane.Server {
	return slices.MaxFunc(candidates, func(a, b controlplane.Server) int {
		return cmp.Or(count(members, a.Location)-count(members, b.Location),
			slices.Index(locations, a.Location)-slices.Index(locations, b.Location),
			a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
}

func memberKeys(members []controlplane.Server) map[string]bool {
	taken := map[string]bool{}
	for _, srv := range members {
		taken[srv.Labels[KeyMember]] = true
	}
	return taken
}

// creation holds what a new member needs from the cluster, and the server types each location offers.
type creation struct {
	network   controlplane.Network
	firewall  controlplane.Firewall
	image     controlplane.Snapshot
	orderable *controlplane.OrderableTypes
}

func newSite(plane controlplane.Plane, st controlplane.State) (*creation, error) {
	network, hasNetwork := st.Network()
	firewall, hasFirewall := st.Firewall()
	image, hasImage := st.WorkerSnapshot()
	switch {
	case !hasNetwork || !hasFirewall:
		return nil, &controlplane.Refusal{Class: "cluster-network-missing", Message: "the cluster has no single network and firewall to put a Cloud worker on"}
	case !hasImage || image.Status != controlplane.SnapshotAvailable:
		return nil, &controlplane.Refusal{Class: "worker-image-missing", Message: "the cluster has no available worker image to boot a Cloud worker from"}
	}
	return &creation{network: network, firewall: firewall, image: image, orderable: &controlplane.OrderableTypes{API: plane.API}}, nil
}

// place is the location of the pool holding the fewest of members, the earlier one listed on a tie, among those that
// offer one of the pool's types whose disk holds the worker image, and the first such type, which a member created
// there takes.
func (c *creation) place(ctx context.Context, pool CloudPool, members []controlplane.Server) (string, string, error) {
	locations := slices.Clone(pool.Locations)
	slices.SortStableFunc(locations, func(a, b string) int { return count(members, a) - count(members, b) })
	return c.firstHolding(ctx, pool, locations)
}

// serverType is the first type of the pool that loc offers now whose disk holds the worker image.
func (c *creation) serverType(ctx context.Context, pool CloudPool, loc string) (string, error) {
	_, serverType, err := c.firstHolding(ctx, pool, []string{loc})
	return serverType, err
}

// firstHolding is the first of locations that offers one of the pool's types whose disk holds the worker image, since
// Hetzner creates no server from an image larger than its type's disk, and the first such type. It refuses, naming
// each type's disk in each location, when none does.
func (c *creation) firstHolding(ctx context.Context, pool CloudPool, locations []string) (string, string, error) {
	offers := map[string]string{}
	for _, loc := range locations {
		offered, err := c.orderable.Offered(ctx, loc)
		if err != nil {
			return "", "", err
		}
		if serverType := controlplane.FirstFitting(offered, pool.ServerTypes, c.image.DiskGB); serverType != "" {
			return loc, serverType, nil
		}
		offers[loc] = loc + ": " + controlplane.DescribeOffer(offered, pool.ServerTypes)
	}
	var described []string
	for _, loc := range pool.Locations {
		if offer, ok := offers[loc]; ok {
			described = append(described, offer)
		}
	}
	return "", "", &controlplane.Refusal{Class: "no-server-type-available", Message: fmt.Sprintf(
		"Hetzner can order none of the Cloud pool's server types in %s now whose disk holds the worker image's %g GB (%s); retry once it sells one there again",
		strings.Join(locations, " or "), c.image.DiskGB, strings.Join(described, "; "))}
}

// specAt boots the member of key in loc from the worker image, which embeds the node's configuration.
func (c *creation) specAt(ctx context.Context, plane controlplane.Plane, pool CloudPool, loc, key string) (controlplane.ServerSpec, error) {
	serverType, err := c.serverType(ctx, pool, loc)
	if err != nil {
		return controlplane.ServerSpec{}, err
	}
	return c.spec(plane, serverType, loc, key), nil
}

// spec boots the member of key in loc on serverType from the worker image.
func (c *creation) spec(plane controlplane.Plane, serverType, loc, key string) controlplane.ServerSpec {
	labels := plane.Labels(RoleCloudWorker)
	labels[KeyMember] = key
	return controlplane.ServerSpec{
		Name: plane.LocatedName(RoleCloudWorker, key), ServerType: serverType, Location: loc, ImageID: c.image.ID,
		Labels: labels, NetworkIDs: []int64{c.network.ID}, FirewallIDs: []int64{c.firewall.ID},
	}
}

// removalOrder puts a member whose removal began before one whose has not, so a removal under way finishes first.
func removalOrder(srv controlplane.Server) int {
	if _, began := controlplane.RemovalBegan(srv, removalPlan); began {
		return 0
	}
	return 1
}

func now(plane controlplane.Plane) time.Time {
	if plane.Now == nil {
		return time.Now()
	}
	return plane.Now()
}

// heldDrain names why leaving may not drain yet, empty when the plane's redundancy guard finds the other workers and
// OpenBao's quorum serving in a node report since its removal began.
func heldDrain(ctx context.Context, plane controlplane.Plane, members []controlplane.Server, leaving controlplane.Server, began time.Time) (string, error) {
	var read *report.Read
	if plane.ReadReport != nil {
		var err error
		if read, err = plane.ReadReport(ctx, began); err != nil {
			return "", err
		}
	}
	if read == nil {
		return fmt.Sprintf("no node report was received since the removal of the Cloud pool's server %s began at %s, so neither the other workers nor OpenBao's members are read",
			leaving.Name, began.Format(time.RFC3339)), nil
	}
	sets, err := drainSets(*read, members, leaving)
	if err != nil {
		return "", err
	}
	for _, s := range sets {
		if err := plane.Guard.Allow(s.Set, s.Removed, began); redundant.Held(err) {
			return err.Error(), nil
		} else if err != nil {
			return "", err
		}
	}
	return "", nil
}

// drainSets are the sets draining leaving removes a member from: the pool's workers, whose minimum keeps every other
// worker serving unless leaving is the last, and OpenBao's members for each member whose pod runs on leaving.
func drainSets(read report.Read, members []controlplane.Server, leaving controlplane.Server) ([]report.Removal, error) {
	names := make([]string, 0, len(members))
	for _, srv := range members {
		names = append(names, srv.Name)
	}
	workers, err := read.Nodes(names)
	if err != nil {
		return nil, err
	}
	minimum := redundant.AllButOne
	if len(workers) == 1 {
		minimum = redundant.NoRedundancy
	}
	sets := []report.Removal{{Set: redundant.Set{Name: "Cloud pool's workers", Members: workers, Minimum: minimum}, Removed: leaving.Name}}
	openBao, err := read.OpenBaoRemovals(leaving.Name)
	if err != nil {
		return nil, err
	}
	return append(sets, openBao...), nil
}

func nextKey(loc string, taken map[string]bool) string {
	for n := 1; ; n++ {
		if key := loc + "-" + strconv.Itoa(n); !taken[key] {
			return key
		}
	}
}
