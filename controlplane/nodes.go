package controlplane

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

const (
	// KeyNodeReplace on the network, followed by a location, records the node an upgrade asked to replace there, so
	// each later call carries the replacement on and reads another server in that location as its successor.
	KeyNodeReplace = "mitstack.dev/node-replace-"
	// nodeReplacePlan, followed by that record, is the plan a replaced node's removal labels name.
	nodeReplacePlan = "node-replace-"
	// removalApplication is the Application of the cluster repository that applies and prunes each removal file.
	removalApplication = "cluster"
	jobKind            = "Job"
	// jobCompleted and jobFailed are the health Argo CD reads of a Job that completed and of one that failed.
	jobCompleted = "Healthy"
	jobFailed    = "Degraded"
	// nodeWhat names a control-plane node in the refusal of a location that can order none of its types.
	nodeWhat = "control-plane node"
)

// Removals is the cluster repository as the plane writes a member removal to it: a file the cluster's own Application
// applies while main holds it, and whose Job that Application prunes once main does not.
type Removals struct {
	// Held lists each removal file main holds, by its path.
	Held func(ctx context.Context) ([]string, error)
	// Commit writes content at path on main, and takes the file out when content is nil.
	Commit func(ctx context.Context, path string, content []byte) error
}

func (r *Removals) writable() bool { return r != nil && r.Held != nil && r.Commit != nil }

type nodeReplacement struct {
	loc   string
	old   int64
	asked time.Time
}

// record is the label value that holds r, which is also the plan its node's removal labels name.
func (r nodeReplacement) record() string {
	return strconv.FormatInt(r.old, 10) + "-" + strconv.FormatInt(r.asked.Unix(), 10)
}

func (r nodeReplacement) plan() string { return nodeReplacePlan + r.record() }

// offPlan is the plan the node's removal labels name from the call that found it powered off.
func (r nodeReplacement) offPlan() string { return nodeReplacePlan + "off-" + r.record() }

// succeededBy reports whether node is another server than the one r names, by id and by no time, so a clock that
// differs from Hetzner's never reads the successor as the node to replace.
func (r nodeReplacement) succeededBy(node Server) bool { return node.ID != r.old }

func nodeReplacements(network Network) ([]nodeReplacement, error) {
	var asked []nodeReplacement
	for _, loc := range Locations {
		value, ok := network.Labels[KeyNodeReplace+loc]
		if !ok {
			continue
		}
		id, unix, _ := strings.Cut(value, "-")
		old, idErr := strconv.ParseInt(id, 10, 64)
		at, atErr := strconv.ParseInt(unix, 10, 64)
		if idErr != nil || atErr != nil {
			return nil, fmt.Errorf("network %d carries the label %s=%q, which is no replacement the plane recorded", network.ID, KeyNodeReplace+loc, value)
		}
		asked = append(asked, nodeReplacement{loc: loc, old: old, asked: time.Unix(at, 0).UTC()})
	}
	return asked, nil
}

func withoutReplacement(network Network, loc string) map[string]string {
	labels := maps.Clone(network.Labels)
	delete(labels, KeyNodeReplace+loc)
	return labels
}

type progress struct {
	step  Step
	waits string
}

func next(what string, run func(ctx context.Context) error) (progress, error) {
	return progress{step: Step{what, run}}, nil
}

func waitsOn(format string, args ...any) (progress, error) {
	return progress{waits: fmt.Sprintf(format, args...)}, nil
}

// replaceNodeStep carries the first recorded location's replacement on, so no second node goes before a report reads
// the first one's successor serving.
func (p Plane) replaceNodeStep(ctx context.Context, st State, s site) (Step, bool, error) {
	asked, err := nodeReplacements(s.network)
	if err != nil || len(asked) == 0 {
		return Step{}, false, err
	}
	r := asked[0]
	if class, what := p.lacks(); class != "" {
		return Step{}, false, refuse(class, "the replacement of the node in %s is under way, and the block passes %s", r.loc, what)
	}
	node, ok := st.NodeIn(r.loc)
	switch {
	case !ok:
		return Step{}, false, nil
	case r.succeededBy(node):
		step, ok, err := p.endNodeReplacementStep(ctx, s.network, r, node)
		return step, ok, r.named(err)
	}
	g, err := p.replaceNode(ctx, st, s.network, r, node)
	return g.step, g.step.Run != nil, r.named(err)
}

