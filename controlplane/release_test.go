package controlplane_test

// The release a running cluster takes through its repository is the set its build applied, but what holds its keys.

import (
	"context"
	"encoding/json"
	"fmt"
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
		// releaseOnly is what no builder's user data has room for, so the release alone carries it.
		releaseOnly []string
	}{
		{"a cluster that hosts no platform", nil, nil},
		{"a cluster that hosts the platform", []func(*harness){hosting}, nil},
		{"a cluster that hosts the platform and ships its telemetry", []func(*harness){hosting, telemetry}, []string{"mitstack-telemetry"}},
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
			for _, name := range tc.releaseOnly {
				carriedAtBuild = append(carriedAtBuild, testutil.Manifest(t, name, release.Components))
			}
			assert.ElementsMatch(t, manifestNames(carriedAtBuild), manifestNames(release.Components), "the manifests the release carries")
			assert.Subset(t, manifestNames(release.Components), []string{"mitstack-node-report", "mitstack-cluster-tokens", "mitstack-node-topology"}, "the plane's own manifests")
			assert.Equal(t, tools.Image, release.ToolsImage, "the tools image each component's handover Job runs, which this kit release published")
			want, err := rollout.Release{Components: carriedAtBuild, ToolsImage: release.ToolsImage}.Files(testutil.InitRepository)
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
						assert.Equal(t, "Sync", testutil.Field(t, doc, "metadata", "annotations", "argocd.argoproj.io/hook"),
							"the one Job %s holds is the carriage's handover, and no Job of the build", f.Path)
					}
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
		"mitstack-telemetry-lease"}, "the walk reaches each renderer's Job")
	assert.Empty(t, wrong, "a failed container leaves the end of its output as its message only under FallbackToLogsOnError, and the report carries it only from a Job with the kit's label")
}

func object(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	of := testutil.Kind(docs, kind)
	i := slices.IndexFunc(of, func(doc map[string]any) bool { return testutil.Field(t, doc, "metadata", "name") == name })
	require.GreaterOrEqual(t, i, 0, "no %s %s", kind, name)
	return of[i]
}

func TestRelease_AClusterRequiringAnOTLPBackendCarriesTheTelemetryCollectorItsLeaseJobAndItsEmptyTokenSecret(t *testing.T) {
	h := newHarness(t, telemetry)
	plane, err := h.plane.WithGatewayCA()
	require.NoError(t, err)

	release, err := plane.Release(h.read())

	require.NoError(t, err)
	assert.Contains(t, manifestNames(release.Components), "mitstack-telemetry")
	tokens := testutil.Documents(t, testutil.Manifest(t, "mitstack-cluster-tokens", release.Components).Bytes)
	lease := object(t, tokens, "CronJob", "mitstack-telemetry-lease")
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
	secret := object(t, tokens, "Secret", "mitstack-telemetry-token")
	assert.Equal(t, "kube-system", testutil.Field(t, secret, "metadata", "namespace"))
	assert.Nil(t, secret["data"], "the kit commits the push token's Secret empty, for the lease job to fill")
	assert.Nil(t, secret["stringData"])
	role := object(t, tokens, "Role", "mitstack-telemetry-lease")
	assert.Equal(t, "kube-system", testutil.Field(t, role, "metadata", "namespace"))
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{"mitstack-telemetry-token"},
		"verbs": []any{"get", "patch"}}}, role["rules"], "the job may read and patch the one Secret, and create none")
	binding := object(t, tokens, "RoleBinding", "mitstack-telemetry-lease")
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
