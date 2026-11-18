package robotcluster

// A change of the pool's inputs only grows the Cloud pool, so one that would remove a worker for good is refused before
// anything changes. A worker is drained and deleted when an upgrade names it to be replaced, or when a pool whose
// minimum is below its maximum finds it idle past its window.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/scaling"
)

const (
	ResourceCloudWorker = "cloud-worker"
	memberSeparator     = "/"
)

func readCloudPool(d contract.Desired) (pools.CloudPool, error) {
	parsed, err := pools.Parse(d.Params)
	if err != nil {
		return pools.CloudPool{}, handler.Refuse("invalid-param", "the Cloud pool's inputs break a rule:\n%v", err)
	}
	return parsed.Cloud, nil
}

func MemberName(key string) string { return ResourceCloudWorker + memberSeparator + key }

func memberKey(name string) string {
	key, found := strings.CutPrefix(name, ResourceCloudWorker+memberSeparator)
	if !found {
		return ""
	}
	return key
}

// neverDrained reads every member as still in the cluster, so no plan deletes one.
func neverDrained(controlplane.Server) bool { return false }

// workerImageState tells an install's worker builder or snapshot still in flight, a wait, from an image the cluster lacks.
type workerImageState int

const (
	workerImageReady workerImageState = iota
	workerImageBuilding
	workerImageMissing
)

func workerImageOf(st controlplane.State) workerImageState {
	image, hasImage := st.WorkerSnapshot()
	switch {
	case hasImage && image.Status == controlplane.SnapshotAvailable:
		return workerImageReady
	case hasImage, len(st.Servers[controlplane.RoleWorkerBuilder]) > 0:
		return workerImageBuilding
	}
	return workerImageMissing
}

// clusterBuilt is whether the install has finished: every location has its node and the Robot worker is written.
func (c convergence) clusterBuilt() bool {
	network, hasNetwork := c.state.Network()
	return hasNetwork && workerProgress(network) == WorkerWritten && len(c.state.Servers[controlplane.RoleNode]) == len(controlplane.Locations)
}

// poolRefusal refuses, before any change, a pool that would remove a worker or that grows with no worker image.
func (c convergence) poolRefusal(ctx context.Context) error {
	pool, members := c.req.params.cloud, len(c.state.Servers[pools.RoleCloudWorker])
	// An install still under way has not built the worker image yet, so only a built cluster is judged for it.
	if c.clusterBuilt() && pool.Max > members && workerImageOf(c.state) == workerImageMissing {
		return handler.Refuse("worker-image-missing",
			"the cluster holds no worker image, which a Cloud worker boots from, so nothing was changed; the image is built only when a cluster is installed")
	}
	if members == 0 {
		return nil
	}
	// A pool no larger than its members plans no creation, so this plan needs no worker image, firewall or orderable type.
	held := pool
	held.Max = min(pool.Max, members)
	plan, err := pools.PlanCloud(ctx, c.plane, held, neverDrained)
	if err != nil {
		return refusal(err)
	}
	if leaving := leavingNames(plan); len(leaving) > 0 {
		return handler.Refuse("cloud-pool-drain-missing",
			"the Cloud pool of %d would remove a worker, %s first, and the pool's inputs never shrink it: a worker is drained and deleted only when an upgrade names it in replace, or when a pool whose minimum is below its maximum finds it idle, so nothing was changed; keep the pool at %d or more workers of the listed types and locations",
			pool.Max, strings.Join(leaving, ", "), members)
	}
	return nil
}

