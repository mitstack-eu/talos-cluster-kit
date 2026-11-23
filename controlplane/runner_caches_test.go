package controlplane_test

// A plane with runners carries the shared package caches, reads the cluster unhealthy naming a cache that stops
// answering, and an upgrade that turns the runner services off carries the caches away and keeps every node.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const cachesApplication = rollout.ApplicationPrefix + components.RunnerCachesName

// appliedCache is a cache's Deployment as Argo CD's health of it reaches the node report.
type appliedCache struct{ name, health, message string }

// cacheReports sends the node report every five minutes with applications as Argo CD reads them: each Application by
// name, holding the caches it lists.
func cacheReports(applications map[string][]appliedCache) func(*harness) {
	return func(h *harness) {
		h.plane.ReadReport = func(_ context.Context, since time.Time) (*report.Read, error) {
			received := h.reportReceived()
			if !received.After(since) {
				return nil, nil
			}
			apps := []map[string]any{}
			for name, caches := range applications {
				resources := []map[string]any{}
				for _, c := range caches {
					resources = append(resources, map[string]any{"group": "apps", "kind": "Deployment", "namespace": components.RunnersNamespace,
						"name": c.name, "sync": "Synced", "health": c.health, "message": c.message})
				}
				resources = append(resources, map[string]any{"group": "", "kind": "Service", "namespace": components.RunnersNamespace,
					"name": "go-cache", "sync": "Synced", "health": "Healthy"})
				apps = append(apps, map[string]any{"name": name, "health": "Healthy", "sync": "Synced", "resources": resources})
			}
			data, err := json.Marshal(map[string]any{"images": []report.ImageRef{}, "argocd": map[string]any{"applications": apps}})
			return &report.Read{Body: data, At: received}, err
		}
	}
}

func healthyCaches() []appliedCache {
	return []appliedCache{{"go-cache", "Healthy", ""}, {"npm-cache", "Healthy", ""}, {"image-cache", "Healthy", ""}}
}

func TestPlane_ReadsTheClusterUnhealthyNamingEachPackageCacheArgoCDReadsOtherThanHealthy(t *testing.T) {
	waiting := "Waiting for rollout to finish: 0 of 1 updated replicas are available..."
	for _, tc := range []struct {
		name    string
		caches  []appliedCache
		down    []string
		healthy []string
	}{
		{"every cache healthy", healthyCaches(), nil, nil},
		{"the image cache whose pod stopped answering its probe",
			[]appliedCache{{"go-cache", "Healthy", ""}, {"npm-cache", "Healthy", ""}, {"image-cache", "Progressing", waiting}},
			[]string{"image-cache", "Progressing", waiting}, []string{"go-cache", "npm-cache"}},
		{"the Go and npm caches past their progress deadline",
			[]appliedCache{{"go-cache", "Degraded", "ProgressDeadlineExceeded"}, {"npm-cache", "Degraded", ""}, {"image-cache", "Healthy", ""}},
			[]string{"go-cache", "npm-cache", "Degraded", "ProgressDeadlineExceeded"}, []string{"image-cache"}},
		{"a cache whose Deployment the Application no longer lists",
			[]appliedCache{{"npm-cache", "Healthy", ""}, {"image-cache", "Healthy", ""}},
			[]string{"go-cache"}, []string{"npm-cache", "image-cache"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports, runners)
			h.built()
			cacheReports(map[string][]appliedCache{cachesApplication: tc.caches})(h)

			cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())

			require.NoError(t, err)
			if tc.down == nil {
				assert.Empty(t, cause, "the cause, with %s: %s", tc.name, detail)
				return
			}
			assert.Equal(t, "a runner package cache does not serve, so its jobs fetch past it from the internet, and each npm install fails while it is the npm cache; the detail names the cache, so read its pod's log", cause,
				"the platform's catalogue lists the cause word for word, and it holds whichever cache is down")
			for _, want := range append(tc.down, h.reportReceived().Format(time.RFC3339)) {
				assert.Contains(t, detail, want, "the detail names the cache, its health and the report that read it")
			}
			for _, serving := range tc.healthy {
				assert.NotContains(t, detail, serving, "the detail names a cache that serves")
			}
		})
	}
}

