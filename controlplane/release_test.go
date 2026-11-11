package controlplane_test

// The release a running cluster takes through its repository is the set its build applied, but what holds its keys.

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
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

func manifestNames(manifests []components.Manifest) []string {
	out := make([]string, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, m.Name)
	}
	return out
}

// withLoadBalancers takes the plane as far as its two load balancers, which hold their addresses from their creation.
func (h *harness) withLoadBalancers() {
	h.t.Helper()
	for range 20 {
		if len(h.cloud.LBSpecs) == 2 {
			return
		}
		require.NoError(h.t, h.ensure())
	}
	require.Len(h.t, h.cloud.LBSpecs, 2, "the load balancers the plane created")
}

// publishedAddressArg is the argument under which Traefik writes one address into the status of every Ingress it serves.
const publishedAddressArg = "--providers.kubernetesingress.ingressendpoint.ip="

// releaseOf is the release a hosting plane that keeps its gateway CA renders from what Hetzner holds now.
func (h *harness) releaseOf(plane controlplane.Plane) rollout.Release {
	h.t.Helper()
	release, err := plane.Release(h.read())
	require.NoError(h.t, err)
	return release
}

func traefikArgs(t *testing.T, release rollout.Release) []string {
	t.Helper()
	daemonSets := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "traefik", release.Components).Bytes), "DaemonSet")
	require.Len(t, daemonSets, 1, "Traefik's DaemonSets")
	var args []string
	for _, arg := range testutil.Field(t, daemonSets[0], "spec", "template", "spec", "containers", "0", "args").([]any) {
		args = append(args, arg.(string))
	}
	return args
}

// A fresh cluster's first release and every later one publish the address the plane outputs, so an app slice's Ingress
// holds it on its first sync, in whatever namespace kube-app-deploy puts it, and a replaced load balancer's Ingresses
// take the new address.
func TestRelease_TraefikPublishesTheAddressThePlaneOutputsOnEveryIngressAndTheNewOneOnceTheLoadBalancerIsReplaced(t *testing.T) {
	const replacement = "198.51.100.251"
	h := newHarness(t, hosting)
	h.withLoadBalancers()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	built := h.loadBalancer(controlplane.RoleIngress)
	outputAddress := func() string {
		outputs, _ := plane.Outputs(h.read())
		var byLocation map[string]string
		require.NoError(t, json.Unmarshal([]byte(outputs["publicIpByLocation"]), &byLocation))
		require.Len(t, byLocation, 1, "the locations the plane outputs an address for")
		return byLocation[controlplane.GatewayLocation]
	}

	first, firstOutput := h.releaseOf(plane), outputAddress()
	h.cloud.LoadBalancers[built.ID].PublicIPv4 = replacement
	second, secondOutput := h.releaseOf(plane), outputAddress()

	require.NotEqual(t, replacement, built.PublicIPv4, "the address the load balancer was built with")
	assert.Equal(t, built.PublicIPv4, firstOutput, "the address the plane outputs for the load balancer it built")
	assert.Equal(t, []string{publishedAddressArg + firstOutput}, testutil.IngressEndpointArgs(t, testutil.Manifest(t, "traefik", first.Components)),
		"what Traefik writes into an Ingress's status on the cluster's first release")
	assert.Equal(t, replacement, secondOutput, "the address the plane outputs once the load balancer is replaced")
	assert.Equal(t, []string{publishedAddressArg + replacement}, testutil.IngressEndpointArgs(t, testutil.Manifest(t, "traefik", second.Components)),
		"what Traefik writes into an Ingress's status once the load balancer is replaced")
	for _, m := range second.Components {
		assert.NotContains(t, string(m.Bytes), built.PublicIPv4, "the replaced load balancer's address in %s", m.Name)
	}
	for _, arg := range traefikArgs(t, second) {
		assert.False(t, strings.HasPrefix(arg, "--providers.kubernetesingress.namespaces"), "Traefik serves the Ingresses of some namespaces alone: %s", arg)
	}
}

func TestRelease_CarriesNoTraefikWithoutAnAddressAndNamesTheIngressLoadBalancerThatHoldsNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state func(h *harness) controlplane.State
	}{
		{"a plane whose state holds no ingress load balancer yet", func(h *harness) controlplane.State { return h.read() }},
		{"an ingress load balancer that holds no public address", func(h *harness) controlplane.State {
			h.withLoadBalancers()
			h.cloud.LoadBalancers[h.loadBalancer(controlplane.RoleIngress).ID].PublicIPv4 = ""
			return h.read()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, hosting)
			plane, err := h.plane.WithGatewayCA()
			require.NoError(t, err)

			release, err := plane.Release(tc.state(h))

			require.ErrorIs(t, err, controlplane.ErrNoIngressAddress)
			assert.ErrorContains(t, err, "no ingress load balancer in "+controlplane.GatewayLocation+" holds a public address", "the load balancer the refusal names")
			assert.Empty(t, release.Components, "the components a block could commit")
		})
	}
}

func TestRelease_AClusterThatHostsNoPlatformCarriesNoTraefikAndNeedsNoIngressAddress(t *testing.T) {
	h := newHarness(t)
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	release, err := plane.Release(h.read())

	require.NoError(t, err)
	assert.NotContains(t, manifestNames(release.Components), "traefik")
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
			h.withLoadBalancers()
			plane, err := h.plane.WithGatewayCA()
			require.NoError(t, err)
			built, err := plane.BuildSet(context.Background(), testStart, "enrolment-token")
			require.NoError(t, err)
			built, err = built.PublishingIngress([]netip.Addr{netip.MustParseAddr(h.loadBalancer(controlplane.RoleIngress).PublicIPv4)})
			require.NoError(t, err)
			var carriedAtBuild []components.Manifest
			for _, m := range slices.Concat(built.Served, built.Inline) {
				if !slices.Contains(buildOnly, m.Name) {
					carriedAtBuild = append(carriedAtBuild, m)
				}
			}

			release, err := plane.Release(h.read())

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
			p, err := h.plane.WithGatewayCA()
			require.NoError(h.t, err)
			p.RepoURL = ""
			return p
		}, "repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			_, err := tc.plane(h).Release(h.read())

			assert.ErrorContains(t, err, tc.want, "the refusal")
		})
	}
}
