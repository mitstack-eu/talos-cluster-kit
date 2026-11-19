package robotcluster

// A release's machine configuration reaches each running node through one rollout Job main holds, since the platform
// never dials into a cluster.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/noderollout"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

type nodeRollout struct {
	// problem names the nodes behind the release, or why the newest report cannot tell which are.
	problem string
	head    string
	changes map[string]*string
	// rerun takes out a Job that completed before a node behind the release joined, since Argo CD runs a Job only as
	// it creates it.
	rerun bool
}

// rollNodes commits the rollout once main holds the release's component set and the cluster otherwise stands, since
// the Job changes one node at a time and must not run beside a step that replaces one.
func (c convergence) rollNodes(ctx context.Context, due carriage, network controlplane.Network) error {
	r, err := c.rolloutDue(ctx, due, network)
	if err != nil || len(r.changes) == 0 {
		return refusal(err)
	}
	message := "Take out a machine configuration rollout no node waits on"
	if r.rerun {
		message = "Take out a machine configuration rollout that ran before a node joined, so it runs again"
	}
	for _, content := range r.changes {
		if content != nil {
			message = "Give each node the release's machine configuration"
		}
	}
	gh := c.req.gh
	if _, err := gh.Client.CommitChanges(ctx, gh.Repo.FullName(), r.head, message+"\n\n"+observe.GitHubSubscriptionTrailer(gh.SubscriptionID()), r.changes); err != nil {
		return fmt.Errorf("committing the machine configuration rollout to %s: %w", gh.Repo.FullName(), err)
	}
	return nil
}

// rolloutDue is the rollout a subscribe sent now would commit.
func (c convergence) rolloutDue(ctx context.Context, due carriage, network controlplane.Network) (nodeRollout, error) {
	if c.plane.GatewayCA == nil || len(due.changes) > 0 || due.release.ToolsImage == "" {
		return nodeRollout{}, nil
	}
	if held, err := pools.HoldsRollout(ctx, c.plane, c.state); err != nil || held {
		return nodeRollout{}, err
	}
	if !c.req.params.cloudOnly && workerProgress(network) != WorkerWritten {
		return nodeRollout{}, nil
	}
	if len(c.plane.RolloutBlockers(ctx, c.state)) > 0 {
		return nodeRollout{}, nil
	}
	return readRollout(ctx, c.plane, c.state, c.req.gh, due.release.ToolsImage, c.robotWorker())
}

func (c convergence) robotWorker() string {
	if c.req.params.cloudOnly {
		return ""
	}
	return c.plane.Name("worker")
}

func readRollout(ctx context.Context, p controlplane.Plane, st controlplane.State, gh *githubAccess, toolsImage, worker string) (nodeRollout, error) {
	release, err := noderollout.Release(toolsImage)
	if err != nil {
		return nodeRollout{}, err
	}
	since := newestServer(st)
	read, problem, err := rolloutReport(ctx, p, st, since, worker)
	if err != nil || problem != "" {
		return nodeRollout{problem: problem}, err
	}
	behind, err := read.NodesBehind(release)
	if err != nil {
		return nodeRollout{}, err
	}
	head, err := gh.Client.HeadCommit(ctx, gh.Repo.FullName())
	if err != nil {
		return nodeRollout{}, err
	}
	if head == "" {
		return nodeRollout{}, fmt.Errorf("the cluster repository %s has no main to commit the machine configuration rollout to", gh.Repo.FullName())
	}
	held, err := gh.heldBlobs(ctx, head)
	if err != nil {
		return nodeRollout{}, err
	}
	path := noderollout.Path(release)
	_, holds := held[path]
	r := nodeRollout{head: head, changes: map[string]*string{}}
	for other := range held {
		if noderollout.IsPath(other) && (other != path || len(behind) == 0) {
			r.changes[other] = nil
		}
	}
	if len(behind) == 0 {
		return r, nil
	}
	r.problem = fmt.Sprintf("the newest node report reads %s on a machine configuration other than the release's, which %s gives each node",
		strings.Join(behind, ", "), noderollout.JobName(release))
	jobs, readable, err := read.AppliedJobs(controlplane.RemovalApplication)
	if err != nil {
		return nodeRollout{}, err
	}
	// The Job lists the nodes once, as it starts, so one that completed has given its configuration to every node then.
	ran, applied := jobs[noderollout.JobName(release)]
	switch {
	case holds && readable && applied && ran.Health == report.JobCompleted:
		r.changes[path], r.rerun = nil, true
		r.problem += ", and it completed before they joined, so it is taken out to run again"
		return r, nil
	case holds:
		return r, nil
	case applied:
		r.problem += ", and Argo CD has yet to prune the one that completed before they joined"
		return r, nil
	}
	job, err := noderollout.Job(noderollout.Rollout{ToolsImage: toolsImage, Report: *read, StepStart: since, Guard: p.Guard})
	if err != nil {
		return nodeRollout{}, controlplane.BelowMinimum(err)
	}
	content := string(job)
	r.changes[path] = &content
	return r, nil
}

// rolloutReport is the newest report made since since, which must read Ready every node the cluster runs, since a
// report that predates a server or omits a node would read a node it never saw as none behind.
func rolloutReport(ctx context.Context, p controlplane.Plane, st controlplane.State, since time.Time, worker string) (*report.Read, string, error) {
	created := since.UTC().Format(time.RFC3339)
	read, err := p.ReadReport(ctx, since)
	if err != nil {
		return nil, "", err
	}
	if read == nil {
		return nil, "the cluster has sent no node report since its newest server was created at " + created + ", so which nodes run the release's machine configuration is unknown", nil
	}
	if read.At.Before(since) {
		return nil, fmt.Sprintf("the newest node report was made at %s, before the cluster's newest server was created at %s, so which nodes run the release's machine configuration is unknown",
			read.At.UTC().Format(time.RFC3339), created), nil
	}
	var names []string
	for _, s := range append(st.Servers[controlplane.RoleNode], st.Servers[pools.RoleCloudWorker]...) {
		names = append(names, s.Name)
	}
	if worker != "" {
		names = append(names, worker)
	}
	nodes, err := read.Nodes(names)
	if err != nil {
		return nil, "", err
	}
	for _, n := range nodes {
		switch {
		case n.Reading == nil:
			return nil, "the newest node report does not name node " + n.Name + ", so whether it runs the release's machine configuration is unknown", nil
		case !n.Reading.Serving:
			return nil, "the newest node report reads node " + n.Name + " not Ready, so no node takes the release's machine configuration", nil
		}
	}
	return read, "", nil
}

// newestServer stands in for the Update's start, which the platform does not pass a block: the last server an Update
// or a replacement builds.
func newestServer(st controlplane.State) time.Time {
	var newest time.Time
	for _, role := range []controlplane.Role{controlplane.RoleGateway, controlplane.RoleNode, pools.RoleCloudWorker} {
		if last := lastCreated(st.Servers[role]); last.After(newest) {
			newest = last
		}
	}
	return newest
}
