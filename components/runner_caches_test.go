// The runner services' shared package caches: a Go module cache, an npm cache and a Docker Hub image mirror.
package components_test

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// cacheDocs are the documents of the runner caches' manifest that a set with runners renders.
func cacheDocs(t *testing.T) []map[string]any {
	t.Helper()
	return testutil.Documents(t, testutil.Manifest(t, components.RunnerCachesName, runnerSet(t).Inline).Bytes)
}

func cachePod(t *testing.T, docs []map[string]any, cache string) map[string]any {
	t.Helper()
	return testutil.Field(t, testutil.Named(t, docs, "Deployment", cache), "spec", "template", "spec").(map[string]any)
}

func containerNamed(t *testing.T, pod any, list, name string) map[string]any {
	t.Helper()
	containers, _ := testutil.Field(t, pod, list).([]any)
	for _, c := range containers {
		if testutil.Field(t, c, "name") == name {
			return c.(map[string]any)
		}
	}
	require.FailNow(t, "no container "+name, "the %s of %v", list, pod)
	return nil
}

// flags are a container's --name=value arguments.
func flags(container map[string]any) map[string]string {
	out := map[string]string{}
	args, _ := container["args"].([]any)
	for _, arg := range args {
		if name, value, ok := strings.Cut(fmt.Sprint(arg), "="); ok && strings.HasPrefix(name, "--") {
			out[strings.TrimPrefix(name, "--")] = value
		}
	}
	return out
}

func volumeNamed(t *testing.T, pod any, name string) map[string]any {
	t.Helper()
	for _, v := range testutil.Field(t, pod, "volumes").([]any) {
		if testutil.Field(t, v, "name") == name {
			return v.(map[string]any)
		}
	}
	require.FailNow(t, "no volume "+name, "the volumes of %v", pod)
	return nil
}

func TestWithRunners_RendersTheGoNpmAndImageCachesEachOnADiskOfItsOwnSizeEvictingBeforeItFills(t *testing.T) {
	docs := cacheDocs(t)

	for _, tc := range []struct {
		name, server, sweeper, size, evictAt string
		sweeperCommand                       []any
	}{
		{"go-cache", "cache", "cache", "6Gi", "5Gi", []any{"package-cache", "go"}},
		{"npm-cache", "cache", "cache", "4Gi", "3Gi", []any{"package-cache", "npm"}},
		{"image-cache", "registry", "sweep", "12Gi", "10Gi", []any{"package-cache", "sweep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deployment := testutil.Named(t, docs, "Deployment", tc.name)
			pod := cachePod(t, docs, tc.name)
			server := containerNamed(t, pod, "containers", tc.server)
			sweeper := containerNamed(t, pod, "containers", tc.sweeper)
			args := flags(sweeper)

			assert.Equal(t, components.RunnersNamespace, testutil.Field(t, deployment, "metadata", "namespace"), "where %s runs", tc.name)
			assert.EqualValues(t, 1, testutil.Field(t, deployment, "spec", "replicas"), "the replicas of %s", tc.name)
			assert.Equal(t, testutil.ToolsImage, sweeper["image"], "the image whose package-cache %s's %s container runs", tc.name, tc.sweeper)
			assert.Equal(t, tc.sweeperCommand, sweeper["command"], "what %s's %s container runs", tc.name, tc.sweeper)
			assert.Equal(t, tc.size, testutil.Field(t, volumeNamed(t, pod, "cache"), "emptyDir", "sizeLimit"), "the disk %s keeps its entries on", tc.name)
			assert.Equal(t, tc.size, testutil.Field(t, server, "resources", "requests", "ephemeral-storage"), "the node disk %s asks the scheduler for", tc.name)
			assert.Equal(t, tc.evictAt, args["max-size"], "the size past which %s evicts its least recently used entries", tc.name)
			age, err := time.ParseDuration(args["max-age"])
			require.NoError(t, err, "the max-age of %s", tc.name)
			assert.Equal(t, 14*24*time.Hour, age, "the age past which %s evicts an unused entry", tc.name)
		})
	}
}