// named says which replacement err stopped, and leaves a refusal as it is, since its message already says.
func (r nodeReplacement) named(err error) error {
	var refusal *Refusal
	if err == nil || errors.As(err, &refusal) {
		return err
	}
	return fmt.Errorf("replacing the node in %s: %w", r.loc, err)
}

// strayRemovalStep takes out each removal main still holds once the node the upgrade replaces is gone, whichever node
// it names, since the plane writes one removal at a time and a held one could run again against a node that serves.
func (p Plane) strayRemovalStep(ctx context.Context, st State, network Network) (Step, bool, error) {
	asked, err := nodeReplacements(network)
	if err != nil || len(asked) == 0 || !p.Removals.writable() {
		// replaceNodeStep answers a record it cannot read, once the rest of the control plane has no step left.
		return Step{}, false, nil
	}
	r := asked[0]
	if node, ok := st.NodeIn(r.loc); ok && !r.succeededBy(node) {
		return Step{}, false, nil
	}
	held, err := p.Removals.Held(ctx)
	if err != nil {
		return Step{}, false, r.named(fmt.Errorf("listing the removals the cluster repository holds: %w", err))
	}
	if len(held) == 0 {
		return Step{}, false, nil
	}
	g, err := p.takeOut(held[0], "since the node in "+r.loc+" that the upgrade replaces is gone")
	return g.step, true, err
}

// recordReplacements records each node the upgrade names that the network does not record yet, and refuses before
// recordUpgradeStep writes anything, so an upgrade never leaves a part of what it named.
func (p Plane) recordReplacements(ctx context.Context, st State, _ Network, rec *upgradeRecord) error {
	if len(p.ReplaceNodes) == 0 {
		return nil
	}
	if class, what := p.lacks(); class != "" {
		return refuse(class, "the upgrade names a node to replace, and the block passes %s, so nothing was changed", what)
	}
	orderable := &OrderableTypes{API: p.API}
	var named []string
	for _, loc := range p.ReplaceNodes {
		node, ok := st.NodeIn(loc)
		if !ok {
			return refuse("node-replacement-unknown",
				"the upgrade names the node in %q to replace, and the cluster runs no single node there, so nothing was changed; it runs one in each of %s",
				loc, strings.Join(Locations, ", "))
		}
		if _, recorded := rec.labels[KeyNodeReplace+loc]; recorded {
			continue
		}
		if _, err := orderable.First(ctx, nodeWhat, p.ServerTypes, loc); err != nil {
			return err
		}
		rec.labels[KeyNodeReplace+loc] = nodeReplacement{loc: loc, old: node.ID, asked: p.now()}.record()
		named = append(named, loc)
	}
	if len(named) > 0 {
		rec.replaces = append(rec.replaces, "the node in "+strings.Join(named, ", "))
	}
	return nil
}

// endNodeReplacementStep takes r's record off the network once a node report reads the successor serving, so the next
// node an upgrade names goes only when this one has joined.
func (p Plane) endNodeReplacementStep(ctx context.Context, network Network, r nodeReplacement, successor Server) (Step, bool, error) {
	if serves, err := p.successorServes(ctx, successor); err != nil || !serves {
		return Step{}, false, err
	}
	return Step{"take the replacement's record off the network, since a node report reads the new node in " + r.loc + " serving", func(ctx context.Context) error {
		if err := p.API.SetNetworkLabels(ctx, network.ID, withoutReplacement(network, r.loc)); err != nil {
			return err
		}
		p.notify(ctx, "node %d in %s, created at %s, took the place of the node the upgrade replaced",
			successor.ID, r.loc, successor.Created.UTC().Format(time.RFC3339))
		return nil
	}}, true, nil
}

func (p Plane) successorServes(ctx context.Context, successor Server) (bool, error) {
	read, err := p.ReadReport(ctx, successor.Created)
	if err != nil || read == nil {
		return false, err
	}
	members, err := read.EtcdMembers()
	if err != nil {
		return false, err
	}
	nodes, err := read.Nodes([]string{successor.Name})
	if err != nil {
		return false, err
	}
	serves := func(m report.Member) bool { return m.Name == successor.Name && m.Reading != nil && m.Reading.Serving }
	return slices.ContainsFunc(members, serves) && slices.ContainsFunc(nodes, serves), nil
}

