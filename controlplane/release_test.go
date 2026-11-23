package controlplane_test

// The release a running cluster takes through its repository is the set its build applied, but what holds its keys and the database operators' zone spread.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/internal/redundanttest"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
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

// A fresh cluster's first release and a replaced load balancer's next one name the address for an Ingress in any namespace.
func TestRelease_TraefikPublishesTheAddressThePlaneOutputsOnEveryIngressAndTheNewOneOnceTheLoadBalancerIsReplaced(t *testing.T) {
	const replacement = "192.0.2.77"
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
	assert.Equal(t, []string{testutil.PublishedAddressArg + firstOutput}, testutil.IngressEndpointArgs(t, testutil.Manifest(t, "traefik", first.Components)),
		"what Traefik writes into an Ingress's status on the cluster's first release")
	assert.Equal(t, replacement, secondOutput, "the address the plane outputs once the load balancer is replaced")
	assert.Equal(t, []string{testutil.PublishedAddressArg + replacement}, testutil.IngressEndpointArgs(t, testutil.Manifest(t, "traefik", second.Components)),
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
			assert.ErrorContains(t, err, `holds "" as the public address of the ingress load balancer in `+controlplane.GatewayLocation, "the load balancer and the value the refusal names")
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

func telemetry(h *harness) { h.plane.Telemetry = testutil.TelemetryExport() }

func TestRelease_CarriesWhatTheBuildAppliedButTheHostingKeysTheEnrolmentTokenAndTheClustersOwnApplication(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*harness)
		// releaseOnly is what the build cannot apply, so the release alone carries it.
		releaseOnly []string
	}{
		{"a cluster that hosts no platform", nil, []string{"coredns"}},
		{"a cluster that hosts the platform", []func(*harness){hosting}, []string{"coredns"}},
		{"a cluster that hosts the platform and ships its telemetry", []func(*harness){hosting, telemetry}, []string{"mitstack-telemetry", "coredns"}},
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
			spreadAtBuild, err := components.Set{Served: carriedAtBuild}.DatabaseOperatorsAcrossZones(controlplane.Locations)
			require.NoError(t, err)
			carriedAtBuild = spreadAtBuild.Served

			release, err := plane.Release(h.read())

			require.NoError(t, err)
			for _, name := range tc.releaseOnly {
				carriedAtBuild = append(carriedAtBuild, testutil.Manifest(t, name, release.Components))
			}
			assert.ElementsMatch(t, manifestNames(carriedAtBuild), manifestNames(release.Components), "the manifests the release carries")
			assert.Subset(t, manifestNames(release.Components), []string{"mitstack-node-report", "mitstack-cluster-tokens", "mitstack-node-topology"}, "the plane's own manifests")
			assert.Equal(t, tools.Image, release.ToolsImage, "the tools image each component's handover Job runs, which this kit release published")
			atBuild := release
			atBuild.Components = carriedAtBuild
			want, err := atBuild.Files(testutil.InitRepository)
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
					if doc["kind"] == "Job" {
						hook, name := testutil.Field(t, doc, "metadata", "annotations", "argocd.argoproj.io/hook"), testutil.Field(t, doc, "metadata", "name")
						carried := hook == "Sync" || hook == "PostSync" && name == "mitstack-openbao-rollout" && f.Path == "components/openbao/manifest.yaml"
						assert.True(t, carried, "Job %v of %s is the carriage's handover, or OpenBao's rollout in OpenBao's file, and no Job of the build", name, f.Path)
					}
				}
			}
		})
	}
}

func TestRelease_RefusesAPlaneThatCannotRenderTheBuildsOwnManifests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []func(*harness)
		plane func(h *harness) controlplane.Plane
		want  string
	}{
		{"a plane without the gateway CA", nil, func(h *harness) controlplane.Plane { return h.plane }, "gateway CA"},
		{"a plane that hosts no platform and names no address for the cluster repository", nil, func(h *harness) controlplane.Plane {
			p, err := h.plane.WithGatewayCA()
			require.NoError(h.t, err)
			p.RepoURL = ""
			return p
		}, "repository"},
		{"a hosting plane that names no cluster repository", []func(*harness){hosting}, func(h *harness) controlplane.Plane {
			p, err := h.plane.WithGatewayCA()
			require.NoError(h.t, err)
			hosted := *p.Hosting
			hosted.Repository = ""
			p.Hosting = &hosted
			return p
		}, "repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)

			_, err := tc.plane(h).Release(h.read())

			assert.ErrorContains(t, err, tc.want, "the refusal")
		})
	}
}

