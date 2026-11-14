package pools

// A confirmed upgrade replaces a named Cloud worker one stage a call, each stage recorded on the network, on the server
// or in the cluster repository, so a call that finds the worker draining, off or gone carries the replacement on there.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

const (
	// KeyWorkerReplace on the network, followed by a member key, records the Cloud worker an upgrade asked to replace,
	// so each later call carries the replacement on and reads another server of that key as its successor.
	KeyWorkerReplace = "mitstack.dev/worker-replace-"
	// workerReplacePlan, followed by that record, is the plan a replaced worker's removal labels name.
	workerReplacePlan = "worker-replace-"
)

type workerReplacement struct {
	key   string
	old   int64
	asked time.Time
}

// record is the label value that holds r, which workerReplacePlan prefixes to give the plan its worker's removal labels name.
func (r workerReplacement) record() string {
	return strconv.FormatInt(r.old, 10) + "-" + strconv.FormatInt(r.asked.Unix(), 10)
}

func (r workerReplacement) plan() string { return workerReplacePlan + r.record() }

// location is where the worker of r's key runs, the part of the key before its sequence number.
func (r workerReplacement) location() string {
	loc, _, _ := strings.Cut(r.key, "-")
	return loc
}

// named says which replacement err stopped, and leaves a refusal as it is, since its message already says.
func (r workerReplacement) named(err error) error {
	var refusal *controlplane.Refusal
	if err == nil || errors.As(err, &refusal) {
		return err
	}
	return fmt.Errorf("replacing the Cloud worker %s: %w", r.key, err)
}

func recordsReplacement(network controlplane.Network) bool {
	return slices.ContainsFunc(slices.Collect(maps.Keys(network.Labels)), func(label string) bool {
		return strings.HasPrefix(label, KeyWorkerReplace)
	})
}

func workerReplacements(network controlplane.Network) ([]workerReplacement, error) {
	var asked []workerReplacement
	for _, label := range slices.Sorted(maps.Keys(network.Labels)) {
		key, ok := strings.CutPrefix(label, KeyWorkerReplace)
		if !ok {
			continue
		}
		id, unix, _ := strings.Cut(network.Labels[label], "-")
		old, idErr := strconv.ParseInt(id, 10, 64)
		at, atErr := strconv.ParseInt(unix, 10, 64)
		if idErr != nil || atErr != nil {
			return nil, fmt.Errorf("network %d carries the label %s=%q, which is no replacement the pool recorded", network.ID, label, network.Labels[label])
		}
		asked = append(asked, workerReplacement{key: key, old: old, asked: time.Unix(at, 0).UTC()})
	}
	return asked, nil
}

func refuse(class, format string, args ...any) error {
	return &controlplane.Refusal{Class: class, Message: fmt.Sprintf(format, args...)}
}

// lacks names what a worker replacement needs on every call and the block does not pass, with the class that refuses it.
func lacks(plane controlplane.Plane) (class, what string) {
	switch {
	case plane.Removals == nil || plane.Removals.Held == nil || plane.Removals.Commit == nil:
		return "worker-removal-unwritable", "no cluster repository to write its drain and its Node object's removal to"
	case plane.ReadReport == nil || plane.Guard == nil:
		return "worker-replacement-unjudged", "no node report read or no redundancy guard to judge the other workers by"
	}
	return "", ""
}

// ReplaceWorkers records each Cloud worker of keys that the network does not record yet, and refuses before the
// upgrade writes anything, so an upgrade never leaves a part of what it named.
func ReplaceWorkers(pool CloudPool, keys []string) controlplane.Record {
	return func(ctx context.Context, plane controlplane.Plane, st controlplane.State, labels map[string]string) (string, error) {
		if len(keys) == 0 {
			return "", nil
		}
		if class, what := lacks(plane); class != "" {
			return "", refuse(class, "the upgrade names a Cloud worker to replace, and the block passes %s, so nothing was changed", what)
		}
		site, err := newSite(st)
		if err != nil {
			return "", err
		}
		members := st.Servers[RoleCloudWorker]
		var named []string
		for _, key := range keys {
			worker, ok := memberOf(members, key)
			if !ok {
				return "", refuse("worker-replacement-unknown", "the upgrade names the Cloud worker %q to replace, and the pool holds no member of that key, so nothing was changed; it holds %s",
					key, strings.Join(slices.Sorted(maps.Keys(memberKeys(members))), ", "))
			}
			if _, recorded := labels[KeyWorkerReplace+key]; recorded {
				continue
			}
			if _, err := site.serverType(ctx, plane, pool, worker.Location); err != nil {
				return "", err
			}
			labels[KeyWorkerReplace+key] = workerReplacement{key: key, old: worker.ID, asked: now(plane)}.record()
			named = append(named, key)
		}
		if len(named) == 0 {
			return "", nil
		}
		return "the Cloud worker " + strings.Join(named, ", "), nil
	}
}

