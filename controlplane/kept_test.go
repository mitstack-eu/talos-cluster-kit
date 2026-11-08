package controlplane_test

// A gateway built once nodes run mirrors the release's list and each image the newest node report names, taken from a
// report read after its location lost its gateway.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

var (
	// earlierTools is the tools image of the release before, which the report's CronJob still runs.
	earlierTools = report.ImageRef{
		Image:  "ghcr.io/mitstack-eu/talos-tools:v0.36.0@sha256:f45478093f69bbc3641a597844fcca5908a6779996fd7d5f83a679cae16369ba",
		Digest: "sha256:f45478093f69bbc3641a597844fcca5908a6779996fd7d5f83a679cae16369ba",
	}
	// nightlyExport is named by a CronJob's template alone, so no pod runs it between two runs.
	nightlyExport = report.ImageRef{
		Image:  "ghcr.io/acme/nightly-export:v2.1.0@sha256:2a3b62b53ddb9f167b63d22202a360811ba78df015021f704d01ee9abad4169c",
		Digest: "sha256:2a3b62b53ddb9f167b63d22202a360811ba78df015021f704d01ee9abad4169c",
	}
)

// mirrored is each reference the gateway's images.txt names.
func (h *harness) mirrored(gw controlplane.Server) []string {
	h.t.Helper()
	part := gatewayParts(h.t, h.cloud.UserData[gw.ID])[0]
	return strings.Fields(decodeCloudConfig(h.t, part).file(h.t, "/etc/mitstack-gateway/images.txt").Content)
}

func (h *harness) lose(loc string) {
	h.t.Helper()
	require.NoError(h.t, h.cloud.DeleteServer(context.Background(), h.gatewayIn(loc).ID))
}

func TestPlane_AnUpgradesReplacementsMirrorAnImageOfTheReleaseBeforeThatARunningPodNamesAndOneACronJobsTemplateNames(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	current := h.gatewayIn(controlplane.GatewayLocation).Labels[controlplane.KeyGatewayList]
	release := h.mirrored(h.gatewayIn(controlplane.GatewayLocation))
	h.relabelGateways("an-earlier-list")
	h.images = []report.ImageRef{earlierTools, nightlyExport}

	h.upgrade(func() {})

	for _, loc := range controlplane.Locations {
		gw := h.gatewayIn(loc)
		assert.ElementsMatch(t, append(release, earlierTools.Image, nightlyExport.Image), h.mirrored(gw),
			"the replacement in %s mirrors the release's list and both images the report names, so a worker added now pulls them", loc)
		assert.Equal(t, current, gw.Labels[controlplane.KeyGatewayList], "the replacement in %s's list label is the release list's digest", loc)
	}
}

func TestPlane_ALostGatewaysRebuildLeavesOutAnImageNoPodAndNoTemplateNamesAnyMore(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	release := h.mirrored(h.gatewayIn(controlplane.GatewayLocation))
	h.images = []report.ImageRef{earlierTools, nightlyExport}
	h.lose("hel1")
	h.built()
	h.images = []report.ImageRef{earlierTools}

	h.lose("nbg1")
	h.built()

	assert.ElementsMatch(t, append(release, earlierTools.Image), h.mirrored(h.gatewayIn("nbg1")),
		"the CronJob is gone from the newest report, so its image leaves the next gateway built")
	assert.Contains(t, h.mirrored(h.gatewayIn("hel1")), nightlyExport.Image, "a running gateway is never changed")
}

func TestPlane_ALostGatewaysRebuildKeepsTheImagesAnUpgradesReplacementKeeps(t *testing.T) {
	upgraded := newHarness(t, withReports)
	upgraded.built()
	upgraded.relabelGateways("an-earlier-list")
	upgraded.images = []report.ImageRef{earlierTools, nightlyExport}
	upgraded.upgrade(func() {})
	rebuilt := newHarness(t, withReports, sequential)
	rebuilt.built()
	rebuilt.images = upgraded.images

	rebuilt.lose("hel1")
	rebuilt.built()

	assert.ElementsMatch(t, upgraded.mirrored(upgraded.gatewayIn("hel1")), rebuilt.mirrored(rebuilt.gatewayIn("hel1")))
}

func TestPlane_CreatesNoGatewayWhileTheNewestNodeReportPredatesTheLocationsLossAndNamesItsReceivedAt(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.images = []report.ImageRef{earlierTools}
	h.reportsStopAt = h.now
	newest := h.reportReceived()
	h.lose("hel1")

	for range 30 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}

	assert.Empty(t, controlplane.ServersIn(h.byRole(controlplane.RoleGateway), "hel1"), "gateways in hel1")
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "),
		"the gateway in hel1 waits on a node report read after ")
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "),
		"the newest node report was read at "+newest.Format(time.RFC3339))
	h.reportsStopAt = time.Time{}
	h.built()
	assert.Contains(t, h.mirrored(h.gatewayIn("hel1")), earlierTools.Image, "the rebuild once a report arrives")
	assert.NotContains(t, h.network().Labels, controlplane.KeyGatewayGone+"hel1", "the record of the loss once hel1 has its gateway")
}