// nodeReplacementProblems names where each replacement the network records stands, so a plane with one under way never
// reads as complete and each wait says what it is for.
func (p Plane) nodeReplacementProblems(ctx context.Context, st State, network Network) []string {
	recorded, err := nodeReplacements(network)
	if err != nil {
		return []string{err.Error()}
	}
	if _, what := p.lacks(); what != "" && len(recorded) > 0 {
		return []string{fmt.Sprintf("the replacement of the node in %s cannot go on, since the block passes %s", recorded[0].loc, what)}
	}
	var out []string
	for i, r := range recorded {
		asked := "the replacement the upgrade asked for at " + r.asked.Format(time.RFC3339)
		node, ok := st.NodeIn(r.loc)
		switch {
		case i > 0:
			out = append(out, fmt.Sprintf("the node in %s waits for %s until the replacement before it has ended", r.loc, asked))
		case !ok:
			out = append(out, fmt.Sprintf("the node in %s is deleted for %s, and its successor is not created yet", r.loc, asked))
		case r.succeededBy(node):
			out = append(out, p.successorProblem(ctx, network, r, node))
		default:
			out = append(out, fmt.Sprintf("node %d in %s %s for %s: %s", node.ID, r.loc, standing(node), asked, p.replacementDetail(ctx, st, network, r, node)))
		}
	}
	return out
}

func standing(node Server) string {
	switch node.Status {
	case ServerRunning:
		return "still runs"
	case ServerOff:
		return "is powered off"
	}
	return "is " + node.Status
}

func (p Plane) replacementDetail(ctx context.Context, st State, network Network, r nodeReplacement, node Server) string {
	switch g, err := p.replaceNode(ctx, st, network, r, node); {
	case err != nil:
		return "it cannot go on, since " + err.Error()
	case g.step.Run != nil:
		return "its next step is to " + g.step.What
	default:
		return g.waits
	}
}

func (p Plane) successorProblem(ctx context.Context, network Network, r nodeReplacement, successor Server) string {
	switch serves, err := p.successorServes(ctx, successor); {
	case err != nil:
		return fmt.Sprintf("node %d in %s, which replaces the node the upgrade named, cannot be judged: %v", successor.ID, r.loc, err)
	case serves:
		return fmt.Sprintf("node %d in %s serves, and network %d still carries its replacement's record", successor.ID, r.loc, network.ID)
	}
	return fmt.Sprintf("node %d in %s has not been read serving since its creation at %s, so the replacement the upgrade asked for waits on a node report that reads its etcd member healthy and its Node object Ready",
		successor.ID, r.loc, successor.Created.UTC().Format(time.RFC3339))
}

// refused withdraws r when err refuses it, so a replacement refused while its node still runs leaves no record a later
// call would carry on; any other error it answers as it is.
func (p Plane) refused(network Network, r nodeReplacement, err error) (progress, error) {
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return progress{}, err
	}
	return next("withdraw the replacement of the node in "+r.loc+", which still runs, since it is refused: "+refusal.Message, func(ctx context.Context) error {
		if werr := p.API.SetNetworkLabels(ctx, network.ID, withoutReplacement(network, r.loc)); werr != nil {
			return errors.Join(err, fmt.Errorf("withdrawing the refused replacement of the node in %s: %w", r.loc, werr))
		}
		p.notify(ctx, "withdrew the replacement of the node in %s, which still runs: %s", r.loc, refusal.Message)
		return err
	})
}

