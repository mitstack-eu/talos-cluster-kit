package controlplane

// A gateway built once nodes run mirrors the release's list and every image the newest node report names, taken from a
// report read after its location lost its gateway, so a gateway upgrade or rebuild strands no image a pod still uses.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// KeyGatewayGone on the network, followed by a location, holds when the plane first found that location without its
// gateway, as Unix seconds.
const KeyGatewayGone = "mitstack.dev/gateway-gone-"

// ClassGatewayImagesUnread refuses an upgrade's replacement of a gateway while no node report lists the images the
// cluster runs.
const ClassGatewayImagesUnread = "gateway-images-unread"

// keeping is what a node report lets a new gateway keep beyond the release's list, and each reference it leaves out
// with why.
type keeping struct {
	report  report.Read
	kept    []components.Image
	leftOut []string
}

// newestImages reads the image list of the newest node report read after since; why names what keeps a gateway from
// keeping it, "" when nothing does.
func (p Plane) newestImages(ctx context.Context, since time.Time) (k keeping, why string, err error) {
	if p.ReadReport == nil {
		return keeping{}, "the block reads no node report", nil
	}
	r, err := p.ReadReport(ctx, since)
	if err != nil {
		return keeping{}, "", err
	}
	if r == nil || r.At.Before(since) {
		return p.newestReportWait(ctx)
	}
	refs, err := report.Images(r.Body)
	if err != nil {
		return keeping{}, fmt.Sprintf("the newest node report, read at %s: %v", stampOf(r.At), err), nil
	}
	k = keeping{report: *r}
	for _, ref := range refs {
		image, why := keptImage(ref)
		if why != "" {
			k.leftOut = append(k.leftOut, why)
		} else if !slices.Contains(k.kept, image) {
			k.kept = append(k.kept, image)
		}
	}
	return k, "", nil
}

// keptImage is the image a node pulls for ref, as the mirror holds it, or why the mirror cannot keep it.
func keptImage(ref report.ImageRef) (components.Image, string) {
	name, tag, digest := splitReference(ref.Image)
	if digest == "" {
		digest = ref.Digest
	}
	if digest == "" {
		return components.Image{}, ref.Image + ", which names no digest and no pod's status resolves"
	}
	switch {
	case tag != "":
	case ref.Image == name:
		tag = "latest"
	default:
		// A reference by digest alone is pulled by its digest, so any tag holds it in the mirror.
		tag = strings.Replace(digest, ":", "-", 1)
	}
	image, err := components.ParseImage(normalized(name) + ":" + tag + "@" + digest)
	if err != nil {
		return components.Image{}, ref.Image + ", which is not a reference the mirror can hold"
	}
	if !slices.Contains(components.MirroredRegistries, image.Registry()) {
		return components.Image{}, fmt.Sprintf("%s, on %s, which the nodes reach directly", ref.Image, image.Registry())
	}
	return image, ""
}

// splitReference cuts a reference into its name, its tag and its own digest, each empty when the reference names none.
func splitReference(ref string) (name, tag, digest string) {
	name, digest, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	return name, tag, digest
}

// normalized names the registry a node asks the mirror for, as containerd completes a short name to Docker Hub's.
func normalized(name string) string {
	host, rest, found := strings.Cut(name, "/")
	if !found || !strings.ContainsAny(host, ".:") && host != "localhost" {
		host, rest = "docker.io", name
	}
	if host == "docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	return host + "/" + rest
}

// event names the report k was read from, each image list keeps beyond the release's own, and each k leaves out.
// newestReportWait names the newest node report of all, which is too old to keep from.
func (p Plane) newestReportWait(ctx context.Context) (keeping, string, error) {
	newest, err := p.ReadReport(ctx, time.Time{})
	switch {
	case err != nil:
		return keeping{}, "", err
	case newest == nil:
		return keeping{}, "the cluster has sent no node report", nil
	default:
		return keeping{}, "the newest node report was read at " + stampOf(newest.At), nil
	}
}

