package components

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

const (
	// RunnerCachesName names the manifest of the runner services' shared Go, npm and image caches.
	RunnerCachesName = "mitstack-runner-caches"
	// ImageCacheImage is the registry the image cache runs as a Docker Hub pull-through mirror.
	ImageCacheImage = "docker.io/library/registry:3.1.2@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"

	// cacheMaxAge is how long an entry may sit unused in a cache, and the registry's own expiry of what it pulled.
	cacheMaxAge = "336h"

	goProxyURL   = "https://proxy.golang.org"
	goSumDBURL   = "https://sum.golang.org"
	npmURL       = "https://registry.npmjs.org"
	dockerHubURL = "https://registry-1.docker.io"
	// goSumDBKey is the key the go command checks sum.golang.org's signatures with, as Go's own source names it.
	goSumDBKey = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"
)

// dockerHubBlobHosts are where Docker Hub sends a pull for its token and its blobs, beside the registry itself.
var dockerHubBlobHosts = []string{"auth.docker.io", "production.cloudflare.docker.com",
	"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com"}

//go:embed runners/caches.yaml
var cachesTemplate string

var caches = tmpl.Must("the runner caches", cachesTemplate)

// packageCache is one shared cache: its Service's port, the node disk it holds, the size its sweep keeps it under, and
// the upstreams it alone reaches.
type packageCache struct {
	Name          string
	Port          int
	Disk, EvictAt string
	Upstream      string
	Hosts         []string
}

// cacheServer is a cache that package-cache serves itself, by the subcommand Kind with the flags beyond the ones every
// cache takes.
type cacheServer struct {
	packageCache
	Kind  string
	Flags []string
}

// Address is where a job's pod reaches the cache.
func (c packageCache) Address() string {
	return fmt.Sprintf("http://%s.%s.svc:%d", c.Name, RunnersNamespace, c.Port)
}

var (
	goCache = packageCache{Name: "go-cache", Port: 8080, Disk: "6Gi", EvictAt: "5Gi", Upstream: goProxyURL,
		Hosts: []string{hostOf(goProxyURL), hostOf(goSumDBURL)}}
	npmCache = packageCache{Name: "npm-cache", Port: 8080, Disk: "4Gi", EvictAt: "3Gi", Upstream: npmURL,
		Hosts: []string{hostOf(npmURL)}}
	imageCache = packageCache{Name: "image-cache", Port: 5000, Disk: "12Gi", EvictAt: "10Gi", Upstream: dockerHubURL,
		Hosts: append([]string{hostOf(dockerHubURL)}, dockerHubBlobHosts...)}
)

func hostOf(upstream string) string { return strings.TrimPrefix(upstream, "https://") }

// RunnerCaches names each cache's Deployment, whose health Argo CD reads.
var RunnerCaches = []string{goCache.Name, npmCache.Name, imageCache.Name}

// runnerCaches renders the caches on cacheImage's package-cache, each reaching its upstreams' hosts alone.
func runnerCaches(cacheImage string) (Manifest, error) {
	if _, err := ParseImage(cacheImage); err != nil {
		return Manifest{}, fmt.Errorf("the runner caches' cache image: %w", err)
	}
	out, err := tmpl.Execute(caches, map[string]any{
		"Namespace":     RunnersNamespace,
		"BurstLabel":    BurstLabel,
		"CacheImage":    cacheImage,
		"RegistryImage": ImageCacheImage,
		"MaxAge":        cacheMaxAge,
		"PolicyName":    RunnerCachesName + "-flows",
		"Servers": []cacheServer{
			{packageCache: goCache, Kind: "go", Flags: []string{"--checksum-db=" + goSumDBURL, "--checksum-db-key=" + goSumDBKey}},
			{packageCache: npmCache, Kind: "npm"},
		},
		"Image":  imageCache,
		"Caches": []packageCache{goCache, npmCache, imageCache},
	})
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Name: RunnerCachesName, Path: "components/runners/caches.yaml", Bytes: out}, nil
}

// runnerCacheEnv is what each job's pod takes the caches by: the go command falls back past a Go cache that fails, and
// Docker past a mirror that fails, while npm has no fallback.
func runnerCacheEnv() map[string]string {
	return map[string]string{
		"GoProxy":        goCache.Address() + "|" + goProxyURL + ",direct",
		"NPMRegistry":    npmCache.Address() + "/",
		"RegistryMirror": imageCache.Address(),
	}
}