// backOn powers node on again and withdraws r, since its etcd member, which the report still lists, is not removed; it takes
// the removal at held out first, when there is one, so no removal stands beside a node that runs.
func (p Plane) backOn(network Network, r nodeReplacement, node Server, held string, refusal *Refusal) (progress, error) {
	return next("power the node in "+r.loc+" back on and withdraw its replacement, since it is refused: "+refusal.Message, func(ctx context.Context) error {
		if held != "" {
			if err := p.Removals.Commit(ctx, held, nil); err != nil {
				return errors.Join(refusal, fmt.Errorf("taking %s out of the cluster repository: %w", held, err))
			}
		}
		if err := p.API.PowerOnServer(ctx, node.ID); err != nil {
			return errors.Join(refusal, fmt.Errorf("powering node %d in %s back on: %w", node.ID, r.loc, err))
		}
		if err := p.API.SetNetworkLabels(ctx, network.ID, withoutReplacement(network, r.loc)); err != nil {
			return errors.Join(refusal, fmt.Errorf("withdrawing the refused replacement of the node in %s: %w", r.loc, err))
		}
		p.notify(ctx, "powered node %d in %s back on and withdrew its replacement: %s", node.ID, r.loc, refusal.Message)
		return refusal
	})
}

// lacks names what a node replacement needs on every call and the block does not pass, with the class that refuses it.
func (p Plane) lacks() (class, what string) {
	switch {
	case !p.Removals.writable():
		return "node-removal-unwritable", "no cluster repository to write its etcd member's removal to"
	case p.ReadReport == nil || p.Guard == nil:
		return "node-replacement-unjudged", "no node report read or no redundancy guard to judge the other members by"
	}
	return "", ""
}

func (p Plane) replaceNode(ctx context.Context, st State, network Network, r nodeReplacement, node Server) (progress, error) {
	switch node.Status {
	case ServerRunning:
		return p.powerOffNode(ctx, st, network, r, node)
	case ServerOff:
		return p.removeOffNode(ctx, st, network, r, node)
	}
	return waitsOn("Hetzner reads it neither running nor off")
}

func (p Plane) powerOffNode(ctx context.Context, st State, network Network, r nodeReplacement, node Server) (progress, error) {
	if _, err := (&OrderableTypes{API: p.API}).First(ctx, nodeWhat, p.ServerTypes, r.loc); err != nil {
		return p.refused(network, r, err)
	}
	began, begun := RemovalBegan(node, r.plan())
	if !begun {
		return p.beginNodeReplacement(st, network, r, node)
	}
	read, err := p.ReadReport(ctx, began)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since its replacement began at %s", began.UTC().Format(time.RFC3339))
	}
	if err := p.allowNodeRemoval(*read, node, began); err != nil {
		return p.refused(network, r, err)
	}
	return next("power off the node in "+r.loc+", whose etcd member and Node object go before its server", func(ctx context.Context) error {
		if err := p.API.PowerOffServer(ctx, node.ID); err != nil {
			return err
		}
		p.notify(ctx, "powered off node %d in %s, since the node report read at %s reads the other etcd and OpenBao members serving",
			node.ID, r.loc, read.At.UTC().Format(time.RFC3339))
		return nil
	})
}

// beginNodeReplacement holds a replacement while the upgrade replaces the gateways, so no successor boots while a gateway
// it names is away: a control-plane node fetches each served manifest from the first location's gateway.
func (p Plane) beginNodeReplacement(st State, network Network, r nodeReplacement, node Server) (progress, error) {
	gateways, err := p.replacement(st, network)
	switch {
	case err != nil:
		return progress{}, err
	case gateways.active:
		return waitsOn("it begins once the upgrade has replaced every gateway")
	}
	return next("begin replacing the node in "+r.loc+", so only a node report read after now judges the other members", func(ctx context.Context) error {
		if err := p.API.SetServerLabels(ctx, node.ID, BeginRemoval(node, p.now(), r.plan())); err != nil {
			return err
		}
		p.notify(ctx, "began replacing node %d in %s, which the upgrade named", node.ID, r.loc)
		return nil
	})
}

// removeOffNode judges the member's removal only by a node report read since the node was found off, so the report
// that allowed the power-off never allows the removal too.
func (p Plane) removeOffNode(ctx context.Context, st State, network Network, r nodeReplacement, node Server) (progress, error) {
	off, marked := RemovalBegan(node, r.offPlan())
	if !marked {
		return next("record when the node in "+r.loc+" was found off, so only a node report read after now judges its member's removal", func(ctx context.Context) error {
			return p.API.SetServerLabels(ctx, node.ID, BeginRemoval(node, p.now(), r.offPlan()))
		})
	}
	read, err := p.ReadReport(ctx, off)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since it was found off at %s", off.UTC().Format(time.RFC3339))
	}
	return p.removeMember(ctx, st, network, r, node, off, *read)
}