// kitJobLabel and kitJobValue are the label each Job the kit writes carries, by which the node report carries its message.
const kitJobLabel, kitJobValue = "mitstack.dev/rendered-by", "talos-cluster-kit"

// jobOf is the Job a Job or a CronJob's template describes, or false for any other kind.
func jobOf(doc map[string]any) (map[string]any, bool) {
	switch doc["kind"] {
	case "Job":
		return doc, true
	case "CronJob":
		spec, _ := doc["spec"].(map[string]any)
		job, ok := spec["jobTemplate"].(map[string]any)
		return job, ok
	}
	return nil, false
}

func labelsOf(object map[string]any) map[string]any {
	metadata, _ := object["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	return labels
}

func TestPlane_EveryJobAndCronJobTheKitWritesLabelsItsJobsAsTheKitsAndRunsEachContainerWithFallbackToLogsOnError(t *testing.T) {
	h := newHarness(t, hosting, telemetry)
	h.withLoadBalancers()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	built, err := plane.BuildSet(context.Background(), testStart, "enrolment-token")
	require.NoError(t, err)
	builder := plane
	builder.Hosting = nil
	unhosted, err := builder.ClusterComponents("enrolment-token")
	require.NoError(t, err)
	files, err := h.releaseOf(plane).Files(testutil.InitRepository)
	require.NoError(t, err)
	removal, err := etcd.RemoveMemberJob(etcd.RemoveMember{Hostname: "shop-node-nbg1", ToolsImage: tools.Image, StepStart: testStart, Guard: redundanttest.Guard,
		Report: report.Read{At: testStart, Body: []byte(`{"etcd":{"members":[{"hostname":"shop-node-fsn1","healthy":true},{"hostname":"shop-node-nbg1","healthy":false},{"hostname":"shop-node-hel1","healthy":true}]}}`)}})
	require.NoError(t, err)
	rendered := [][]byte{removal}
	for _, m := range slices.Concat(built.Served, built.Inline, unhosted.Inline) {
		rendered = append(rendered, m.Bytes)
	}
	for _, f := range files {
		rendered = append(rendered, f.Bytes)
	}

	var written, wrong []string
	for _, manifest := range rendered {
		for _, doc := range testutil.Documents(t, manifest) {
			job, ok := jobOf(doc)
			// An upstream chart writes its own Job's pod spec, and the pinned argo-cd chart has no value for the policy.
			if !ok || labelsOf(doc)["helm.sh/chart"] != nil {
				continue
			}
			name := fmt.Sprint(testutil.Field(t, doc, "metadata", "name"))
			written = append(written, name)
			if labelsOf(job)[kitJobLabel] != kitJobValue {
				wrong = append(wrong, fmt.Sprintf("%s %s gives its Jobs no %s: %s label", doc["kind"], name, kitJobLabel, kitJobValue))
			}
			spec := testutil.Field(t, job, "spec", "template", "spec").(map[string]any)
			inits, _ := spec["initContainers"].([]any)
			containers, _ := spec["containers"].([]any)
			for _, c := range slices.Concat(inits, containers) {
				if policy := testutil.Field(t, c, "terminationMessagePolicy"); policy != "FallbackToLogsOnError" {
					wrong = append(wrong, fmt.Sprintf("%s %s runs container %v with terminationMessagePolicy %v", doc["kind"], name, testutil.Field(t, c, "name"), policy))
				}
			}
		}
	}

	assert.Subset(t, written, []string{"component-cilium-handover", etcd.JobName("shop-node-nbg1"), "mitstack-openbao-init", "mitstack-node-report",
		"mitstack-security-scan", "mitstack-mesh-probe", "mitstack-enrolment", "mitstack-repository-token", "mitstack-node-topology",
		"mitstack-telemetry-lease", "mitstack-openbao-rollout"}, "the walk reaches each renderer's Job")
	assert.Empty(t, wrong, "a failed container leaves the end of its output as its message only under FallbackToLogsOnError, and the report carries it only from a Job with the kit's label")
}

func TestRelease_AClusterRequiringAnOTLPBackendCarriesTheTelemetryCollectorItsLeaseJobAndItsEmptyTokenSecret(t *testing.T) {
	h := newHarness(t, telemetry)
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	release, err := plane.Release(h.read())

	require.NoError(t, err)
	assert.Contains(t, manifestNames(release.Components), "mitstack-telemetry")
	tokens := testutil.Documents(t, testutil.Manifest(t, "mitstack-cluster-tokens", release.Components).Bytes)
	lease := testutil.Named(t, tokens, "CronJob", "mitstack-telemetry-lease")
	assert.Equal(t, "mitstack-node", testutil.Field(t, lease, "metadata", "namespace"), "the namespace that holds the report token")
	assert.Equal(t, "*/15 * * * *", testutil.Field(t, lease, "spec", "schedule"))
	assert.Equal(t, "Forbid", testutil.Field(t, lease, "spec", "concurrencyPolicy"))
	assert.EqualValues(t, 0, testutil.Field(t, lease, "spec", "jobTemplate", "spec", "backoffLimit"), "the next run, 15 minutes on, is the retry")
	pod := testutil.Field(t, lease, "spec", "jobTemplate", "spec", "template", "spec")
	assert.Equal(t, "mitstack-telemetry-lease", testutil.Field(t, pod, "serviceAccountName"))
	container := testutil.Field(t, pod, "containers", "0")
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/cluster-tokens.sh", "lease"}, testutil.Field(t, container, "command"))
	env := testutil.ContainerEnv(t, container)
	assert.Equal(t, platformURL, env["PLATFORM_URL"])
	assert.Equal(t, testutil.TelemetryExport().Resource, env["LEASE_RESOURCE"], "the resource the block requires, whose push token the job leases")
	assert.Equal(t, "mitstack-report-token", testutil.Field(t, pod, "volumes", "0", "secret", "secretName"), "the job's bearer, the report token")
	assert.Contains(t, testutil.Field(t, container, "volumeMounts"), map[string]any{"name": "token", "mountPath": "/var/run/mitstack", "readOnly": true})
	assert.Contains(t, testutil.Field(t, pod, "volumes"), map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory"}}, "no token touches the node's disk")
	secret := testutil.Named(t, tokens, "Secret", "mitstack-telemetry-token")
	assert.Equal(t, "kube-system", testutil.Field(t, secret, "metadata", "namespace"))
	assert.Nil(t, secret["data"], "the kit commits the push token's Secret empty, for the lease job to fill")
	assert.Nil(t, secret["stringData"])
	role := testutil.Named(t, tokens, "Role", "mitstack-telemetry-lease")
	assert.Equal(t, "kube-system", testutil.Field(t, role, "metadata", "namespace"))
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{"mitstack-telemetry-token"},
		"verbs": []any{"get", "patch"}}}, role["rules"], "the job may read and patch the one Secret, and create none")
	binding := testutil.Named(t, tokens, "RoleBinding", "mitstack-telemetry-lease")
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-telemetry-lease", "namespace": "mitstack-node"}}, binding["subjects"])
	files, err := release.Files(testutil.InitRepository)
	require.NoError(t, err)
	var carried []string
	for _, f := range files {
		for _, doc := range testutil.Documents(t, f.Bytes) {
			carried = append(carried, fmt.Sprintf("%v %v", doc["kind"], testutil.Field(t, doc, "metadata", "name")))
		}
	}
	assert.Subset(t, carried, []string{"Secret mitstack-telemetry-token", "CronJob mitstack-telemetry-lease", "DaemonSet mitstack-telemetry-node",
		"Deployment mitstack-telemetry-cluster"}, "what Argo CD applies from the cluster repository")
}