func memberOf(members []controlplane.Server, key string) (controlplane.Server, bool) {
	i := slices.IndexFunc(members, func(srv controlplane.Server) bool { return srv.Labels[KeyMember] == key })
	if i < 0 {
		return controlplane.Server{}, false
	}
	return members[i], true
}

// ReplaceStep is the next step of the first replacement the network records, false while it waits, so no second
// worker goes before a node report reads the first one's successor Ready.
func ReplaceStep(ctx context.Context, plane controlplane.Plane, pool CloudPool) (controlplane.Step, bool, error) {
	x, ok, err := replacing(ctx, plane, pool)
	if err != nil || !ok {
		return controlplane.Step{}, false, err
	}
	g, err := x.next(ctx)
	return g.step, g.step.Run != nil, x.r.named(err)
}

// ReplacementProblems names where each replacement the network records stands, so a pool with one under way never
// reads as complete and each wait says what it is for.
func ReplacementProblems(ctx context.Context, plane controlplane.Plane, pool CloudPool) []string {
	x, ok, err := replacing(ctx, plane, pool)
	if err != nil {
		return []string{err.Error()}
	} else if !ok {
		return nil
	}
	asked := "the replacement the upgrade asked for at " + stamp(x.r.asked)
	var out []string
	switch g, err := x.next(ctx); {
	case err != nil:
		out = append(out, fmt.Sprintf("the Cloud worker %s cannot go on with %s, since %v", x.r.key, asked, err))
	case g.step.Run != nil:
		out = append(out, fmt.Sprintf("the Cloud worker %s is under %s, whose next step is to %s", x.r.key, asked, g.step.What))
	default:
		out = append(out, fmt.Sprintf("the Cloud worker %s waits for %s: %s", x.r.key, asked, g.waits))
	}
	for _, r := range x.later {
		out = append(out, fmt.Sprintf("the Cloud worker %s waits for the replacement the upgrade asked for at %s until the replacement before it has ended",
			r.key, stamp(r.asked)))
	}
	return out
}

// replacer carries one recorded replacement on from what one call reads.
type replacer struct {
	plane   controlplane.Plane
	st      controlplane.State
	network controlplane.Network
	pool    CloudPool
	r       workerReplacement
	later   []workerReplacement
}

func replacing(ctx context.Context, plane controlplane.Plane, pool CloudPool) (replacer, bool, error) {
	st, err := plane.Read(ctx)
	if err != nil {
		return replacer{}, false, err
	}
	network, ok := st.Network()
	if !ok {
		return replacer{}, false, nil
	}
	asked, err := workerReplacements(network)
	if err != nil || len(asked) == 0 {
		return replacer{}, false, err
	}
	if class, what := lacks(plane); class != "" {
		return replacer{}, false, refuse(class, "the replacement of the Cloud worker %s is under way, and the block passes %s", asked[0].key, what)
	}
	return replacer{plane: plane, st: st, network: network, pool: pool, r: asked[0], later: asked[1:]}, true, nil
}

type progress struct {
	step  controlplane.Step
	waits string
}

func next(what string, run func(ctx context.Context) error) (progress, error) {
	return progress{step: controlplane.Step{What: what, Run: run}}, nil
}

func waitsOn(format string, args ...any) (progress, error) {
	return progress{waits: fmt.Sprintf(format, args...)}, nil
}

