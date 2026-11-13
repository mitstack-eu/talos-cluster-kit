package controlplane

import (
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

// replaceNodeStep records the nodes an upgrade names, then carries the first location's replacement on, so no second
// node goes before a report reads the first one's successor serving.
func (p Plane) replaceNodeStep(ctx context.Context, st State, s site) (Step, bool, error) {
	if step, ok, err := p.recordReplacementsStep(ctx, st, s.network); err != nil || ok {
		return step, ok, err
	}
	asked, err := nodeReplacements(s.network)
	if err != nil || len(asked) == 0 {
		return Step{}, false, err
	}
	r := asked[0]
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

// strayRemovalStep takes out a removal main still holds once the node the upgrade replaces is gone, so no Job removes
// the member of a successor of the same name.
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

// recordReplacementsStep refuses before it records anything, so an upgrade never leaves a part of what it named.
func (p Plane) recordReplacementsStep(ctx context.Context, st State, network Network) (Step, bool, error) {
	if !p.Upgrading || len(p.ReplaceNodes) == 0 {
		return Step{}, false, nil
	}
	if !p.Removals.writable() {
		return Step{}, false, refuse("node-removal-unwritable",
			"the upgrade names a node to replace, and the block passes no cluster repository to write its etcd member's removal to, so nothing was changed")
	}
	if p.ReadReport == nil || p.Guard == nil {
		return Step{}, false, refuse("node-replacement-unjudged",
			"the upgrade names a node to replace, and the block passes no node report read or no redundancy guard to judge the other members by, so nothing was changed")
	}
	labels := maps.Clone(network.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	orderable := &OrderableTypes{API: p.API}
	var asked []string
	for _, loc := range p.ReplaceNodes {
		node, ok := st.NodeIn(loc)
		if !ok {
			return Step{}, false, refuse("node-replacement-unknown",
				"the upgrade names the node in %q to replace, and the cluster runs no single node there, so nothing was changed; it runs one in each of %s",
				loc, strings.Join(Locations, ", "))
		}
		if _, recorded := labels[KeyNodeReplace+loc]; recorded {
			continue
		}
		if _, err := orderable.First(ctx, nodeWhat, p.ServerTypes, loc); err != nil {
			return Step{}, false, err
		}
		labels[KeyNodeReplace+loc] = nodeReplacement{loc: loc, old: node.ID, asked: p.now()}.record()
		asked = append(asked, loc)
	}
	if len(asked) == 0 {
		return Step{}, false, nil
	}
	return Step{"record that the upgrade replaces the node in " + strings.Join(asked, ", "), func(ctx context.Context) error {
		return p.API.SetNetworkLabels(ctx, network.ID, labels)
	}}, true, nil
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
	read, err := p.readReport(ctx, successor.Created)
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
		return "the next call will " + g.step.What
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

// backOn powers node on again and withdraws r when err refuses the removal of its member, which no step has written
// yet, so a second member that stopped serving meanwhile gets the node's member back.
func (p Plane) backOn(network Network, r nodeReplacement, node Server, err error) (progress, error) {
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		return progress{}, err
	}
	return next("power the node in "+r.loc+" back on and withdraw its replacement, since it is refused: "+refusal.Message, func(ctx context.Context) error {
		if perr := p.API.PowerOnServer(ctx, node.ID); perr != nil {
			return errors.Join(err, fmt.Errorf("powering node %d in %s back on: %w", node.ID, r.loc, perr))
		}
		if werr := p.API.SetNetworkLabels(ctx, network.ID, withoutReplacement(network, r.loc)); werr != nil {
			return errors.Join(err, fmt.Errorf("withdrawing the refused replacement of the node in %s: %w", r.loc, werr))
		}
		p.notify(ctx, "powered node %d in %s back on and withdrew its replacement: %s", node.ID, r.loc, refusal.Message)
		return err
	})
}

func (p Plane) replaceNode(ctx context.Context, st State, network Network, r nodeReplacement, node Server) (progress, error) {
	if !p.Removals.writable() {
		return progress{}, refuse("node-removal-unwritable",
			"the replacement of the node in %s is under way, and the block passes no cluster repository to write its etcd member's removal to", r.loc)
	}
	running := node.Status == ServerRunning
	if !running && node.Status != ServerOff {
		return waitsOn("Hetzner reads it neither running nor off")
	}
	if running {
		if _, err := (&OrderableTypes{API: p.API}).First(ctx, nodeWhat, p.ServerTypes, r.loc); err != nil {
			return p.refused(network, r, err)
		}
	}
	began, begun := RemovalBegan(node, r.plan())
	if !begun {
		return next("begin replacing the node in "+r.loc+", so only a node report read after now judges the other members", func(ctx context.Context) error {
			if err := p.API.SetServerLabels(ctx, node.ID, BeginRemoval(node, p.now(), r.plan())); err != nil {
				return err
			}
			p.notify(ctx, "began replacing node %d in %s, which the upgrade named", node.ID, r.loc)
			return nil
		})
	}
	read, err := p.readReport(ctx, began)
	switch {
	case err != nil:
		return progress{}, err
	case read == nil:
		return waitsOn("no node report was received since its replacement began at %s", began.UTC().Format(time.RFC3339))
	case !running:
		return p.removeMember(ctx, st, network, r, node, began, *read)
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

func (p Plane) readReport(ctx context.Context, since time.Time) (*report.Read, error) {
	if p.ReadReport == nil {
		return nil, nil
	}
	return p.ReadReport(ctx, since)
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

// removeMember deletes the server only once main holds no removal, since Argo CD would run a held removal again, a day
// after its Job finished, against the successor of the same name.
func (p Plane) removeMember(ctx context.Context, st State, network Network, r nodeReplacement, node Server, began time.Time, read report.Read) (progress, error) {
	held, err := p.Removals.Held(ctx)
	if err != nil {
		return progress{}, fmt.Errorf("listing the removals the cluster repository holds: %w", err)
	}
	gone, err := memberGone(read, st, node.Name)
	if err != nil {
		return progress{}, err
	}
	path := etcd.RemovalPath(node.Name)
	holds := slices.Contains(held, path)
	switch {
	case gone && holds:
		return p.takeOut(path, "whose Job removed the member of "+node.Name)
	case gone:
		return p.deleteNode(node, read)
	}
	for _, other := range held {
		if other != path {
			return p.takeOut(other, "which an earlier replacement left")
		}
	}
	name, at := etcd.JobName(node.Name), read.At.UTC().Format(time.RFC3339)
	jobs, readable, err := appliedJobs(read)
	health, applied := jobs[name]
	switch {
	case err != nil:
		return progress{}, err
	case !readable:
		return waitsOn("%s, so its removal Job %s cannot be read", unreadJobs(read), name)
	case holds && health == jobFailed:
		return p.takeOut(path, "whose Job failed and left the member of "+node.Name)
	case holds && health == jobCompleted:
		return p.takeOut(path, "whose Job completed in a report that still lists the member of "+node.Name)
	case holds:
		return waitsOn("its removal Job %s has not finished in the node report read at %s", name, at)
	case !applied:
		g, err := p.writeRemoval(node, began, read, path)
		if err != nil {
			return p.backOn(network, r, node, err)
		}
		return g, nil
	}
	return waitsOn("Argo CD still holds the removal Job %s in the node report read at %s, and prunes it now that the cluster repository no longer holds its file", name, at)
}

// memberGone reports whether read lists neither an etcd member nor a Node object named hostname while it lists both
// for another node, since a report whose read of either failed lists none and proves nothing.
func memberGone(read report.Read, st State, hostname string) (bool, error) {
	members, err := read.EtcdMembers()
	if err != nil {
		return false, err
	}
	names := make([]string, 0, len(st.Servers[RoleNode]))
	for _, node := range st.Servers[RoleNode] {
		names = append(names, node.Name)
	}
	nodes, err := read.Nodes(names)
	if err != nil {
		return false, err
	}
	listed := func(m report.Member) bool { return m.Name == hostname && m.Reading != nil }
	otherListed := func(m report.Member) bool { return m.Name != hostname && m.Reading != nil }
	return !slices.ContainsFunc(members, listed) && !slices.ContainsFunc(nodes, listed) &&
		slices.ContainsFunc(members, otherListed) && slices.ContainsFunc(nodes, otherListed), nil
}

// appliedJobs reads the health Argo CD keeps of each Job the cluster's own Application applied, and false when read
// carries none: a body near its size limit drops the objects, and an Argo CD that keeps health off the Application
// leaves each object without one.
func appliedJobs(read report.Read) (map[string]string, bool, error) {
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
		jobs := map[string]string{}
		for _, r := range *app.Resources {
			health, kept := r["health"]
			if !kept {
				return nil, false, nil
			}
			if name, _ := r["name"].(string); r["kind"] == jobKind {
				jobs[name], _ = health.(string)
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
