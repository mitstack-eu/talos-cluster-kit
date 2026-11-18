package robotcluster

import (
	"context"
	"fmt"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

// Delete stops the worker's install, deletes every labelled Cloud resource, the snapshot that holds every key of the
// cluster among them, cancels the vSwitch and releases the dedicated server without cancelling it, and deletes the
// repository. Without a usable Robot login or GitHub connection it still deletes the Cloud resources, and then refuses,
// naming what it left in place.
func (k *Cluster) Delete(ctx context.Context, d contract.Desired) (map[string]string, error) {
	token, err := operator.HetznerToken(d)
	if err != nil {
		return nil, err
	}
	subID, err := subscriptionID(d)
	if err != nil {
		return nil, err
	}
	k.narrate(ctx, d, "stopping the dedicated server's install, if one runs")
	if err := k.StopJob(ctx, subID); err != nil {
		return nil, err
	}
	k.narrate(ctx, d, "deleting the cluster's Hetzner Cloud resources")
	gh, ghErr := clusterrepo.DialApp(d, k.cfg.Block, k.cfg.GitHubClient)
	if err := k.NewPlane(k.cfg.CloudAPI(token), subID, d.Params["appName"]).Delete(ctx); err != nil {
		return nil, err
	}
	t, refused, robotOK := k.robotReader(d)
	if robotOK {
		k.narrate(ctx, d, "releasing the dedicated server and cancelling its vSwitch")
		if err := t.release(ctx); err != nil {
			return nil, err
		}
	}
	switch {
	case !robotOK:
		return nil, handler.Refuse("robot-left-in-place",
			"the cluster's Hetzner Cloud resources are deleted, but the dedicated server and its vSwitch were left in place: %s", refused.StatusDetail)
	case ghErr != nil:
		return nil, handler.Refuse("repository-left-in-place",
			"the cluster's Hetzner resources are deleted and the dedicated server is released, but the cluster repository was left in place: %s",
			operator.RefusalDetail(ghErr))
	}
	return nil, k.deleteMarkedRepository(ctx, gh, d)
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