func TestRelease_TheCollectorOfAClusterThatHostsThePlatformScrapesEachOpenBaoMemberAndAnotherClustersScrapesArgoCDAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []func(*harness)
		jobs    []string
	}{
		{"a cluster that hosts the platform", []func(*harness){hosting, telemetry}, []string{"argocd", "openbao"}},
		{"a cluster that hosts none", []func(*harness){telemetry}, []string{"argocd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.options...)
			h.withLoadBalancers()
			plane, err := h.plane.WithGatewayCA()
			require.NoError(t, err)

			release, err := plane.Release(h.read())

			require.NoError(t, err)
			jobs := testutil.ScrapeJobs(t, testutil.Documents(t, testutil.Manifest(t, "mitstack-telemetry", release.Components).Bytes))
			assert.Equal(t, tc.jobs, slices.Sorted(maps.Keys(jobs)), "the cluster copy's scrape jobs")
		})
	}
}

func TestRelease_AClusterRequiringNoOTLPBackendCarriesNoTelemetryCollectorAndNoExportEndpoint(t *testing.T) {
	h := newHarness(t, hosting)
	h.withLoadBalancers()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	built, err := plane.BuildSet(context.Background(), testStart, "enrolment-token")
	require.NoError(t, err)

	release, err := plane.Release(h.read())

	require.NoError(t, err)
	for _, m := range slices.Concat(release.Components, built.Served, built.Inline) {
		for _, doc := range testutil.Documents(t, m.Bytes) {
			name := fmt.Sprint(testutil.Field(t, doc, "metadata", "name"))
			assert.False(t, strings.HasPrefix(name, "mitstack-telemetry"), "%s renders %v %s", m.Name, doc["kind"], name)
		}
	}
}

