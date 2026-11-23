// The runner services' shared package caches: a Go module cache, an npm cache and a Docker Hub image mirror.
package components_test

import (
	"fmt"
	"path"
	"strings"
	"testing"

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

func TestWithRunners_RendersTheGoNpmAndImageCachesEachOnADiskOfItsOwnSizeEvictingBeforeItFills(t *testing.T) {
	docs := cacheDocs(t)

	for _, tc := range []struct {
		name, server, sweeper, size, evictAt, probe string
		sweeperCommand                              []any
	}{
		{"go-cache", "cache", "cache", "6Gi", "5Gi", "/-/healthz", []any{"package-cache", "go"}},
		{"npm-cache", "cache", "cache", "4Gi", "3Gi", "/-/healthz", []any{"package-cache", "npm"}},
		{"image-cache", "registry", "sweep", "12Gi", "10Gi", "/", []any{"package-cache", "sweep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deployment := testutil.Named(t, docs, "Deployment", tc.name)
			pod := testutil.Field(t, deployment, "spec", "template", "spec")
			server := testutil.Item(t, pod, tc.server, "containers")
			sweeper := testutil.Item(t, pod, tc.sweeper, "containers")

			assert.Equal(t, components.RunnersNamespace, testutil.Field(t, deployment, "metadata", "namespace"), "where %s runs", tc.name)
			assert.EqualValues(t, 1, testutil.Field(t, deployment, "spec", "replicas"), "the replicas of %s", tc.name)
			assert.Equal(t, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}},
				testutil.Field(t, deployment, "spec", "strategy"), "%s serves on until the pod of a new release answers", tc.name)
			assert.Equal(t, testutil.ToolsImage, sweeper["image"], "the image whose package-cache %s's %s container runs", tc.name, tc.sweeper)
			assert.Equal(t, tc.sweeperCommand, sweeper["command"], "what %s's %s container runs", tc.name, tc.sweeper)
			assert.Equal(t, tc.size, testutil.Field(t, testutil.Item(t, pod, "cache", "volumes"), "emptyDir", "sizeLimit"), "the disk %s keeps its entries on", tc.name)
			assert.Equal(t, tc.size, testutil.Field(t, server, "resources", "requests", "ephemeral-storage"), "the node disk %s asks the scheduler for", tc.name)
			assert.Equal(t, tc.probe, testutil.Field(t, server, "readinessProbe", "httpGet", "path"), "the path %s answers its readiness on, which no package takes", tc.name)
			assert.Equal(t, tc.evictAt, testutil.Flags(sweeper)["max-size"], "the size past which %s evicts its least recently used entries", tc.name)
			assert.Equal(t, "336h", testutil.Flags(sweeper)["max-age"], "the age past which %s evicts an unused entry, fourteen days", tc.name)
		})
	}
}

func TestWithRunners_RunsTheImageCacheAsADockerHubPullThroughMirrorWhoseBlobsItsSweepEvicts(t *testing.T) {
	docs := cacheDocs(t)
	pod := testutil.Field(t, testutil.Named(t, docs, "Deployment", "image-cache"), "spec", "template", "spec")
	registry := testutil.Item(t, pod, "registry", "containers")
	sweep := testutil.Item(t, pod, "sweep", "containers")
	config := testutil.Documents(t, []byte(testutil.Field(t, testutil.Named(t, docs, "ConfigMap", "image-cache"), "data", "config.yml").(string)))[0]

	assert.Equal(t, components.ImageCacheImage, registry["image"], "the registry the image cache runs")
	assert.Equal(t, "https://registry-1.docker.io", testutil.Field(t, config, "proxy", "remoteurl"), "the registry the mirror pulls through from")
	assert.Equal(t, "336h", testutil.Field(t, config, "proxy", "ttl"), "the age past which the registry drops what it pulled")
	assert.Equal(t, "/var/lib/registry", testutil.Field(t, config, "storage", "filesystem", "rootdirectory"), "where the registry keeps its blobs")
	assert.NotContains(t, testutil.Field(t, config, "storage"), "cache", "a blob descriptor cache would name a blob the sweep removed as present")
	assert.Equal(t, "/var/lib/registry/docker/registry/v2/blobs", testutil.Flags(sweep)["dir"], "what the sweep evicts from")
	assert.Equal(t, "image-cache", testutil.Field(t, testutil.Item(t, pod, "config", "volumes"), "configMap", "name"), "the ConfigMap the config volume holds")
	assert.Equal(t, "/etc/distribution", testutil.Item(t, registry, "config", "volumeMounts")["mountPath"], "where the registry's image reads config.yml")
	for _, c := range []map[string]any{registry, sweep} {
		assert.Equal(t, "/var/lib/registry", testutil.Item(t, c, "cache", "volumeMounts")["mountPath"], "where %s mounts the cache's disk", c["name"])
	}
}

