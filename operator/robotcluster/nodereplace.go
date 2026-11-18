package robotcluster

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

func requestedServerTypes(d contract.Desired) ([]string, error) {
	return readServerTypes(cmp.Or(d.Params["serverType"], controlplane.DefaultServerType))
}

// namedNodes are the locations of the control-plane nodes a request names in replace, which the platform sends on the
// upgrade's own call alone, where the kit records them.
func namedNodes(replace []string) (locs []string) {
	for _, loc := range controlplane.Locations {
		if slices.Contains(replace, Located(ResourceNode, loc)) {
			locs = append(locs, loc)
		}
	}
	return locs
}

// PlanReplacements names the nodes and the Cloud workers the request names that the cluster runs, so the platform
// refuses any other name before the upgrade, and none for a request the upgrade would refuse before it reads the cluster.
// It refuses a control-plane size the cluster cannot run, so the platform's dry run shows that refusal.
func (k *Cluster) PlanReplacements(ctx context.Context, d contract.Desired) ([]string, error) {
	size, err := operator.ControlPlaneSize(d)
	if err != nil {
		return nil, err
	}
	if err := k.refuseUndeclaredSize(size); err != nil {
		return nil, err
	}
	if k.cloudOnly(d) {
		if _, err := k.readPool(d); err != nil {
			return nil, err
		}
	}
	locs, keys := namedNodes(d.Replace), namedWorkers(d.Replace)
	_, err = requestedServerTypes(d)
	p, _, ok := k.hetznerReader(d)
	if len(locs)+len(keys) == 0 || err != nil || !ok {
		return nil, nil
	}
	nodes, err := p.API.ListServers(ctx, p.RoleSelector(controlplane.RoleNode))
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's nodes to name those an upgrade would replace: %w", err)
	}
	st := controlplane.State{Servers: map[controlplane.Role][]controlplane.Server{controlplane.RoleNode: nodes}}
	var names []string
	for _, loc := range locs {
		if _, runs := st.NodeIn(loc); runs {
			names = append(names, Located(ResourceNode, loc))
		}
	}
	workers, err := heldWorkers(ctx, p, keys)
	return append(names, workers...), err
}

// removals lets the plane write a member removal, and the Cloud pool the Jobs of a worker it replaces or removes, to
// the cluster repository, each call reading main's head anew.
func (g *githubAccess) removals() *controlplane.Removals {
	return &controlplane.Removals{Held: g.heldRemovals, Commit: g.commitRemoval}
}

func isRemovalPath(path string) bool { return etcd.IsRemovalPath(path) || pools.IsRemovalPath(path) }

// removalMessages are the commit messages that write and take out the file at path, by the kind of removal it holds.
func removalMessages(path string) (write, takeOut string) {
	if pools.IsRemovalPath(path) {
		return "Drain a Cloud worker the pool replaces or removes, or remove its Node object", "Take out the drain or Node object removal of a Cloud worker"
	}
	return "Remove a replaced node's etcd member and Node object", "Take out the removal of a replaced node's etcd member"
}

func (g *githubAccess) heldRemovals(ctx context.Context) ([]string, error) {
	head, err := g.Client.HeadCommit(ctx, g.Repo.FullName())
	if err != nil || head == "" {
		return nil, err
	}
	held, err := g.heldBlobs(ctx, head)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Sorted(maps.Keys(held)), func(path string) bool { return !isRemovalPath(path) }), nil
}

func (g *githubAccess) commitRemoval(ctx context.Context, path string, content []byte) error {
	head, err := g.Client.HeadCommit(ctx, g.Repo.FullName())
	if err != nil {
		return err
	}
	if head == "" {
		return fmt.Errorf("the cluster repository %s has no main to commit %s to", g.Repo.FullName(), path)
	}
	write, takeOut := removalMessages(path)
	changes, message := map[string]*string{path: nil}, takeOut
	if content != nil {
		written := string(content)
		changes[path], message = &written, write
	}
	if _, err := g.Client.CommitChanges(ctx, g.Repo.FullName(), head, message+"\n\n"+observe.GitHubSubscriptionTrailer(g.SubscriptionID()), changes); err != nil {
		return fmt.Errorf("committing %s to %s: %w", path, g.Repo.FullName(), err)
	}
	return nil
}