func (p Plane) allowNodeRemoval(read report.Read, node Server, began time.Time) error {
	if _, readable, err := appliedJobs(read); err != nil {
		return err
	} else if !readable {
		return refuse("node-replacement-unjudged", "%s, so no report could show a removal Job that failed", unreadJobs(read))
	}
	members, err := read.EtcdMembers()
	if err != nil {
		return err
	}
	sets := []report.Removal{{Set: redundant.Set{Name: "etcd members", Members: members, Minimum: redundant.Majority}, Removed: node.Name}}
	openBao, err := read.OpenBaoRemovals(node.Name)
	if err != nil {
		return err
	}
	for _, s := range append(sets, openBao...) {
		if err := p.Guard.Allow(s.Set, s.Removed, began); err != nil {
			return belowMinimum(err)
		}
	}
	return nil
}

func belowMinimum(err error) error {
	if !redundant.Held(err) {
		return err
	}
	return &Refusal{Class: redundant.ClassBelowMinimum, Message: strings.TrimPrefix(err.Error(), "refused:"+redundant.ClassBelowMinimum+": ")}
}

// removeMember deletes the server only once main holds no removal, since self-heal applies what main holds and the
// cluster lacks, so a held removal could run again against the successor of the same name once its Job is deleted.
func (p Plane) removeMember(ctx context.Context, st State, network Network, r nodeReplacement, node Server, off time.Time, read report.Read) (progress, error) {
	held, err := p.Removals.Held(ctx)
	if err != nil {
		return progress{}, fmt.Errorf("listing the removals the cluster repository holds: %w", err)
	}
	left, err := leftOf(read, st, node.Name)
	if err != nil {
		return progress{}, err
	}
	path := etcd.RemovalPath(node.Name)
	holds := slices.Contains(held, path)
	switch {
	case left.nothing() && holds:
		return p.takeOut(path, "whose Job removed the member of "+node.Name)
	case left.nothing():
		return p.deleteNode(node, read)
	}
	for _, other := range held {
		if other != path {
			return p.takeOut(other, "which an earlier replacement left")
		}
	}
	name, at := etcd.JobName(node.Name), read.At.UTC().Format(time.RFC3339)
	jobs, readable, err := appliedJobs(read)
	job, applied := jobs[name]
	switch {
	case err != nil:
		return progress{}, err
	case !readable:
		return waitsOn("%s, so its removal Job %s cannot be read", unreadJobs(read), name)
	case holds && job.health == jobFailed && left.member:
		return p.backOn(network, r, node, path, &Refusal{Class: "node-removal-failed", Message: fmt.Sprintf(
			"the removal Job %s of the node in %s failed and left its etcd member, in the node report read at %s: %s",
			name, r.loc, at, cmp.Or(job.message, "Argo CD gives no message of it"))})
	case holds && job.health == jobFailed:
		return p.takeOut(path, "whose Job failed once the member of "+node.Name+" was gone and left its Node object")
	case holds && job.health == jobCompleted:
		return p.takeOut(path, "whose Job completed in a report that still lists the member of "+node.Name)
	case holds:
		return waitsOn("its removal Job %s has not finished in the node report read at %s", name, at)
	case !applied:
		g, err := p.writeRemoval(node, off, read, path)
		var refusal *Refusal
		if errors.As(err, &refusal) && left.member {
			return p.backOn(network, r, node, "", refusal)
		}
		return g, err
	}
	return waitsOn("Argo CD still holds the removal Job %s in the node report read at %s, and prunes it now that the cluster repository no longer holds its file", name, at)
}

// remains is what a node report still lists of a node: its etcd member and its Node object, proven only when the
// report lists both for another node, since a report whose read of either failed lists none and proves nothing.
type remains struct{ member, object, proven bool }

func (l remains) nothing() bool { return l.proven && !l.member && !l.object }