func (k keeping) event(loc string, list gateway.List) string {
	kept := make([]string, 0, len(k.kept))
	for _, image := range list.Kept() {
		kept = append(kept, image.Ref())
	}
	event := fmt.Sprintf("the gateway in %s keeps %d images beyond this release's list, from the node report read at %s",
		loc, len(kept), stampOf(k.report.At))
	if len(kept) > 0 {
		event += ": " + strings.Join(kept, ", ")
	}
	if len(k.leftOut) > 0 {
		event += "; it leaves out " + strings.Join(k.leftOut, "; ")
	}
	return event
}

func gatewayGoneAt(network Network, loc string) (time.Time, bool) {
	unix, err := strconv.ParseInt(network.Labels[KeyGatewayGone+loc], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unix, 0).UTC(), true
}

func (p Plane) setGatewayGone(network Network, loc string, at *time.Time) Step {
	what := "record when the location " + loc + " lost its gateway, so its next gateway keeps what a node report since names"
	if at == nil {
		what = "take the record of the location " + loc + " losing its gateway off the network, since it has one again"
	}
	return Step{what, func(ctx context.Context) error {
		labels := maps.Clone(network.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		delete(labels, KeyGatewayGone+loc)
		if at != nil {
			// Rounded up, so a report from earlier in the second the loss was seen never reads as after it.
			labels[KeyGatewayGone+loc] = strconv.FormatInt(at.Add(time.Second-1).Unix(), 10)
		}
		return p.API.SetNetworkLabels(ctx, network.ID, labels)
	}}
}

// rebuildStep builds the gateway of a location that lost it once nodes run, from a node report read after the loss;
// it takes no step while it waits on one, and rebuildWait names why.
func (p Plane) rebuildStep(ctx context.Context, st State, s site, loc string, sibling Server) (Step, bool, error) {
	if _, recorded := gatewayGoneAt(s.network, loc); !recorded {
		now := p.now()
		return p.setGatewayGone(s.network, loc, &now), true, nil
	}
	k, why, err := p.rebuildWait(ctx, s.network, loc)
	if err != nil || why != "" {
		return Step{}, false, err
	}
	return Step{"create the gateway in " + loc + " from this release's list and the images the node report read at " +
		stampOf(k.report.At) + " names", func(ctx context.Context) error {
		return p.createGateway(ctx, st, s, loc, sibling, &k)
	}}, true, nil
}

// rebuildWait reads what a location's next gateway keeps; why names the node report it waits on, "" when it waits on none.
func (p Plane) rebuildWait(ctx context.Context, network Network, loc string) (keeping, string, error) {
	since, recorded := gatewayGoneAt(network, loc)
	if !recorded {
		return keeping{}, "", nil
	}
	k, why, err := p.newestImages(ctx, since)
	if err != nil || why == "" {
		return k, "", err
	}
	return keeping{}, fmt.Sprintf("the gateway in %s waits on a node report read after %s, when the location lost it, to keep each image the cluster still runs: %s",
		loc, stampOf(since), why), nil
}

// keptListStep refuses an upgrade's replacement of a gateway, before any gateway is deleted, while the newest node
// report lists no image a replacement could keep.
func (p Plane) keptListStep(ctx context.Context, st State, s site) (Step, bool, error) {
	if st.lacksGateway() {
		return Step{}, false, nil
	}
	r, err := p.replacement(st, s.network)
	if err != nil || !r.active || len(r.stale) == 0 {
		return Step{}, false, err
	}
	_, why, err := p.newestImages(ctx, time.Time{})
	if err != nil || why == "" {
		return Step{}, false, err
	}
	return Step{}, false, refuse(ClassGatewayImagesUnread,
		"the upgrade replaces no gateway, since each replacement keeps the images the cluster still runs and %s; retry the upgrade once the node report lists them", why)
}

func (st State) lacksGateway() bool {
	for _, loc := range Locations {
		if _, ok := st.GatewayIn(loc); !ok {
			return true
		}
	}
	return false
}

func stampOf(t time.Time) string { return t.UTC().Format(time.RFC3339) }