func TestWithRunners_GivesEachJobTheGoCacheInItsEnvironmentTheNpmCacheInAGlobalNpmrcAndTheImageCacheAsItsDockerDaemonsMirror(t *testing.T) {
	docs := cacheDocs(t)
	scaleSet := testutil.Documents(t, testutil.Manifest(t, "mitstack-runner-scale-set", runnerSet(t).Inline).Bytes)
	pod := testutil.Field(t, testutil.Named(t, scaleSet, "AutoscalingRunnerSet", "mitstack-ci"), "spec", "template", "spec")
	runner := testutil.Item(t, pod, "runner", "containers")
	env := testutil.ContainerEnv(t, runner)
	goCache, fallback, _ := strings.Cut(env["GOPROXY"], "|")
	writer := testutil.Item(t, pod, "npmrc", "initContainers")
	writerEnv := testutil.ContainerEnv(t, writer)

	assert.Equal(t, "https://proxy.golang.org,direct", fallback, "where the go command turns on any error of the Go cache")
	assert.NotContains(t, env, "npm_config_registry", "a registry in the environment beats the npmrc setup-node writes for a publish")
	assert.Equal(t, env["npm_config_globalconfig"], writerEnv["npm_config_globalconfig"], "the runner reads the global npmrc the init container writes")
	assert.Equal(t, []any{"sh", "-c", `printf 'registry=%s\n' "$NPM_REGISTRY" > "$npm_config_globalconfig"`}, writer["command"], "what writes the npmrc")
	assert.Equal(t, path.Dir(env["npm_config_globalconfig"]), testutil.Item(t, runner, "npmrc", "volumeMounts")["mountPath"], "where the runner mounts the npmrc")
	assert.Equal(t, path.Dir(env["npm_config_globalconfig"]), testutil.Item(t, writer, "npmrc", "volumeMounts")["mountPath"], "where the init container writes it")
	for cache, address := range map[string]string{"go-cache": goCache, "npm-cache": writerEnv["NPM_REGISTRY"], "image-cache": testutil.Flags(testutil.Item(t, pod, "dind", "initContainers"))["registry-mirror"]} {
		assertReaches(t, docs, cache, address)
	}
}

// assertReaches fails unless address reaches the cache's Deployment through its Service on the port the Service serves.
func assertReaches(t *testing.T, docs []map[string]any, cache, address string) {
	t.Helper()
	service := testutil.Named(t, docs, "Service", cache)
	require.Len(t, testutil.Field(t, service, "spec", "ports"), 1, "the ports of the Service %s", cache)
	selector := testutil.Labels(t, service, "spec", "selector")

	assert.Equal(t, fmt.Sprintf("http://%s.%s.svc:%v", cache, components.RunnersNamespace, testutil.Field(t, service, "spec", "ports", "0", "port")),
		strings.TrimSuffix(address, "/"), "the address %s's Service serves on", cache)
	require.NotEmpty(t, selector, "the Service %s's selector", cache)
	assert.Subset(t, cacheLabels(t, docs, cache), selector, "the Service %s selects the pods of the Deployment %s", cache, cache)
}

