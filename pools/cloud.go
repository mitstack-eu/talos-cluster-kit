package pools

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

const (
	// RoleCloudWorker labels each server of the Cloud pool, so a plan finds its members by label alone.
	RoleCloudWorker = controlplane.RoleCloudWorker
	// KeyMember labels a Cloud server with its member key, its location and a sequence number such as fsn1-1.
	KeyMember = "mitstack.dev/member"
)

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
	site, err := newSite(st)
	if err != nil {
		return CloudPlan{}, err
	}
	taken := memberKeys(members)
	for range room {
		loc := fewest(keep, pool.Locations)
		key := nextKey(loc, taken)
		taken[key] = true
		spec, err := site.specAt(ctx, plane, pool, loc, key)
		if err != nil {
			return CloudPlan{}, err
		}
		plan.Create = append(plan.Create, spec)
		keep = append(keep, controlplane.Server{Location: loc})
	}
	return plan, nil
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
		loc := most(keep, pool.Locations)
		var inLoc []controlplane.Server
		for _, srv := range keep {
			if srv.Location == loc {
				inLoc = append(inLoc, srv)
			}
		}
		newest := slices.MaxFunc(inLoc, func(a, b controlplane.Server) int {
			if c := a.Created.Compare(b.Created); c != 0 {
				return c
			}
			return int(a.ID - b.ID)
		})
		keep = slices.DeleteFunc(keep, func(srv controlplane.Server) bool { return srv.ID == newest.ID })
		leaving = append(leaving, newest)
	}
	return keep, leaving
}

func count(servers []controlplane.Server, loc string) int {
	return len(slices.DeleteFunc(slices.Clone(servers), func(srv controlplane.Server) bool { return srv.Location != loc }))
}

// most is the location holding the most servers, the later one listed on a tie.
func most(servers []controlplane.Server, locations []string) string {
	best := locations[0]
	for _, loc := range locations {
		if count(servers, loc) >= count(servers, best) {
			best = loc
		}
	}
	return best
}

// fewest is the location holding the fewest servers, the earlier one listed on a tie.
func fewest(servers []controlplane.Server, locations []string) string {
	best := locations[0]
	for _, loc := range locations {
		if count(servers, loc) < count(servers, best) {
			best = loc
		}
	}
	return best
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
	network  controlplane.Network
	firewall controlplane.Firewall
	image    controlplane.Snapshot
	offered  map[string][]string
}

func newSite(st controlplane.State) (*creation, error) {
	network, hasNetwork := st.Network()
	firewall, hasFirewall := st.Firewall()
	image, hasImage := st.WorkerSnapshot()
	switch {
	case !hasNetwork || !hasFirewall:
		return nil, &controlplane.Refusal{Class: "cluster-network-missing", Message: "the cluster has no single network and firewall to put a Cloud worker on"}
	case !hasImage || image.Status != controlplane.SnapshotAvailable:
		return nil, &controlplane.Refusal{Class: "worker-image-missing", Message: "the cluster has no available worker image to boot a Cloud worker from"}
	}
	return &creation{network: network, firewall: firewall, image: image, offered: map[string][]string{}}, nil
}

// serverType is the first type of the pool that loc offers now, which a member created there takes.
func (c *creation) serverType(ctx context.Context, plane controlplane.Plane, pool CloudPool, loc string) (string, error) {
	if _, ok := c.offered[loc]; !ok {
		types, err := plane.API.AvailableServerTypes(ctx, loc)
		if err != nil {
			return "", fmt.Errorf("listing the server types %s offers: %w", loc, err)
		}
		c.offered[loc] = types
	}
	i := slices.IndexFunc(pool.ServerTypes, func(t string) bool { return slices.Contains(c.offered[loc], t) })
	if i < 0 {
		return "", &controlplane.Refusal{Class: "no-server-type-available", Message: fmt.Sprintf("%s offers none of the Cloud pool's server types %v", loc, pool.ServerTypes)}
	}
	return pool.ServerTypes[i], nil
}

// specAt boots the member of key in loc from the worker image, which embeds the node's configuration.
func (c *creation) specAt(ctx context.Context, plane controlplane.Plane, pool CloudPool, loc, key string) (controlplane.ServerSpec, error) {
	serverType, err := c.serverType(ctx, plane, pool, loc)
	if err != nil {
		return controlplane.ServerSpec{}, err
	}
	labels := plane.Labels(RoleCloudWorker)
	labels[KeyMember] = key
	return controlplane.ServerSpec{
		Name: plane.LocatedName(RoleCloudWorker, key), ServerType: serverType, Location: loc, ImageID: c.image.ID,
		Labels: labels, NetworkIDs: []int64{c.network.ID}, FirewallIDs: []int64{c.firewall.ID},
	}, nil
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