func TestPlane_OutputsTheCollectorsOTLPReceiverOnlyForAClusterRequiringAnOTLPBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []func(*harness)
		want    map[string]string
	}{
		{"a cluster requiring an OTLP backend", []func(*harness){telemetry}, map[string]string{"otlpEndpoint": components.TelemetryOTLPEndpoint}},
		{"a cluster requiring none", nil, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.options...)
			h.built()

			outputs, _ := h.plane.Outputs(h.read())

			got := map[string]string{}
			if endpoint, present := outputs["otlpEndpoint"]; present {
				got["otlpEndpoint"] = endpoint
			}
			assert.Equal(t, tc.want, got, "the otlpEndpoint output of %s", tc.name)
		})
	}
}

func TestRelease_AClusterInFsn1Nbg1AndHel1RunsThreeCoreDNSReplicasWithASpreadThatAllowsNoTwoInOneLocation(t *testing.T) {
	require.Equal(t, []string{"fsn1", "nbg1", "hel1"}, controlplane.Locations, "the locations a node of the cluster runs in")
	h := newHarness(t)
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	release := h.releaseOf(plane)

	want, err := components.CoreDNS(controlplane.Locations)
	require.NoError(t, err)
	assert.Equal(t, want, []components.Manifest{testutil.Manifest(t, "coredns", release.Components)}, "the Deployment the golden coredns-3-locations.yaml pins")
}

// spreadOver is the manifest of set named name once its database operators are spread over zones.
func spreadOver(t *testing.T, set components.Set, name string, zones ...string) components.Manifest {
	t.Helper()
	spread, err := set.DatabaseOperatorsAcrossZones(zones)
	require.NoError(t, err)
	return testutil.Manifest(t, name, spread.Served)
}