func (x replacer) next(ctx context.Context) (progress, error) {
	all, err := x.plane.Removals.Held(ctx)
	if err != nil {
		return progress{}, fmt.Errorf("listing the removals the cluster repository holds: %w", err)
	}
	held := slices.DeleteFunc(all, func(path string) bool {
		return path != jobPath(drainStep, x.r.key) && path != jobPath(removeNodeStep, x.r.key)
	})
	worker, ok := memberOf(x.st.Servers[RoleCloudWorker], x.r.key)
	switch {
	case !ok:
		return x.createSuccessor(ctx, held)
	case worker.ID != x.r.old:
		return x.endStep(ctx, worker)
	case worker.Status == controlplane.ServerRunning && slices.Contains(held, jobPath(drainStep, x.r.key)):
		return x.draining(ctx, worker)
	case worker.Status == controlplane.ServerRunning:
		return x.running(ctx, worker)
	case worker.Status == controlplane.ServerOff:
		return x.off(ctx, worker, held)
	}
	return waitsOn("Hetzner reads Cloud worker %d %s, neither running nor off", worker.ID, worker.Status)
}

// running judges a running worker's removal by a node report since its replacement began, and refuses it before its
// drain is written, which withdraws the replacement.
func (x replacer) running(ctx context.Context, worker controlplane.Server) (progress, error) {
	site, err := newSite(x.st)
	if err == nil {
		_, err = site.serverType(ctx, x.plane, x.pool, x.r.location())
	}
	if err != nil {
		return x.refused(err)
	}
	began, begun := controlplane.RemovalBegan(worker, x.r.plan())
	if !begun {
		return x.begin(worker)
	}
	read, err := x.plane.ReadReport(ctx, began)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since its replacement began at %s", stamp(began))
	}
	jobs, readable, err := read.AppliedJobs(controlplane.RemovalApplication)
	switch {
	case err != nil:
		return progress{}, err
	case !readable:
		return x.refused(refuse("worker-replacement-unjudged", "%s, so no report could show a drain that failed", read.UnreadJobs(controlplane.RemovalApplication)))
	}
	if _, applied := jobs[jobName(drainStep, x.r.key)]; applied {
		return waitsOn("Argo CD still holds an earlier drain Job %s in the node report read at %s, and prunes it now that the cluster repository no longer holds its file",
			jobName(drainStep, x.r.key), stamp(read.At))
	}
	ready, err := x.allowRemoval(*read, worker, began)
	switch {
	case err != nil:
		return x.refused(err)
	case !ready:
		return x.powerOff(worker, "with no drain, since the node report read at "+stamp(read.At)+" reads its Node object not Ready")
	}
	return x.write(drainStep, worker.Name, "whose Job drains it under its pods' disruption budgets")
}

// allowRemoval judges the worker's removal by block-kit's guard over every worker the report lists, whose minimum
// keeps each other one serving, and over the OpenBao members on it, and reports whether its Node object reads Ready.
func (x replacer) allowRemoval(read report.Read, worker controlplane.Server, began time.Time) (bool, error) {
	listed, err := read.ListedNodes()
	if err != nil {
		return false, err
	}
	workers := slices.DeleteFunc(listed, func(m report.Member) bool {
		return slices.ContainsFunc(x.st.Servers[controlplane.RoleNode], func(node controlplane.Server) bool { return node.Name == m.Name })
	})
	i := slices.IndexFunc(workers, func(m report.Member) bool { return m.Name == worker.Name })
	if i < 0 {
		workers = append(workers, report.Member{Name: worker.Name})
	}
	sets := []report.Removal{{Set: redundant.Set{Name: "workers", Members: workers, Minimum: redundant.AllButOne}, Removed: worker.Name}}
	openBao, err := read.OpenBaoRemovals(worker.Name)
	if err != nil {
		return false, err
	}
	for _, s := range append(sets, openBao...) {
		if err := x.plane.Guard.Allow(s.Set, s.Removed, began); err != nil {
			return false, controlplane.BelowMinimum(err)
		}
	}
	return i >= 0 && workers[i].Reading.Serving, nil
}

// begin holds a replacement while the upgrade replaces the gateways or a control-plane node, so no worker goes while
// the control plane is short of a member.
func (x replacer) begin(worker controlplane.Server) (progress, error) {
	for label := range x.network.Labels {
		if label == controlplane.KeyGatewayUpgrade || strings.HasPrefix(label, controlplane.KeyNodeReplace) {
			return waitsOn("it begins once the upgrade has replaced the gateways and the control-plane nodes it names")
		}
	}
	return next("begin replacing Cloud worker "+x.r.key+", so only a node report read after now judges the other workers", func(ctx context.Context) error {
		if err := x.plane.API.SetServerLabels(ctx, worker.ID, controlplane.BeginRemoval(worker, now(x.plane), x.r.plan())); err != nil {
			return err
		}
		x.notify(ctx, "began replacing Cloud worker %d (%s), which the upgrade named", worker.ID, x.r.key)
		return nil
	})
}