func TestWithRunners_RunsTheImageCacheAsADockerHubPullThroughMirrorWhoseBlobsItsSweepEvicts(t *testing.T) {
	docs := cacheDocs(t)
	pod := cachePod(t, docs, "image-cache")
	registry := containerNamed(t, pod, "containers", "registry")
	sweep := containerNamed(t, pod, "containers", "sweep")
	config := testutil.Documents(t, []byte(testutil.Field(t, testutil.Named(t, docs, "ConfigMap", "image-cache"), "data", "config.yml").(string)))[0]

	assert.Equal(t, components.ImageCacheImage, registry["image"], "the registry the image cache runs")
	assert.Equal(t, "docker.io/library/registry:3.1.2@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c", components.ImageCacheImage)
	assert.Equal(t, "https://registry-1.docker.io", testutil.Field(t, config, "proxy", "remoteurl"), "the registry the mirror pulls through from")
	assert.Equal(t, "336h", testutil.Field(t, config, "proxy", "ttl"), "the age past which the registry drops what it pulled")
	assert.Equal(t, "/var/lib/registry", testutil.Field(t, config, "storage", "filesystem", "rootdirectory"), "where the registry keeps its blobs")
	assert.NotContains(t, testutil.Field(t, config, "storage"), "cache", "a blob descriptor cache would name a blob the sweep removed as present")
	assert.Equal(t, "/var/lib/registry/docker/registry/v2/blobs", flags(sweep)["dir"], "what the sweep evicts from")
	assert.Equal(t, "image-cache", testutil.Field(t, volumeNamed(t, pod, "config"), "configMap", "name"), "the ConfigMap the config volume holds")
	registryMounts, _ := registry["volumeMounts"].([]any)
	assert.True(t, slices.ContainsFunc(registryMounts, func(m any) bool {
		return testutil.Field(t, m, "name") == "config" && testutil.Field(t, m, "mountPath") == "/etc/distribution"
	}), "the registry reads config.yml where its image looks for it")
	for _, c := range []map[string]any{registry, sweep} {
		mounts, _ := c["volumeMounts"].([]any)
		assert.True(t, slices.ContainsFunc(mounts, func(m any) bool {
			return testutil.Field(t, m, "name") == "cache" && testutil.Field(t, m, "mountPath") == "/var/lib/registry"
		}), "container %s mounts the cache's disk at the registry's root", c["name"])
	}
}