func TestRelease_SpreadsTheDatabaseOperatorsOverTheZonesOfTheNodesAPodWithoutATolerationRunsOn(t *testing.T) {
	steadyAndBurst := func(h *harness) []controlplane.Server {
		return h.grow(pools.CloudPool{ServerTypes: []string{controlplane.DefaultServerType}, Locations: []string{"fsn1", "nbg1"}, Min: 1, Max: 2})
	}
	for _, tc := range []struct {
		name  string
		opts  []func(*harness)
		pool  func(h *harness) []controlplane.Server
		set   components.Set
		zones []string
	}{
		{"a hosting plane whose apps run on workers and whose Cloud pool sits in fsn1 and nbg1 runs both operators in those two",
			[]func(*harness){hosting, appsOnWorkers}, func(h *harness) []controlplane.Server { return h.poolWorkers(2, "fsn1", "nbg1") },
			components.HostingServed(), []string{"fsn1", "nbg1"}},
		{"a plane whose apps run on workers in fsn1 alone keeps one replica, since its control-plane nodes are tainted",
			[]func(*harness){appsOnWorkers}, func(h *harness) []controlplane.Server { return h.poolWorkers(1, "fsn1") },
			components.Default(), nil},
		{"a plane whose apps run on workers counts no zone that holds only a burst member of its Cloud pool",
			[]func(*harness){appsOnWorkers}, steadyAndBurst, components.Default(), nil},
		{"a plane whose apps run on its control-plane nodes spreads over their three locations",
			nil, func(*harness) []controlplane.Server { return nil }, components.Default(), controlplane.Locations},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.built()
			plane, err := h.plane.WithGatewayCA()
			require.NoError(t, err)
			workers := tc.pool(h)
			locations := map[string]bool{}
			for _, w := range workers {
				locations[w.Location] = true
			}
			require.Len(t, locations, len(workers), "the Cloud pool's workers %v each sit in their own location", workers)

			release := h.releaseOf(plane)

			for _, m := range tc.set.Served {
				if m.Name == "cloudnative-pg" || m.Name == "plugin-barman-cloud" {
					assert.Equal(t, spreadOver(t, tc.set, m.Name, tc.zones...), testutil.Manifest(t, m.Name, release.Components),
						"the %s manifest of a cluster whose pods may run in %v", m.Name, tc.zones)
				}
			}
		})
	}
}

func TestRelease_SpreadsTheDatabaseOperatorsOnceTheCloudPoolGainsAWorkerInASecondZone(t *testing.T) {
	h := newHarness(t, appsOnWorkers)
	h.built()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	h.poolWorkers(1, "fsn1")
	inOne := h.releaseOf(plane)

	h.poolWorkers(2, "fsn1", "nbg1")
	inTwo := h.releaseOf(plane)

	assert.Equal(t, testutil.Manifest(t, "cloudnative-pg", components.Default().Served), testutil.Manifest(t, "cloudnative-pg", inOne.Components),
		"the operator of a cluster whose workers sit in fsn1")
	assert.Equal(t, spreadOver(t, components.Default(), "cloudnative-pg", "fsn1", "nbg1"), testutil.Manifest(t, "cloudnative-pg", inTwo.Components),
		"the operator of the cluster once a worker in nbg1 joins")
}

func TestRelease_CountsNoZoneForACloudWorkerThatIsOff(t *testing.T) {
	h := newHarness(t, appsOnWorkers)
	h.built()
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	workers := h.poolWorkers(2, "fsn1", "nbg1")
	i := slices.IndexFunc(workers, func(s controlplane.Server) bool { return s.Location == "nbg1" })
	require.GreaterOrEqual(t, i, 0, "the Cloud pool's workers %v hold one in nbg1", workers)
	h.cloud.Servers[workers[i].ID].Status = controlplane.ServerOff

	release := h.releaseOf(plane)

	assert.Equal(t, testutil.Manifest(t, "cloudnative-pg", components.Default().Served), testutil.Manifest(t, "cloudnative-pg", release.Components),
		"the operator of a cluster whose only worker outside fsn1 is off")
}