// refused withdraws the replacement when err refuses it, so a replacement refused before its drain leaves no record a
// later call would carry on; any other error it answers as it is.
func (x replacer) refused(err error) (progress, error) {
	var refusal *controlplane.Refusal
	if !errors.As(err, &refusal) {
		return progress{}, err
	}
	return next("withdraw the replacement of Cloud worker "+x.r.key+", which still runs, since it is refused: "+refusal.Message, func(ctx context.Context) error {
		if werr := x.unrecord(ctx); werr != nil {
			return errors.Join(err, werr)
		}
		x.notify(ctx, "withdrew the replacement of Cloud worker %s, which still runs: %s", x.r.key, refusal.Message)
		return err
	})
}

func (x replacer) unrecord(ctx context.Context) error {
	labels := maps.Clone(x.network.Labels)
	delete(labels, KeyWorkerReplace+x.r.key)
	if err := x.plane.API.SetNetworkLabels(ctx, x.network.ID, labels); err != nil {
		return fmt.Errorf("taking the replacement of Cloud worker %s off the network: %w", x.r.key, err)
	}
	return nil
}

// draining waits on the drain Job's health in a node report, naming what its newest failed attempt ended with, and
// withdraws the replacement once the Job has failed its last attempt, whose script gave the worker back to the scheduler.
func (x replacer) draining(ctx context.Context, worker controlplane.Server) (progress, error) {
	began, _ := controlplane.RemovalBegan(worker, x.r.plan())
	read, err := x.plane.ReadReport(ctx, began)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since its replacement began at %s; its server is kept", stamp(began))
	}
	job, at := jobName(drainStep, x.r.key), stamp(read.At)
	jobs, readable, err := read.AppliedJobs(controlplane.RemovalApplication)
	if err != nil {
		return progress{}, err
	}
	failure, ferr := read.JobFailure(nodeconfig.TalosAPINamespace, job)
	_, applied := jobs[job]
	switch {
	case !readable:
		return waitsOn("%s, so its drain Job %s cannot be read; its server is kept", read.UnreadJobs(controlplane.RemovalApplication), job)
	case !applied:
		return waitsOn("the node report read at %s lists no Job %s, so the Application %s has not applied its file; its server is kept",
			at, job, controlplane.RemovalApplication)
	case jobs[job].Health == report.JobCompleted:
		return x.powerOff(worker, "since the node report read at "+at+" reads its drain complete")
	case ferr != nil:
		return progress{}, ferr
	case jobs[job].Health == report.JobFailed:
		return x.drainFailed(job, at, cmp.Or(failure, jobs[job].Message, "the node report gives no message of it"))
	case failure != "":
		return waitsOn("its drain waits, since the newest attempt of its Job %s ended in the node report read at %s: %s; its server is kept", job, at, failure)
	}
	return waitsOn("its drain Job %s has not finished in the node report read at %s; its server is kept", job, at)
}

func (x replacer) drainFailed(job, at, why string) (progress, error) {
	path := jobPath(drainStep, x.r.key)
	refusal := &controlplane.Refusal{Class: "worker-drain-failed", Message: fmt.Sprintf(
		"the drain Job %s of Cloud worker %s failed its last attempt in the node report read at %s, so the worker serves on and its replacement is withdrawn: %s",
		job, x.r.key, at, why)}
	return next("take "+path+" out and withdraw the replacement of Cloud worker "+x.r.key+", whose drain failed", func(ctx context.Context) error {
		if err := x.plane.Removals.Commit(ctx, path, nil); err != nil {
			return errors.Join(refusal, fmt.Errorf("taking %s out of the cluster repository: %w", path, err))
		}
		if err := x.unrecord(ctx); err != nil {
			return errors.Join(refusal, err)
		}
		x.notify(ctx, "took %s out of the cluster repository and withdrew the replacement: %s", path, refusal.Message)
		return refusal
	})
}