func leavingNames(plan pools.CloudPlan) []string {
	var names []string
	for _, group := range [][]controlplane.Server{plan.Mark, plan.Drain, plan.Delete} {
		for _, srv := range group {
			names = append(names, srv.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// poolStep removes the worker the scaling chose, else creates the workers the pool lacks, once the install has built the
// cluster and its worker image and while no worker's replacement or removal is under way.
func (c convergence) poolStep(ctx context.Context) (controlplane.Step, bool, error) {
	pool := c.req.params.cloud
	if !c.clusterBuilt() || pool.Max == 0 || workerImageOf(c.state) != workerImageReady || pools.ChangeUnderWay(c.state) {
		return controlplane.Step{}, false, nil
	}
	decision, err := c.scaling(ctx)
	if err != nil {
		return controlplane.Step{}, false, err
	}
	if decision.Remove != nil {
		s, err := pools.RemoveWorker(c.plane, c.state, decision.Remove.Labels[pools.KeyMember], decision.Detail)
		return s, err == nil, refusal(err)
	}
	sized := pool
	sized.Max = decision.Want
	plan, err := pools.PlanCloud(ctx, c.plane, sized, neverDrained)
	if err != nil || len(plan.Create) == 0 {
		return controlplane.Step{}, false, refusal(err)
	}
	// Only the creations are applied, so a worker that appeared since the refusal read the pool is never marked.
	grow := pools.CloudPlan{Create: plan.Create}
	what := describeCreations(grow.Create)
	if decision.Detail != "" {
		what += ", since " + decision.Detail
	}
	return controlplane.Step{What: what, Run: func(ctx context.Context) error {
		if decision.Detail != "" {
			c.req.k.narrate(ctx, c.req.d, decision.Detail)
		}
		if err := grow.Apply(ctx, c.plane.API); err != nil {
			c.req.k.narrate(ctx, c.req.d, fmt.Sprintf("the Cloud pool stopped growing, since Hetzner refused a worker: %v", err))
			return err
		}
		for _, spec := range grow.Create {
			c.req.k.narrate(ctx, c.req.d, fmt.Sprintf("created Cloud worker %s as %s in %s", spec.Name, spec.ServerType, spec.Location))
		}
		return nil
	}}, true, nil
}

// scaling is what the pool should hold by the node reports, its size for a pool of a set size.
func (c convergence) scaling(ctx context.Context) (scaling.Decision, error) {
	pool := c.req.params.cloud
	if !pool.Scales() {
		return scaling.Decision{Want: pool.Max}, nil
	}
	reads, err := c.req.reports.readAll(ctx)
	if err != nil {
		return scaling.Decision{}, err
	}
	return scaling.Decide(pool, c.state.Servers[pools.RoleCloudWorker], reads)
}

// heldAtMaximum names a pool that holds its maximum while pods wait for a node, the cluster resource's health cause.
func heldAtMaximum(ctx context.Context, d contract.Desired, st controlplane.State, reports *GatewayReports) (cause, detail string, err error) {
	pool, err := readCloudPool(d)
	if err != nil || !pool.Scales() {
		return "", "", nil
	}
	reads, err := reports.readAll(ctx)
	if err != nil {
		return "", "", err
	}
	decision, err := scaling.Decide(pool, st.Servers[pools.RoleCloudWorker], reads)
	if err != nil || !decision.AtMaximum {
		return "", "", err
	}
	return scaling.CauseAtMaximum, decision.Detail, nil
}

func describeCreations(specs []controlplane.ServerSpec) string {
	parts := make([]string, 0, len(specs))
	for _, spec := range specs {
		parts = append(parts, fmt.Sprintf("%s as %s in %s", spec.Name, spec.ServerType, spec.Location))
	}
	return "create the Cloud pool's workers " + strings.Join(parts, ", ")
}

func cloudWorkerResources(st controlplane.State) []contract.CreatedResource {
	var out []contract.CreatedResource
	for _, w := range st.Servers[pools.RoleCloudWorker] {
		key := w.Labels[pools.KeyMember]
		if key == "" {
			continue
		}
		out = append(out, contract.CreatedResource{Name: MemberName(key), Type: TypeServer, Identity: map[string]string{"serverId": strconv.FormatInt(w.ID, 10)}})
	}
	return out
}

// cloudWorkerObserver reads one member of the cloud-worker entry by its member key label, never by a recorded identity.
type cloudWorkerObserver struct{ k *Cluster }

func (o cloudWorkerObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	key := memberKey(r.Name)
	if key == "" {
		return operator.Unknown(fmt.Sprintf("%q names no member of the %s entry", r.Name, ResourceCloudWorker)), nil
	}
	p, refused, ok := o.k.hetznerReader(r.Desired)
	if !ok {
		return refused, nil
	}
	st, err := p.Read(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	members := slices.DeleteFunc(slices.Clone(st.Servers[pools.RoleCloudWorker]), func(s controlplane.Server) bool { return s.Labels[pools.KeyMember] != key })
	return judgeLabelled("Cloud worker "+key, controlplane.IDs(members, controlplane.ServerID)), nil
}

func unreportedWorker(newest []operator.ReportedNode, workers []controlplane.Server) string {
	for _, w := range workers {
		if !slices.ContainsFunc(newest, func(n operator.ReportedNode) bool { return n.Name == w.Name }) {
			return fmt.Sprintf("the newest node report does not name the Cloud worker %s", w.Name)
		}
	}
	return ""
}
