package components

import (
	"fmt"
	"regexp"
	"strings"
)

// Image is a container image named by registry, repository, tag and digest.
type Image struct {
	// Name is the registry host and repository, as quay.io/cilium/cilium.
	Name string
	Tag  string
	// Digest is the manifest digest, as sha256: followed by 64 hex characters.
	Digest string
}

// Ref returns the image as name:tag@digest, the form a node pulls and the mirror stores.
func (i Image) Ref() string {
	return i.Name + ":" + i.Tag + "@" + i.Digest
}

// Registry returns the registry host the image's name starts with.
func (i Image) Registry() string {
	host, _, _ := strings.Cut(i.Name, "/")
	return host
}

// Repository returns the image's path within its registry.
func (i Image) Repository() string {
	_, repository, _ := strings.Cut(i.Name, "/")
	return repository
}

// imageRefRe matches registry/repository:tag@sha256:digest, with a registry host that holds a dot.
var imageRefRe = regexp.MustCompile(`^([a-z0-9-]+(?:\.[a-z0-9-]+)+/[a-z0-9._/-]+):([A-Za-z0-9._-]+)@(sha256:[0-9a-f]{64})$`)

// ParseImage reads a fully qualified reference that names its tag and its digest.
func ParseImage(ref string) (Image, error) {
	m := imageRefRe.FindStringSubmatch(ref)
	if m == nil {
		return Image{}, fmt.Errorf("image %q is not registry/repository:tag@sha256:digest", ref)
	}
	return Image{Name: m[1], Tag: m[2], Digest: m[3]}, nil
}

// MirroredRegistries are the public registries a node's configuration sends to the private mirror.
var MirroredRegistries = []string{"docker.io", "ghcr.io", "public.ecr.aws", "quay.io", "registry.k8s.io"}

// pinnedTalosImages are the images Talos v1.14.4 pulls for Kubernetes 1.37.1, by digest.
var pinnedTalosImages = []Image{
	{Name: "ghcr.io/siderolabs/kubelet", Tag: "v1.37.1", Digest: "sha256:3777bdaaa352e1f425dff9d8868a970923c66d0629bad16f935c41018a83d9d8"},
	{Name: "registry.k8s.io/coredns/coredns", Tag: "v1.14.7", Digest: "sha256:7efd3c635b03efd68c4e8398fc45f0d993d0e9ab016f72c1cefb0fd6d01aa286"},
	{Name: "registry.k8s.io/etcd", Tag: "v3.7.2", Digest: "sha256:7c6c239825d00e3f6328a69caafd54be92063acf0c2ce78b8394699f52b75dc3"},
	{Name: "registry.k8s.io/kube-apiserver", Tag: "v1.37.1", Digest: "sha256:e190f914a6cc21cab9268485d1ae7282fdeab1315ac64b668aa42d732fd1061d"},
	{Name: "registry.k8s.io/kube-controller-manager", Tag: "v1.37.1", Digest: "sha256:d470c1b85aebb466e971c06239cc6a31a4ac337a78600cef80375a79a1be1641"},
	{Name: "registry.k8s.io/kube-scheduler", Tag: "v1.37.1", Digest: "sha256:b6e2474a6c20309f0df1e7337364281fda95c6a0722259ba7f032a1e177d3f29"},
	{Name: "registry.k8s.io/pause", Tag: "3.10.2", Digest: "sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"},
}