func TestWithRunners_GivesEveryCacheEmptyDirDisksAloneAndNoToken(t *testing.T) {
	deployments := testutil.Kind(cacheDocs(t), "Deployment")

	require.Len(t, deployments, 3, "the caches' Deployments")
	for _, deployment := range deployments {
		name := testutil.Field(t, deployment, "metadata", "name")
		pod := testutil.Field(t, deployment, "spec", "template", "spec")
		labels := testutil.Field(t, deployment, "spec", "template", "metadata", "labels")

		assert.Equal(t, false, testutil.Field(t, pod, "automountServiceAccountToken"), "%s reaches no cluster token", name)
		assert.Equal(t, true, testutil.Field(t, pod, "securityContext", "runAsNonRoot"), "%s runs as no root user", name)
		assert.NotEmpty(t, testutil.Field(t, labels, "mitstack.dev/app"), "the app-label policy refuses an unlabelled pod")
		assert.NotEqual(t, "runner", testutil.Field(t, labels, "app.kubernetes.io/component"), "%s would reach the internet as a job pod does", name)
		for _, volume := range testutil.Field(t, pod, "volumes").([]any) {
			_, disk := volume.(map[string]any)["emptyDir"]
			_, config := volume.(map[string]any)["configMap"]
			assert.True(t, disk || config, "volume %v of %s is an emptyDir or a ConfigMap, never a host path or a claim that binds the cache to one node", volume, name)
		}
		for _, c := range testutil.Field(t, pod, "containers").([]any) {
			assert.Equal(t, true, testutil.Field(t, c, "securityContext", "readOnlyRootFilesystem"), "container %v of %s writes its disk alone", testutil.Field(t, c, "name"), name)
		}
	}
}

func cacheLabels(t *testing.T, docs []map[string]any, cache string) map[string]string {
	t.Helper()
	return testutil.Labels(t, testutil.Named(t, docs, "Deployment", cache), "spec", "template", "metadata", "labels")
}

func TestWithRunners_LetsJobPodsAloneReachEachCacheAndEachCacheOnlyItsOwnUpstream(t *testing.T) {
	docs := cacheDocs(t)
	pods := map[string]testutil.CiliumEndpoint{
		"a job pod":      testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/component": "runner"}),
		"a listener pod": testutil.Endpoint(components.RunnersNamespace, nil, map[string]string{"app.kubernetes.io/component": "runner-scale-set-listener"}),
		"an app pod":     testutil.Endpoint("app-shop", nil, map[string]string{"app.kubernetes.io/component": "runner"}),
	}
	for _, cache := range components.RunnerCaches {
		pods[cache] = testutil.Endpoint(components.RunnersNamespace, nil, cacheLabels(t, docs, cache))
	}
	want := map[string][]string{
		"a job pod": {"egress with go-cache on 8080/TCP 15008/TCP", "egress with npm-cache on 8080/TCP 15008/TCP",
			"egress with image-cache on 5000/TCP 15008/TCP"},
		"go-cache": {"ingress with a job pod on 8080/TCP 15008/TCP", "egress to proxy.golang.org on 443/TCP",
			"egress to sum.golang.org on 443/TCP"},
		"npm-cache": {"ingress with a job pod on 8080/TCP 15008/TCP", "egress to registry.npmjs.org on 443/TCP"},
		"image-cache": {"ingress with a job pod on 5000/TCP 15008/TCP", "egress to registry-1.docker.io on 443/TCP",
			"egress to auth.docker.io on 443/TCP", "egress to production.cloudflare.docker.com on 443/TCP",
			"egress to docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com on 443/TCP"},
	}

	for pod := range pods {
		assert.ElementsMatch(t, want[pod], testutil.Grants(t, testutil.Named(t, docs, "CiliumClusterwideNetworkPolicy", components.RunnerCachesName+"-flows"), pods, pod), "what the caches' policy grants %s", pod)
	}
}

func TestWithRunners_RefusesACacheImageWithoutItsDigest(t *testing.T) {
	for _, image := range []string{"", "ghcr.io/mitstack-eu/talos-tools:v0.1.0"} {
		_, err := components.Default().WithRunners(runnerInputs(), image)

		assert.ErrorContains(t, err, "cache image", "WithRunners with the cache image %q", image)
	}
}