func TestWithRunners_GivesEachJobTheGoAndNpmCachesInItsEnvironmentAndTheImageCacheAsItsDockerDaemonsMirror(t *testing.T) {
	set := runnerSet(t)
	docs := testutil.Documents(t, testutil.Manifest(t, components.RunnerCachesName, set.Inline).Bytes)
	scaleSet := testutil.Named(t, testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", set.Inline).Bytes), "AutoscalingRunnerSet", "mitstack-ci")
	pod := testutil.Field(t, scaleSet, "spec", "template", "spec")
	env := map[string]string{}
	for _, e := range containerNamed(t, pod, "containers", "runner")["env"].([]any) {
		env[fmt.Sprint(testutil.Field(t, e, "name"))] = fmt.Sprint(testutil.Field(t, e, "value"))
	}
	var mirror string
	for _, arg := range containerNamed(t, pod, "initContainers", "dind")["args"].([]any) {
		if value, ok := strings.CutPrefix(fmt.Sprint(arg), "--registry-mirror="); ok {
			mirror = value
		}
	}
	goCache, fallback, _ := strings.Cut(env["GOPROXY"], "|")

	assert.Equal(t, "https://proxy.golang.org,direct", fallback, "where the go command turns on any error of the Go cache")
	for cache, address := range map[string]string{"go-cache": goCache, "npm-cache": env["npm_config_registry"], "image-cache": mirror} {
		assertReaches(t, docs, cache, address)
	}
}

// assertReaches fails unless address reaches the cache's Deployment through its Service on the port the Service serves.
func assertReaches(t *testing.T, docs []map[string]any, cache, address string) {
	t.Helper()
	u, err := url.Parse(address)
	require.NoError(t, err, "the address of %s", cache)
	service := testutil.Named(t, docs, "Service", cache)
	labels := testutil.Field(t, testutil.Named(t, docs, "Deployment", cache), "spec", "template", "metadata", "labels").(map[string]any)
	ports := testutil.Field(t, service, "spec", "ports").([]any)
	require.Len(t, ports, 1, "the ports of the Service %s", cache)

	assert.Equal(t, "http", u.Scheme, "the scheme of %s's address %q", cache, address)
	assert.Equal(t, cache+"."+components.RunnersNamespace+".svc", u.Hostname(), "the Service %s's address %q names", cache, address)
	assert.Equal(t, fmt.Sprint(testutil.Field(t, ports[0], "port")), u.Port(), "the Service port %s's address %q names", cache, address)
	selector := testutil.Field(t, service, "spec", "selector").(map[string]any)
	require.NotEmpty(t, selector, "the Service %s's selector", cache)
	for k, v := range selector {
		assert.Equal(t, v, labels[k], "the Service %s selects the pods of the Deployment %s by %s", cache, cache, k)
	}
}

func TestWithRunners_KeepsEveryCacheOffBurstMembersWithEmptyDirDisksAloneNoTokenAndAReadinessProbe(t *testing.T) {
	deployments := testutil.Kind(cacheDocs(t), "Deployment")

	require.Len(t, deployments, 3, "the caches' Deployments")
	for _, deployment := range deployments {
		name := testutil.Field(t, deployment, "metadata", "name")
		pod := testutil.Field(t, deployment, "spec", "template", "spec")
		terms, _ := testutil.Field(t, pod, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)

		require.NotEmpty(t, terms, "the node selector terms of %s", name)
		for _, term := range terms {
			assert.True(t, slices.ContainsFunc(testutil.Field(t, term, "matchExpressions").([]any), func(e any) bool {
				m, _ := e.(map[string]any)
				return m["key"] == components.BurstLabel && m["operator"] == "DoesNotExist"
			}), "%s refuses a burst member, whose removal would take the cache's entries", name)
		}
		assert.Equal(t, false, testutil.Field(t, pod, "automountServiceAccountToken"), "%s reaches no cluster token", name)
		assert.Equal(t, true, testutil.Field(t, pod, "securityContext", "runAsNonRoot"), "%s runs as no root user", name)
		assert.NotEmpty(t, testutil.Field(t, deployment, "spec", "template", "metadata", "labels", "mitstack.dev/app"), "the app-label policy refuses an unlabelled pod")
		assert.NotEqual(t, "runner", testutil.Field(t, deployment, "spec", "template", "metadata", "labels", "app.kubernetes.io/component"), "%s would reach the internet as a job pod does", name)
		for _, volume := range testutil.Field(t, pod, "volumes").([]any) {
			_, disk := volume.(map[string]any)["emptyDir"]
			_, config := volume.(map[string]any)["configMap"]
			assert.True(t, disk || config, "volume %v of %s is an emptyDir or a ConfigMap, never a host path or a claim that binds the cache to one node", volume, name)
		}
		for _, c := range testutil.Field(t, pod, "containers").([]any) {
			if testutil.Field(t, c, "name") != "sweep" {
				assert.NotNil(t, testutil.Field(t, c, "readinessProbe"), "container %v of %s says when it stops answering", testutil.Field(t, c, "name"), name)
			}
			assert.Equal(t, true, testutil.Field(t, c, "securityContext", "readOnlyRootFilesystem"), "container %v of %s writes its disk alone", testutil.Field(t, c, "name"), name)
		}
	}
}

func cachePolicy(t *testing.T) map[string]any {
	t.Helper()
	for _, doc := range cacheDocs(t) {
		if strings.HasPrefix(fmt.Sprint(doc["kind"]), "Cilium") {
			return doc
		}
	}
	require.FailNow(t, "the caches' Cilium policy is missing")
	return nil
}

func podLabels(t *testing.T, docs []map[string]any, deployment string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, v := range testutil.Field(t, testutil.Named(t, docs, "Deployment", deployment), "spec", "template", "metadata", "labels").(map[string]any) {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func TestWithRunners_LetsJobPodsAloneReachEachCacheAndEachCacheOnlyItsOwnUpstream(t *testing.T) {
	docs := cacheDocs(t)
	pods := map[string]testutil.CiliumEndpoint{
		"a job pod":      testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/component": "runner"}),
		"a listener pod": testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/component": "runner-scale-set-listener"}),
		"an app pod":     testutil.Endpoint("app-shop", nil, map[string]string{"app.kubernetes.io/component": "runner"}),
		"go-cache":       testutil.Endpoint(components.RunnersNamespace, nil, podLabels(t, docs, "go-cache")),
		"npm-cache":      testutil.Endpoint(components.RunnersNamespace, nil, podLabels(t, docs, "npm-cache")),
		"image-cache":    testutil.Endpoint(components.RunnersNamespace, nil, podLabels(t, docs, "image-cache")),
	}
	want := map[string][]string{
		"a job pod": {
			"egress with go-cache on 8080/TCP 15008/TCP",
			"egress with npm-cache on 8080/TCP 15008/TCP",
			"egress with image-cache on 5000/TCP 15008/TCP",
		},
		"go-cache": {
			"ingress with a job pod on 8080/TCP 15008/TCP",
			"egress to proxy.golang.org on 443/TCP",
			"egress to sum.golang.org on 443/TCP",
		},
		"npm-cache": {
			"ingress with a job pod on 8080/TCP 15008/TCP",
			"egress to registry.npmjs.org on 443/TCP",
		},
		"image-cache": {
			"ingress with a job pod on 5000/TCP 15008/TCP",
			"egress to registry-1.docker.io on 443/TCP",
			"egress to auth.docker.io on 443/TCP",
			"egress to production.cloudflare.docker.com on 443/TCP",
			"egress to docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com on 443/TCP",
		},
	}

	for pod := range pods {
		assert.ElementsMatch(t, want[pod], testutil.Grants(t, cachePolicy(t), pods, pod), "what the caches' policy grants %s", pod)
	}
}

func TestWithRunners_RefusesACacheImageWithoutItsDigest(t *testing.T) {
	for _, image := range []string{"", "ghcr.io/mitstack-eu/talos-tools:v0.1.0"} {
		_, err := components.Default().WithRunners(runnerInputs(), image)

		assert.ErrorContains(t, err, "cache image", "WithRunners with the cache image %q", image)
	}
}