func TestPlane_CreatesNoGatewayFromANodeReportReceivedInTheSecondBeforeTheLossWasSeen(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.now = h.now.Truncate(reportEvery).Add(reportEvery + 500*time.Millisecond)
	h.reportsStopAt = h.now
	h.lose("hel1")

	for range 3 {
		require.NoError(t, h.converge())
	}

	assert.Empty(t, controlplane.ServersIn(h.byRole(controlplane.RoleGateway), "hel1"), "gateways in hel1")
}

func TestPlane_CreatesNoGatewayOnceNodesRunWhileTheClusterHasSentNoNodeReport(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.plane.ReadReport = func(context.Context, time.Time) (*report.Read, error) { return nil, nil }
	h.lose("hel1")

	for range 10 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
	}

	assert.Empty(t, controlplane.ServersIn(h.byRole(controlplane.RoleGateway), "hel1"), "gateways in hel1")
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "), "the cluster has sent no node report")
}

func TestPlane_LeavesOutAnUnresolvedTagAndAnImageOnARegistryTheNodesReachDirectlyAndNamesEachBesideWhatItKeeps(t *testing.T) {
	const direct = "registry.example.com/acme/api:v1@sha256:3bc801a33ea83df414e1aeb962a52412835be99db28668ec25de73fdd4733804"
	h := newHarness(t, withReports)
	h.built()
	h.images = []report.ImageRef{earlierTools, {Image: "ghcr.io/acme/cleanup:latest"},
		{Image: direct, Digest: "sha256:3bc801a33ea83df414e1aeb962a52412835be99db28668ec25de73fdd4733804"},
		{Image: "nginx:1.27", Digest: "sha256:4b5e57f6eb2f42b9039b3d1e13929295f231749c510cbe341cd68036d9af97e2"},
		{Image: h.mirrored(h.gatewayIn(controlplane.GatewayLocation))[0]}}
	h.lose("hel1")

	h.built()

	mirrored := h.mirrored(h.gatewayIn("hel1"))
	assert.Contains(t, mirrored, earlierTools.Image)
	assert.Contains(t, mirrored, "docker.io/library/nginx:1.27@sha256:4b5e57f6eb2f42b9039b3d1e13929295f231749c510cbe341cd68036d9af97e2",
		"a short name is mirrored under the Docker Hub name a node asks the mirror for")
	assert.NotContains(t, strings.Join(mirrored, "\n"), "ghcr.io/acme/cleanup")
	assert.NotContains(t, strings.Join(mirrored, "\n"), "registry.example.com")
	var event string
	for _, e := range h.events {
		if strings.HasPrefix(e, "the gateway in hel1 keeps ") {
			event = e
		}
	}
	require.NotEmpty(t, event, "no event names what the gateway in hel1 keeps: %v", h.events)
	assert.Contains(t, event, "keeps 2 images beyond this release's list, from the node report read at ")
	assert.Contains(t, event, earlierTools.Image)
	assert.Contains(t, event, "ghcr.io/acme/cleanup:latest, which names no digest and no pod's status resolves")
	assert.Contains(t, event, direct+", on registry.example.com, which the nodes reach directly")
}

func TestPlane_AnUpgradeReplacesNoGatewayWhileTheNewestNodeReportListsNoImages(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.relabelGateways("an-earlier-list")
	h.noImageList = true
	writes := h.cloud.Writes()

	h.plane.Upgrading = true
	err := h.ensure()

	assert.Equal(t, controlplane.ClassGatewayImagesUnread, refusalClass(t, err))
	assert.Contains(t, err.Error(), "the report lists no images")
	assert.Equal(t, writes, h.cloud.Writes(), "writes on a refused upgrade")
}

// threeHundredImages are 300 distinct images on a mirrored registry, each named by tag and resolved by a pod's status.
func threeHundredImages() (refs []report.ImageRef, want []string) {
	for i := range 300 {
		name := fmt.Sprintf("ghcr.io/example-org/service-%03d", i)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(name)))
		refs = append(refs, report.ImageRef{Image: name + ":v1.4.2", Digest: digest})
		want = append(want, name+":v1.4.2@"+digest)
	}
	return refs, want
}

func TestPlane_AGatewayKeeping300ImagesStaysInsideHetznersUserDataLimit(t *testing.T) {
	for name, opts := range map[string][]func(*harness){
		"a cluster":                         {withReports},
		"a cluster that hosts the platform": {hosting, withReports},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, opts...)
			h.built()
			t.Logf("the user data of the install's gateway with the etcd bootstrap is %d bytes",
				len(h.cloud.UserData[h.gatewayIn(controlplane.GatewayLocation).ID]))
			var want []string
			h.images, want = threeHundredImages()
			h.lose("hel1")

			h.built()

			gw := h.gatewayIn("hel1")
			t.Logf("the user data of a gateway keeping 300 images is %d bytes", len(h.cloud.UserData[gw.ID]))
			assert.LessOrEqual(t, len(h.cloud.UserData[gw.ID]), cloudtest.UserDataLimit, "Hetzner's user data limit")
			assert.Subset(t, h.mirrored(gw), want, "images.txt holds all 300 kept images")
		})
	}
}