func (x replacer) powerOff(worker controlplane.Server, why string) (progress, error) {
	return next("power off Cloud worker "+x.r.key+" "+why+", so its Node object goes before its server", func(ctx context.Context) error {
		if err := x.plane.API.PowerOffServer(ctx, worker.ID); err != nil {
			return fmt.Errorf("powering off Cloud worker %d: %w", worker.ID, err)
		}
		x.notify(ctx, "powered off Cloud worker %d (%s) %s", worker.ID, x.r.key, why)
		return nil
	})
}

// off removes a powered-off worker's Node object through the cluster repository, and deletes its server only once a
// node report reads that removal complete and lists no Node object of it.
func (x replacer) off(ctx context.Context, worker controlplane.Server, held []string) (progress, error) {
	if drain := jobPath(drainStep, x.r.key); slices.Contains(held, drain) {
		return x.takeOut(drain, "since Cloud worker "+x.r.key+" is off")
	}
	began, begun := controlplane.RemovalBegan(worker, x.r.plan())
	if !begun {
		return x.begin(worker)
	}
	read, err := x.plane.ReadReport(ctx, began)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since its replacement began at %s", stamp(began))
	}
	if g, removed, err := x.removal(*read, worker.Name, held); err != nil || !removed {
		return g, err
	}
	listed, err := read.ListedNodes()
	if err != nil {
		return progress{}, err
	}
	if len(listed) == 0 || slices.ContainsFunc(listed, func(m report.Member) bool { return m.Name == worker.Name }) {
		return waitsOn("its Node object's removal completed, and the node report read at %s still lists the Node object", stamp(read.At))
	}
	return x.deleteServer(worker, *read)
}

// removal carries on the Job that removes the Node object of hostname, and reports whether read reads it complete
// while main holds its file, which only this replacement writes.
func (x replacer) removal(read report.Read, hostname string, held []string) (progress, bool, error) {
	path, job, at := jobPath(removeNodeStep, x.r.key), jobName(removeNodeStep, x.r.key), stamp(read.At)
	jobs, readable, err := read.AppliedJobs(controlplane.RemovalApplication)
	if err != nil {
		return progress{}, false, err
	}
	status, applied := jobs[job]
	holds := slices.Contains(held, path)
	pending := func(g progress, err error) (progress, bool, error) { return g, false, err }
	switch {
	case !readable:
		return pending(waitsOn("%s, so the Job %s that removes its Node object cannot be read", read.UnreadJobs(controlplane.RemovalApplication), job))
	case holds && status.Health == report.JobCompleted:
		return progress{}, true, nil
	case holds && status.Health == report.JobFailed:
		failure, err := read.JobFailure(nodeconfig.TalosAPINamespace, job)
		if err != nil {
			return progress{}, false, err
		}
		return pending(x.takeOut(path, "whose Job failed in the node report read at "+at+", so it runs again: "+
			cmp.Or(failure, status.Message, "the node report gives no message of it")))
	case holds && !applied:
		return pending(waitsOn("the node report read at %s lists no Job %s, so the Application %s has not applied its file", at, job, controlplane.RemovalApplication))
	case holds:
		return pending(waitsOn("the Job %s that removes its Node object has not finished in the node report read at %s", job, at))
	case applied:
		return pending(waitsOn("Argo CD still holds the Job %s in the node report read at %s, and prunes it now that the cluster repository no longer holds its file", job, at))
	}
	return pending(x.write(removeNodeStep, hostname, "whose Job releases each claim on a local volume of it and removes its Node object"))
}

func (x replacer) write(step, hostname, why string) (progress, error) {
	job, err := renderWorkerJob(step, x.r.key, hostname)
	if err != nil {
		return progress{}, err
	}
	path := jobPath(step, x.r.key)
	return next("write "+path+" to the cluster repository, "+why, func(ctx context.Context) error {
		if err := x.plane.Removals.Commit(ctx, path, job); err != nil {
			return fmt.Errorf("writing %s to the cluster repository: %w", path, err)
		}
		x.notify(ctx, "wrote %s to the cluster repository, %s", path, why)
		return nil
	})
}