func leftOf(read report.Read, st State, hostname string) (remains, error) {
	members, err := read.EtcdMembers()
	if err != nil {
		return remains{}, err
	}
	names := make([]string, 0, len(st.Servers[RoleNode]))
	for _, node := range st.Servers[RoleNode] {
		names = append(names, node.Name)
	}
	nodes, err := read.Nodes(names)
	if err != nil {
		return remains{}, err
	}
	listed := func(m report.Member) bool { return m.Name == hostname && m.Reading != nil }
	otherListed := func(m report.Member) bool { return m.Name != hostname && m.Reading != nil }
	return remains{
		member: slices.ContainsFunc(members, listed), object: slices.ContainsFunc(nodes, listed),
		proven: slices.ContainsFunc(members, otherListed) && slices.ContainsFunc(nodes, otherListed),
	}, nil
}

type appliedJob struct{ health, message string }

// appliedJobs reads the health Argo CD keeps of each Job the cluster's own Application applied, and false when read
// carries none: a body near its size limit drops the objects, and an Argo CD that keeps health off the Application
// leaves each object without one.
func appliedJobs(read report.Read) (map[string]appliedJob, bool, error) {
	var body struct {
		ArgoCD struct {
			Applications []struct {
				Name      string            `json:"name"`
				Resources *[]map[string]any `json:"resources"`
			} `json:"applications"`
		} `json:"argocd"`
	}
	if err := json.Unmarshal(read.Body, &body); err != nil {
		return nil, false, fmt.Errorf("the node report read at %s cannot be read: %w", read.At.UTC().Format(time.RFC3339), err)
	}
	for _, app := range body.ArgoCD.Applications {
		if app.Name != removalApplication || app.Resources == nil {
			continue
		}
		jobs := map[string]appliedJob{}
		for _, r := range *app.Resources {
			health, kept := r["health"]
			if !kept {
				return nil, false, nil
			}
			if r["kind"] == jobKind {
				name, _ := r["name"].(string)
				status, _ := health.(string)
				message, _ := r["message"].(string)
				jobs[name] = appliedJob{health: status, message: message}
			}
		}
		return jobs, true, nil
	}
	return nil, false, nil
}

func unreadJobs(read report.Read) string {
	return fmt.Sprintf("the node report read at %s carries no health of what the Application %s applied", read.At.UTC().Format(time.RFC3339), removalApplication)
}

func (p Plane) takeOut(path, why string) (progress, error) {
	return next("take "+path+" out of the cluster repository, "+why, func(ctx context.Context) error {
		if err := p.Removals.Commit(ctx, path, nil); err != nil {
			return fmt.Errorf("taking %s out of the cluster repository: %w", path, err)
		}
		p.notify(ctx, "took %s out of the cluster repository, %s, so Argo CD prunes its Job", path, why)
		return nil
	})
}

// writeRemoval renders the removal once more through the guard, since the node is off by now and a second member may
// have stopped serving since.
func (p Plane) writeRemoval(node Server, began time.Time, read report.Read, path string) (progress, error) {
	job, err := etcd.RemoveMemberJob(etcd.RemoveMember{Hostname: node.Name, ToolsImage: toolsImage(), Report: read, StepStart: began, Guard: p.Guard})
	if err != nil {
		return progress{}, belowMinimum(err)
	}
	return next("write the removal of "+node.Name+"'s etcd member and Node object to the cluster repository", func(ctx context.Context) error {
		if err := p.Removals.Commit(ctx, path, job); err != nil {
			return fmt.Errorf("writing %s to the cluster repository: %w", path, err)
		}
		p.notify(ctx, "wrote %s to the cluster repository, whose Job removes the etcd member and the Node object of %s", path, node.Name)
		return nil
	})
}

func (p Plane) deleteNode(node Server, read report.Read) (progress, error) {
	return next("delete the node in "+node.Location+", whose etcd member and Node object are gone", func(ctx context.Context) error {
		if err := p.API.DeleteServer(ctx, node.ID); err != nil {
			return fmt.Errorf("deleting the node %d in %s: %w", node.ID, node.Location, err)
		}
		p.notify(ctx, "deleted node %d in %s, whose etcd member and Node object the node report read at %s no longer lists, so a node from the cluster's snapshot takes its place",
			node.ID, node.Location, read.At.UTC().Format(time.RFC3339))
		return nil
	})
}