const (
	runnerImage = "ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300"
	dindImage   = "docker.io/library/docker:29.10.0-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c"
	// otherRunnerImage is a later actions-runner release, which is another image to mirror.
	otherRunnerImage = "ghcr.io/actions/actions-runner:2.339.0@sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

func runnerInputs(options ...func(*components.RunnerInputs)) *components.RunnerInputs {
	in := components.RunnerInputs{Organization: "acme-org", Group: "mitstack-shared", ScaleSet: "mitstack-ci",
		RunnerImage: runnerImage, DindImage: dindImage, MaxRunners: 4, LeaseResource: "github-runner-token"}
	for _, option := range options {
		option(&in)
	}
	return &in
}

func runners(h *harness) { h.plane.Runners = runnerInputs() }

func TestRelease_APlaneWithRunnersCarriesTheRunnerTokenSecretTheLeaseCronJobOffBurstMembersAndTheScaleSet(t *testing.T) {
	h := newHarness(t, runners)
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	release, err := plane.Release(h.read())

	require.NoError(t, err)
	assert.Contains(t, manifestNames(release.Components), "mitstack-runner-scale-set", "the scale set")
	assert.Contains(t, manifestNames(release.Components), "gha-runner-scale-set-controller", "ARC's controller")
	docs := testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-token", release.Components).Bytes)
	secret := testutil.Named(t, docs, "Secret", "mitstack-runner-token")
	assert.Equal(t, "mitstack-runners", testutil.Field(t, secret, "metadata", "namespace"), "the namespace ARC reads the Secret from")
	assert.Nil(t, secret["data"], "the kit commits the token's Secret empty, for the lease job to fill")
	assert.Nil(t, secret["stringData"])
	account := testutil.Named(t, docs, "ServiceAccount", "mitstack-runner-lease")
	assert.Equal(t, "mitstack-node", testutil.Field(t, account, "metadata", "namespace"), "the namespace that holds the report token")
	role := testutil.Named(t, docs, "Role", "mitstack-runner-lease")
	assert.Equal(t, "mitstack-runners", testutil.Field(t, role, "metadata", "namespace"))
	assert.Equal(t, []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{"mitstack-runner-token"}, "verbs": []any{"patch"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"deletecollection"}},
	}, role["rules"], "the job may patch the one Secret and delete pods by collection, and read, list or create none")
	binding := testutil.Named(t, docs, "RoleBinding", "mitstack-runner-lease")
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-runner-lease", "namespace": "mitstack-node"}}, binding["subjects"])
	lease := testutil.Named(t, docs, "CronJob", "mitstack-runner-lease")
	assert.Equal(t, "mitstack-node", testutil.Field(t, lease, "metadata", "namespace"))
	assert.Equal(t, "*/30 * * * *", testutil.Field(t, lease, "spec", "schedule"), "twice in the hour a token lives")
	assert.Equal(t, "Forbid", testutil.Field(t, lease, "spec", "concurrencyPolicy"))
	pod := testutil.Field(t, lease, "spec", "jobTemplate", "spec", "template", "spec")
	assert.Equal(t, "mitstack-runner-lease", testutil.Field(t, pod, "serviceAccountName"))
	container := testutil.Field(t, pod, "containers", "0")
	assert.Equal(t, []any{"bash", tools.ScriptDir + "/cluster-tokens.sh", "runner-lease"}, testutil.Field(t, container, "command"))
	env := testutil.ContainerEnv(t, container)
	assert.Equal(t, platformURL, env["PLATFORM_URL"])
	assert.Equal(t, "github-runner-token", env["LEASE_RESOURCE"], "the resource the block requires, whose token the job leases")
	assert.Equal(t, "mitstack-report-token", testutil.Field(t, pod, "volumes", "0", "secret", "secretName"), "the job's bearer, the report token")
	assert.Contains(t, testutil.Field(t, container, "volumeMounts"), map[string]any{"name": "token", "mountPath": "/var/run/mitstack", "readOnly": true})
	assert.Equal(t, true, testutil.Field(t, container, "securityContext", "readOnlyRootFilesystem"), "the job's root filesystem")
	assert.Equal(t, true, testutil.Field(t, pod, "securityContext", "runAsNonRoot"), "the job runs as a non-root user")
	terms := testutil.Field(t, pod, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)
	require.NotEmpty(t, terms, "the job's node selector terms")
	for _, term := range terms {
		assert.True(t, slices.ContainsFunc(testutil.Field(t, term, "matchExpressions").([]any), func(e any) bool {
			m, _ := e.(map[string]any)
			return m["key"] == components.BurstLabel && m["operator"] == "DoesNotExist"
		}), "term %v keeps the job off a burst member", term)
	}
	files, err := release.Files(testutil.InitRepository)
	require.NoError(t, err)
	var carried []string
	for _, f := range files {
		for _, doc := range testutil.Documents(t, f.Bytes) {
			carried = append(carried, fmt.Sprintf("%v %v", doc["kind"], testutil.Field(t, doc, "metadata", "name")))
		}
	}
	assert.Subset(t, carried, []string{"Secret mitstack-runner-token", "CronJob mitstack-runner-lease", "AutoscalingRunnerSet mitstack-ci"},
		"what Argo CD applies from the cluster repository")
}