func (x replacer) takeOut(path, why string) (progress, error) {
	return next("take "+path+" out of the cluster repository, "+why, func(ctx context.Context) error {
		if err := x.plane.Removals.Commit(ctx, path, nil); err != nil {
			return fmt.Errorf("taking %s out of the cluster repository: %w", path, err)
		}
		x.notify(ctx, "took %s out of the cluster repository, %s, so Argo CD prunes its Job", path, why)
		return nil
	})
}

func (x replacer) deleteServer(worker controlplane.Server, read report.Read) (progress, error) {
	return next("delete Cloud worker "+x.r.key+", whose Node object is gone", func(ctx context.Context) error {
		if err := x.plane.API.DeleteServer(ctx, worker.ID); err != nil {
			return fmt.Errorf("deleting Cloud worker %d: %w", worker.ID, err)
		}
		x.notify(ctx, "deleted Cloud worker %d (%s), whose Node object the node report read at %s no longer lists, so a worker from the worker image takes its place",
			worker.ID, x.r.key, stamp(read.At))
		return nil
	})
}

// createSuccessor boots the successor of the key's worker once the Job that removes its Node object has run, and takes
// that Job's file out in the same step, since Argo CD would run a held Job again against the successor of the same name.
func (x replacer) createSuccessor(ctx context.Context, held []string) (progress, error) {
	if drain := jobPath(drainStep, x.r.key); slices.Contains(held, drain) {
		return x.takeOut(drain, "before the successor of Cloud worker "+x.r.key+" is created")
	}
	read, err := x.plane.ReadReport(ctx, x.r.asked)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since the upgrade asked for its replacement at %s", stamp(x.r.asked))
	}
	if g, removed, err := x.removal(*read, x.plane.LocatedName(RoleCloudWorker, x.r.key), held); err != nil || !removed {
		return g, err
	}
	site, err := newSite(x.st)
	if err != nil {
		return progress{}, err
	}
	loc := x.r.location()
	spec, err := site.specAt(ctx, x.plane, x.pool, loc, x.r.key)
	if err != nil {
		return progress{}, err
	}
	why := ""
	if i := slices.Index(x.pool.ServerTypes, spec.ServerType); i > 0 {
		why = fmt.Sprintf(", since Hetzner can order no %s there now", strings.Join(x.pool.ServerTypes[:i], " or "))
	}
	path := jobPath(removeNodeStep, x.r.key)
	return next("take "+path+" out and create the successor of Cloud worker "+x.r.key+" in "+loc+" from the worker image", func(ctx context.Context) error {
		if err := x.plane.Removals.Commit(ctx, path, nil); err != nil {
			return fmt.Errorf("taking %s out of the cluster repository: %w", path, err)
		}
		if _, err := x.plane.API.CreateServers(ctx, spec); err != nil {
			return fmt.Errorf("creating the successor of Cloud worker %s: %w", x.r.key, err)
		}
		x.notify(ctx, "took %s out of the cluster repository and created the successor of Cloud worker %s in %s as %s%s", path, x.r.key, loc, spec.ServerType, why)
		return nil
	})
}

// endStep takes the record off the network once a node report since the successor's creation reads its Node object
// Ready, so the next worker an upgrade names goes only once this one has joined.
func (x replacer) endStep(ctx context.Context, successor controlplane.Server) (progress, error) {
	read, err := x.plane.ReadReport(ctx, successor.Created)
	if err != nil {
		return progress{}, err
	}
	if read != nil {
		nodes, err := read.Nodes([]string{successor.Name})
		if err != nil {
			return progress{}, err
		}
		if reading := nodes[0].Reading; reading != nil && reading.Serving {
			return next("take the replacement's record off the network, since a node report reads the successor of Cloud worker "+x.r.key+" Ready", func(ctx context.Context) error {
				if err := x.unrecord(ctx); err != nil {
					return err
				}
				x.notify(ctx, "Cloud worker %d (%s), created at %s, took the place of the worker the upgrade replaced", successor.ID, x.r.key, stamp(successor.Created))
				return nil
			})
		}
	}
	return waitsOn("Cloud worker %d (%s) has not been read Ready since its creation at %s", successor.ID, x.r.key, stamp(successor.Created))
}

func (x replacer) notify(ctx context.Context, format string, args ...any) {
	if x.plane.Notify != nil {
		x.plane.Notify(ctx, fmt.Sprintf(format, args...))
	}
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
