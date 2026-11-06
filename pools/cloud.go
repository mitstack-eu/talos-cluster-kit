package pools

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

const (
	// RoleCloudWorker labels each server of the Cloud pool, so a plan finds its members by label alone.
	RoleCloudWorker controlplane.Role = "cloud-worker"
	// KeyMember labels a Cloud server with its member key, its location and a sequence number such as fsn1-1.
	KeyMember = "mitstack.dev/member"
)

// CloudPlan is what a Cloud pool lacks. A member leaves in two passes: it appears in Drain until its node is gone from
// the cluster, then in Delete.
type CloudPlan struct {
	Create        []controlplane.ServerSpec
	Drain, Delete []controlplane.Server
}

// Empty reports whether the pool already stands as wanted.
func (p CloudPlan) Empty() bool { return len(p.Create)+len(p.Drain)+len(p.Delete) == 0 }

// Apply creates the planned servers and deletes the ones Delete names. The block drains each node in Drain itself.
func (p CloudPlan) Apply(ctx context.Context, api controlplane.API) error {
	if len(p.Create) > 0 {
		if _, err := api.CreateServers(ctx, p.Create...); err != nil {
			return fmt.Errorf("creating the Cloud pool's servers: %w", err)
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
// a server only while the pool holds fewer than Max, a member leaving included, so the pool never passes its size.
// drained reports whether a member's node has left the cluster.
func PlanCloud(ctx context.Context, plane controlplane.Plane, pool CloudPool, drained func(controlplane.Server) bool) (CloudPlan, error) {
	st, err := plane.Read(ctx)
	if err != nil {
		return CloudPlan{}, err
	}
	members := st.Servers[RoleCloudWorker]
	keep, leaving := splitMembers(members, pool)
	var plan CloudPlan
	for _, srv := range leaving {
		if drained(srv) {
			plan.Delete = append(plan.Delete, srv)
		} else {
			plan.Drain = append(plan.Drain, srv)
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
		spec, err := site.spec(ctx, plane, pool, loc, taken)
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

// spec boots a member from the worker image, which embeds the node's configuration, in loc, of the first type listed
// that loc offers, under the first member key of loc not taken.
func (c *creation) spec(ctx context.Context, plane controlplane.Plane, pool CloudPool, loc string, taken map[string]bool) (controlplane.ServerSpec, error) {
	if _, ok := c.offered[loc]; !ok {
		types, err := plane.API.AvailableServerTypes(ctx, loc)
		if err != nil {
			return controlplane.ServerSpec{}, fmt.Errorf("listing the server types %s offers: %w", loc, err)
		}
		c.offered[loc] = types
	}
	i := slices.IndexFunc(pool.ServerTypes, func(t string) bool { return slices.Contains(c.offered[loc], t) })
	if i < 0 {
		return controlplane.ServerSpec{}, &controlplane.Refusal{Class: "no-server-type-available", Message: fmt.Sprintf("%s offers none of the Cloud pool's server types %v", loc, pool.ServerTypes)}
	}
	key := nextKey(loc, taken)
	taken[key] = true
	labels := plane.Labels(RoleCloudWorker)
	labels[KeyMember] = key
	return controlplane.ServerSpec{
		Name: plane.LocatedName(RoleCloudWorker, key), ServerType: pool.ServerTypes[i], Location: loc, ImageID: c.image.ID,
		Labels: labels, NetworkIDs: []int64{c.network.ID}, FirewallIDs: []int64{c.firewall.ID},
	}, nil
}

func nextKey(loc string, taken map[string]bool) string {
	for n := 1; ; n++ {
		if key := loc + "-" + strconv.Itoa(n); !taken[key] {
			return key
		}
	}
}
