package controlplane_test

// The release a running cluster takes through its repository is the set its build applied, but what holds its keys.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// buildOnly are the manifests a build applies and no upgrade carries: the hosting set's keys, sealed keys, the volumes
// named for its nodes and OpenBao's init, which only an install's keys render, and the cluster's own Application,
// through which the carriage runs.
var buildOnly = []string{
	"mitstack-openbao-seal-key", "mitstack-sealed-secrets-key", "mitstack-openbao-deploy-key", "mitstack-cluster-repository",
	"mitstack-openbao-storage", "mitstack-openbao-init", "mitstack-cluster-application",
}

// gatewayCA is the gateway CA an install mints, for a plane that installed nothing.
func gatewayCA(t *testing.T) *gateway.CA {
	t.Helper()
	ca, err := gateway.NewCA(testStart, time.Hour, controlplane.GatewayAddrs())
	require.NoError(t, err)
	return &ca
}

func manifestNames(manifests []components.Manifest) []string {
	out := make([]string, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, m.Name)
	}
	return out
}

func TestRelease_CarriesWhatTheBuildAppliedButTheHostingKeysTheEnrolmentTokenAndTheClustersOwnApplication(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*harness)
	}{
		{"a cluster that hosts no platform", nil},
		{"a cluster that hosts the platform", []func(*harness){hosting}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.plane.GatewayCA = gatewayCA(t)
			built, err := h.plane.BuildSet(context.Background(), testStart, "enrolment-token")
			require.NoError(t, err)
			var carriedAtBuild []components.Manifest
			for _, m := range slices.Concat(built.Served, built.Inline) {
				if !slices.Contains(buildOnly, m.Name) {
					carriedAtBuild = append(carriedAtBuild, m)
				}
			}

			release, err := h.plane.Release()

			require.NoError(t, err)
			assert.ElementsMatch(t, manifestNames(carriedAtBuild), manifestNames(release.Components), "the manifests the release carries")
			assert.Subset(t, manifestNames(release.Components), []string{"mitstack-node-report", "mitstack-cluster-tokens", "mitstack-node-topology"}, "the plane's own manifests")
			want, err := rollout.Release{Components: carriedAtBuild}.Files(testutil.InitRepository)
			require.NoError(t, err)
			got, err := release.Files(testutil.InitRepository)
			require.NoError(t, err)
			assert.ElementsMatch(t, want, got, "the files the release commits, against those of the manifests the build applied")
			for _, f := range got {
				for _, doc := range testutil.Documents(t, f.Bytes) {
					if doc["kind"] == "Secret" {
						assert.Nil(t, doc["data"], "the data of Secret %v in %s", doc["metadata"], f.Path)
						assert.Nil(t, doc["stringData"], "the string data of Secret %v in %s", doc["metadata"], f.Path)
					}
					assert.NotEqual(t, "Job", doc["kind"], "a Job %v in %s", doc["metadata"], f.Path)
				}
			}
		})
	}
}

func TestRelease_RefusesAPlaneThatCannotRenderTheBuildsOwnManifests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plane func(h *harness) controlplane.Plane
		want  string
	}{
		{"a plane without the gateway CA", func(h *harness) controlplane.Plane { return h.plane }, "gateway CA"},
		{"a plane without the cluster repository", func(h *harness) controlplane.Plane {
			p := h.plane
			p.GatewayCA, p.RepoURL = gatewayCA(h.t), ""
			return p
		}, "repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			_, err := tc.plane(h).Release()

			assert.ErrorContains(t, err, tc.want, "the refusal")
		})
	}
}
