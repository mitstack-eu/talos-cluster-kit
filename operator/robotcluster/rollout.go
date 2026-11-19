package robotcluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/helpers/safe"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// MaxTreeAnswerBytes bounds the listing of the repository's files, which names each file and no content.
const MaxTreeAnswerBytes = 4 << 20

// carryRelease commits once every gateway serves the release's image list, so no manifest names an unmirrored image,
// and answers the carriage it judged.
func (c convergence) carryRelease(ctx context.Context) (carriage, error) {
	due, err := c.releaseDue(ctx)
	if err != nil || len(due.changes) == 0 {
		return due, err
	}
	return due, c.req.gh.rollOut(ctx, due)
}

// carriage lands a release on main's head, naming each path it writes as true and each it takes out as false.
type carriage struct {
	release rollout.Release
	head    string
	changes map[string]bool
}

// releaseDue is the release's carriage, empty until every gateway serves its image list, which each kit release changes.
func (c convergence) releaseDue(ctx context.Context) (carriage, error) {
	serves, err := c.plane.GatewaysServeRelease(ctx, c.state)
	if err != nil || !serves {
		return carriage{}, err
	}
	release, err := ReleaseFor(c.plane, c.state, c.req.params.hosting, c.req.gh.Repo)
	if errors.Is(err, controlplane.ErrNoIngressAddress) {
		// The plane's next step builds a missing load balancer, and one that stands without an address fails the observe.
		return carriage{}, nil
	}
	if err != nil {
		return carriage{}, err
	}
	return c.req.gh.releaseChanges(ctx, release)
}

// carriageWaits is whether a release main does not hold as stated waits on the gateways: the cluster awaits it no longer,
// yet not every gateway serves it, so no subscribe commits it.
func carriageWaits(ctx context.Context, p controlplane.Plane, st controlplane.State) (bool, error) {
	awaits, err := p.GatewaysAwaitRelease(st)
	if err != nil || awaits {
		return false, err
	}
	serves, err := p.GatewaysServeRelease(ctx, st)
	return err == nil && !serves, err
}

// ReleaseFor renders p's release for repo from st, so Ensure and Observe judge one rendering whatever keys p holds.
func ReleaseFor(p controlplane.Plane, st controlplane.State, hosting bool, repo clusterrepo.Repo) (rollout.Release, error) {
	p.RepoURL, p.Hosting = repo.URL(), nil
	if hosting {
		p.Hosting = &controlplane.Hosting{Repository: repo.FullName()}
	}
	return p.Release(st)
}

// rollOut lands the carriage in one commit for Argo CD to apply, since Talos never re-applies a manifest.
func (g *githubAccess) rollOut(ctx context.Context, c carriage) error {
	files, err := c.release.Files(g.Repo.FullName())
	if err != nil {
		return err
	}
	changes := map[string]*string{}
	for _, f := range files {
		if c.changes[f.Path] {
			content := string(f.Bytes)
			changes[f.Path] = &content
		}
	}
	for path, written := range c.changes {
		if !written {
			changes[path] = nil
		}
	}
	message := "Carry this release's component set to the cluster\n\n" + observe.GitHubSubscriptionTrailer(g.SubscriptionID())
	if _, err := g.Client.CommitChanges(ctx, g.Repo.FullName(), c.head, message, changes); err != nil {
		return fmt.Errorf("committing the release's component set to %s: %w", g.Repo.FullName(), err)
	}
	return nil
}

// releaseChanges is the carriage of the release onto main: each file main lacks as stated, and each one it retires.
func (g *githubAccess) releaseChanges(ctx context.Context, release rollout.Release) (carriage, error) {
	stated, err := g.k.RenderingOf(release, g.Repo.FullName())
	if err != nil {
		return carriage{}, err
	}
	head, err := g.Client.HeadCommit(ctx, g.Repo.FullName())
	if err != nil {
		return carriage{}, err
	}
	if head == "" {
		return carriage{}, fmt.Errorf("the cluster repository %s has no main to carry the release to", g.Repo.FullName())
	}
	held, err := g.heldBlobs(ctx, head)
	if err != nil {
		return carriage{}, err
	}
	c := carriage{release: release, head: head, changes: map[string]bool{}}
	for path, blob := range stated.blobs {
		if held[path] != blob {
			c.changes[path] = true
		}
	}
	retired, err := stated.retiredFrom(release, slices.Sorted(maps.Keys(held)))
	for _, path := range retired {
		c.changes[path] = false
	}
	return c, err
}

// heldBlobs is main's blob SHAs at head, listed once per access, since a validate compares them for the read-back and
// for the step left.
func (g *githubAccess) heldBlobs(ctx context.Context, head string) (map[string]string, error) {
	if held, ok := g.held[head]; ok {
		return held, nil
	}
	held, err := g.listBlobs(ctx, head)
	if err != nil {
		return nil, err
	}
	if g.held == nil {
		g.held = map[string]map[string]string{}
	}
	g.held[head] = held
	return held, nil
}

// listBlobs reads the blob SHAs at head in one listing, since the contents API omits a file past one megabyte.
func (g *githubAccess) listBlobs(ctx context.Context, head string) (map[string]string, error) {
	tree, err := g.Client.CommitTree(ctx, g.Repo.FullName(), head)
	if err != nil {
		return nil, err
	}
	path := "/repos/" + g.Repo.FullName() + "/git/trees/" + url.PathEscape(tree) + "?recursive=1"
	status, raw, err := g.CallLimited(ctx, http.MethodGet, path, nil, MaxTreeAnswerBytes+1)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxTreeAnswerBytes {
		return nil, fmt.Errorf("the file listing of %s passes %d bytes, so the files it holds cannot all be compared", g.Repo.FullName(), MaxTreeAnswerBytes)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("listing the files of %s: github returned %d: %s", g.Repo.FullName(), status, safe.LogSafe(string(raw), g.Client.Token))
	}
	var listed struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path, Type, SHA string
		} `json:"tree"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("reading the file listing of %s: %w", g.Repo.FullName(), err)
	}
	if listed.Truncated {
		return nil, fmt.Errorf("github truncated the file listing of %s, so the files it holds cannot all be compared", g.Repo.FullName())
	}
	held := map[string]string{}
	for _, e := range listed.Tree {
		if e.Type == "blob" {
			held[e.Path] = e.SHA
		}
	}
	return held, nil
}

// readBack reads a release main holds as stated back from the newest node report.
func (g *githubAccess) readBack(ctx context.Context, d contract.Desired, release rollout.Release) (applied, problem string, err error) {
	reports, err := g.k.cfg.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return "", fmt.Sprintf("the node reports cannot be read to read the component set back: %v", err), nil
	}
	newest := operator.NewestReport(reports, time.Time{})
	if newest == nil {
		return "", "the cluster has sent no node report to read the component set back from", nil
	}
	stated, err := g.k.RenderingOf(release, g.Repo.FullName())
	if err != nil {
		return "", "", err
	}
	reading, err := stated.readingOf(release, newest.Body)
	if err != nil {
		return "", "", err
	}
	if reading.Problem != "" {
		return "", reading.Problem, nil
	}
	return fmt.Sprintf("Argo CD's component Applications hold each of the release's %d objects", len(reading.Applied)), "", nil
}