func TestPlane_ReadsNoCacheCauseFromAReportThatCannotJudgeTheCachesOrAPlaneWithoutRunners(t *testing.T) {
	down := []appliedCache{{"go-cache", "Degraded", "ProgressDeadlineExceeded"}}
	for _, tc := range []struct {
		name string
		opts []func(*harness)
	}{
		{"a report from before Argo CD applied the caches", []func(*harness){runners, cacheReports(map[string][]appliedCache{"component-other": healthyCaches()})}},
		{"a plane without runners whose report still lists a cache that is down", []func(*harness){cacheReports(map[string][]appliedCache{cachesApplication: down})}},
		{"a plane whose block reads no node report", []func(*harness){runners, func(h *harness) { h.plane.ReadReport = nil }}},
		{"a report from before Argo CD judged a cache's health", []func(*harness){runners, cacheReports(map[string][]appliedCache{
			cachesApplication: {{"go-cache", "", ""}, {"npm-cache", "Healthy", ""}, {"image-cache", "Healthy", ""}}})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, append([]func(*harness){withReports}, tc.opts...)...)
			h.built()

			cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())

			require.NoError(t, err)
			assert.Empty(t, cause, "the cause, with %s: %s", tc.name, detail)
		})
	}
}

func TestPlane_ReadsNoCacheCauseFromAReportWhoseApplicationsCarryNoHealth(t *testing.T) {
	h := newHarness(t, withReports, runners)
	h.built()
	h.plane.ReadReport = func(context.Context, time.Time) (*report.Read, error) {
		body := fmt.Sprintf(`{"images": [], "argocd": {"applications": [{"name": %q, "resources": [
			{"group": "apps", "kind": "Deployment", "namespace": "mitstack-runners", "name": "go-cache", "sync": "Synced"}]}]}}`, cachesApplication)
		return &report.Read{Body: []byte(body), At: h.reportReceived()}, nil
	}

	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())

	require.NoError(t, err)
	assert.Empty(t, cause, "the cause from a report whose Argo CD keeps health off the Application: %s", detail)
}

func TestRelease_APlaneWithRunnersCarriesThePackageCachesOnTheReleasesToolsImage(t *testing.T) {
	h := newHarness(t, runners)
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	release, err := plane.Release(h.read())

	require.NoError(t, err)
	files, err := release.Files(testutil.InitRepository)
	require.NoError(t, err)
	var carried []string
	for _, f := range files {
		if !strings.Contains(f.Path, components.RunnerCachesName) {
			continue
		}
		for _, line := range strings.Split(string(f.Bytes), "\n") {
			if image, ok := strings.CutPrefix(strings.TrimSpace(line), "image: "); ok {
				carried = append(carried, strings.Trim(image, `"`))
			}
		}
	}
	assert.Contains(t, carried, tools.Image, "the image whose package-cache the Go and npm caches run")
	assert.Contains(t, carried, components.ImageCacheImage, "the registry the image cache runs")
}

func TestPlane_TheGatewaysMirrorTheImageCachesRegistryOnlyForAPlaneWithRunners(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	without := h.mirrored(h.gatewayIn(controlplane.GatewayLocation))
	h.plane.Runners = runnerInputs()

	h.upgrade(func() {})

	assert.NotContains(t, without, components.ImageCacheImage, "the mirror of a plane without runners")
	for _, loc := range controlplane.Locations {
		mirrored := h.mirrored(h.gatewayIn(loc))
		assert.Contains(t, mirrored, components.ImageCacheImage, "the replacement in %s mirrors the registry the image cache's pod starts", loc)
		assert.Contains(t, mirrored, tools.Image, "the replacement in %s mirrors the image the Go and npm caches' pods start", loc)
	}
}

func TestPlane_AnUpgradeThatTurnsTheRunnerServicesOffCarriesThePackageCachesAwayAndKeepsEveryNode(t *testing.T) {
	h := newHarness(t, withReports, runners)
	h.built()
	nodes := controlplane.IDs(h.byRole(controlplane.RoleNode), controlplane.ServerID)
	h.plane.Runners = nil
	cacheReports(map[string][]appliedCache{cachesApplication: {{"go-cache", "Missing", ""}}})(h)

	h.upgrade(func() {
		require.Equal(t, nodes, controlplane.IDs(h.byRole(controlplane.RoleNode), controlplane.ServerID), "the nodes during the upgrade")
	})

	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	release, err := plane.Release(h.read())
	require.NoError(t, err)
	assert.NotContains(t, manifestNames(release.Components), components.RunnerCachesName, "the release once the runner services are off")
	files, err := release.Files(testutil.InitRepository)
	require.NoError(t, err)
	for _, f := range files {
		assert.NotContains(t, f.Path, components.RunnerCachesName, "the cluster repository holds no caches' file, so Argo CD prunes them")
	}
	for _, loc := range controlplane.Locations {
		assert.NotContains(t, h.mirrored(h.gatewayIn(loc)), components.ImageCacheImage, "the replacement in %s", loc)
	}
	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Empty(t, cause, "the cause while Argo CD prunes the caches: %s", detail)
	assert.Empty(t, h.plane.Problems(context.Background(), h.read()), "the problems once the upgrade is done")
}
