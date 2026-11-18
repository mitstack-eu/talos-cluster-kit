package robotcluster

import (
	"context"
	"fmt"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

// Delete removes the cluster's labelled Cloud resources, the snapshot holding its keys among them, and its repository, and
// for a cluster with a dedicated server also stops its install, cancels the vSwitch and releases the server without
// cancelling it; a missing Robot login or GitHub connection leaves that part in place and refuses, naming it.
func (k *Cluster) Delete(ctx context.Context, d contract.Desired) (map[string]string, error) {
	token, err := operator.HetznerToken(d)
	if err != nil {
		return nil, err
	}
	subID, err := subscriptionID(d)
	if err != nil {
		return nil, err
	}
	// The reading comes before the Cloud delete, since a dropped server is told by the network the delete removes.
	server, reading, readable := k.robotReader(ctx, d)
	if reading.State != contract.Absent {
		k.narrate(ctx, d, "stopping the dedicated server's install, if one runs")
		if err := k.StopJob(ctx, subID); err != nil {
			return nil, err
		}
	}
	k.narrate(ctx, d, "deleting the cluster's Hetzner Cloud resources")
	gh, ghErr := clusterrepo.DialApp(d, k.cfg.Block, k.cfg.GitHubClient)
	if err := k.NewPlane(k.cfg.CloudAPI(token), subID, d.Params["appName"]).Delete(ctx); err != nil {
		return nil, err
	}
	deleted := "the cluster's Hetzner Cloud resources are deleted"
	if reading.State != contract.Absent {
		if err := k.releaseDedicatedServer(ctx, d, server, reading, readable); err != nil {
			return nil, err
		}
		deleted = "the cluster's Hetzner resources are deleted and the dedicated server is released"
	}
	if ghErr != nil {
		return nil, handler.Refuse("repository-left-in-place", "%s, but the cluster repository was left in place: %s", deleted, operator.RefusalDetail(ghErr))
	}
	return nil, k.deleteMarkedRepository(ctx, gh, d)
}

// releaseDedicatedServer cancels the vSwitch and releases the dedicated server, or refuses when the reading taken
// before the Cloud delete could not read the Robot resources.
func (k *Cluster) releaseDedicatedServer(ctx context.Context, d contract.Desired, t robotTarget, refused contract.Observation, ok bool) error {
	if !ok {
		return handler.Refuse("robot-left-in-place",
			"the cluster's Hetzner Cloud resources are deleted, but the dedicated server and its vSwitch were left in place: %s", refused.StatusDetail)
	}
	k.narrate(ctx, d, "releasing the dedicated server and cancelling its vSwitch")
	return t.release(ctx)
}

// deleteMarkedRepository reads the mark just before the delete, and keeps a repository without it untouched.
func (k *Cluster) deleteMarkedRepository(ctx context.Context, gh *clusterrepo.Access, d contract.Desired) error {
	repo, err := gh.Read(ctx)
	if err != nil {
		return err
	}
	switch {
	case !repo.Exists:
		return nil
	case !repo.Marked:
		k.narrate(ctx, d, fmt.Sprintf("keeping the repository %s, which does not carry this subscription's mark", gh.Repo.FullName()))
		return nil
	}
	k.narrate(ctx, d, "deleting the cluster repository")
	return gh.Delete(ctx)
}