func TestRelease_APlaneWithoutRunnersCarriesNoRunnerManifestAndTheSameClusterTokensAsWithThem(t *testing.T) {
	h := newHarness(t)
	without, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	with := without
	with.Runners = runnerInputs()

	bare, err := without.Release(h.read())
	require.NoError(t, err)
	equipped, err := with.Release(h.read())
	require.NoError(t, err)

	for _, name := range []string{"mitstack-runner-token", "mitstack-runner-scale-set", "gha-runner-scale-set-controller", "mitstack-runners"} {
		assert.NotContains(t, manifestNames(bare.Components), name, "a plane that asks for no runners")
	}
	assert.Equal(t, string(testutil.Manifest(t, "mitstack-cluster-tokens", bare.Components).Bytes),
		string(testutil.Manifest(t, "mitstack-cluster-tokens", equipped.Components).Bytes), "the telemetry and repository credentials render as they do without runners")
}

func TestPlane_TheGatewayListChangesWithTheRunnerImagesAndNotWithoutRunners(t *testing.T) {
	h := newHarness(t)
	plain, err := h.plane.WithGatewayCA()
	require.NoError(t, err)
	digest := func(p controlplane.Plane) string {
		t.Helper()
		d, err := p.GatewayListDigest()
		require.NoError(t, err)
		return d
	}
	with := func(options ...func(*components.RunnerInputs)) controlplane.Plane {
		p := plain
		p.Runners = runnerInputs(options...)
		return p
	}
	later := func(in *components.RunnerInputs) { in.RunnerImage = otherRunnerImage }

	assert.Equal(t, digest(plain), digest(plain), "a plane without runners lists the same images every time")
	assert.NotEqual(t, digest(plain), digest(with()), "the runner services add images to mirror")
	assert.Equal(t, digest(with()), digest(with()), "the same runner images list the same")
	assert.NotEqual(t, digest(with()), digest(with(later)), "a new runner image replaces the gateway")
	assert.Equal(t, digest(with()), digest(with(func(in *components.RunnerInputs) { in.MaxRunners = 9 })), "a setting no image names leaves the list alone")
}

func TestPlane_AnUpgradeThatAddsRunnerServicesReplacesEachGatewayAndTheReplacementsMirrorTheControllerRunnerAndDindImages(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	old := controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID)
	before := h.gatewayIn(controlplane.GatewayLocation).Labels[controlplane.KeyGatewayList]
	h.plane.Runners = runnerInputs()

	h.upgrade(func() {})

	for _, loc := range controlplane.Locations {
		gw := h.gatewayIn(loc)
		assert.NotContains(t, old, gw.ID, "the gateway in %s was not replaced", loc)
		assert.NotEqual(t, before, gw.Labels[controlplane.KeyGatewayList], "the replacement in %s's list label", loc)
		assert.Subset(t, h.mirrored(gw), []string{runnerImage, dindImage}, "the replacement in %s mirrors the runner and dind images", loc)
		assert.True(t, slices.ContainsFunc(h.mirrored(gw), func(ref string) bool {
			return strings.HasPrefix(ref, "ghcr.io/actions/gha-runner-scale-set-controller:")
		}), "the replacement in %s mirrors the controller image", loc)
	}
}

func TestPlane_AnInstallRendersTheNodesForCIJobsOnlyForAPlaneWithRunners(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*harness)
		want bool
	}{
		{"a plane with runners renders its nodes' kubelet for CI jobs", []func(*harness){runners}, true},
		{"a plane without runners leaves the kubelet's settings alone", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)

			require.NoError(t, h.converge())

			require.NotEmpty(t, h.installed, "the clusters the plane installed")
			assert.Equal(t, tc.want, h.installed[0].RunsCIJobs, "the cluster the install rendered")
		})
	}
}
