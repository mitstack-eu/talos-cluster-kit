// Package components holds the pinned component set every Talos cluster starts with, and every image it pulls.
package components

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

type Versions struct {
	Talos      string
	Kubernetes string
}

// PinnedVersions are the releases this kit's image digests belong to.
var PinnedVersions = Versions{Talos: "v1.14.4", Kubernetes: "1.37.1"}

type Manifest struct {
	Name  string
	Path  string
	Bytes []byte
}

func (m Manifest) SHA256() string {
	sum := sha256.Sum256(m.Bytes)
	return hex.EncodeToString(sum[:])
}

type Set struct {
	Versions Versions
	// Served are the manifests too large for the machine configuration, which the gateway serves and each node fetches.
	Served      []Manifest
	Inline      []Manifest
	TalosImages []Image
	UserVolumes []string
}

//go:embed admission/policies argocd cilium cloudnative-pg istio kyverno local-path-provisioner mesh metadata network-policies
var defaultFiles embed.FS

const LocalPathVolume = "local-path-provisioner"

func Default() Set {
	return Set{
		Versions: PinnedVersions,
		Served: []Manifest{
			defaultManifest("cilium", "cilium/cilium.yaml"),
			defaultManifest("cilium-policy-crd", "cilium/policy-crd.yaml"),
			defaultManifest("argocd", "argocd/argocd.yaml"),
			defaultManifest("kyverno", "kyverno/kyverno.yaml"),
			defaultManifest("cloudnative-pg", "cloudnative-pg/cloudnative-pg.yaml"),
			defaultManifest("local-path-provisioner", "local-path-provisioner/local-path-provisioner.yaml"),
			defaultManifest("istio-base", "istio/istio-base.yaml"),
			defaultManifest("istiod", "istio/istiod.yaml"),
			defaultManifest("istio-cni", "istio/istio-cni.yaml"),
			defaultManifest("ztunnel", "istio/ztunnel.yaml"),
			defaultManifest("mitstack-mesh-probe", "mesh/probe.yaml"),
		},
		Inline: []Manifest{
			defaultManifest("mitstack-metadata-block", "metadata/metadata-block.yaml"),
			defaultManifest("mitstack-require-app-label", "admission/policies/mitstack-require-app-label.yaml"),
			defaultManifest("mitstack-generate-default-deny", "admission/policies/mitstack-generate-default-deny.yaml"),
			defaultManifest("cloudnative-pg-namespace", "cloudnative-pg/namespace.yaml"),
			defaultManifest("mitstack-database-slices", "network-policies/mitstack-database-slices.yaml"),
			defaultManifest("local-path-provisioner-namespace", "local-path-provisioner/namespace.yaml"),
			defaultManifest("mitstack-local-path-flows", "local-path-provisioner/flows.yaml"),
			defaultManifest("mitstack-strict-mtls", "mesh/strict-mtls.yaml"),
			defaultManifest("mitstack-ambient-probes", "mesh/probes.yaml"),
			defaultManifest("mitstack-mesh-cloudnative-pg", "mesh/cloudnative-pg.yaml"),
			defaultManifest("mitstack-mesh-argocd", "mesh/argocd.yaml"),
			defaultManifest("mitstack-require-mesh", "admission/policies/mitstack-require-mesh.yaml"),
			defaultManifest("mitstack-require-strict-mtls", "admission/policies/mitstack-require-strict-mtls.yaml"),
			defaultManifest("mitstack-disallow-host-path", "admission/policies/mitstack-disallow-host-path.yaml"),
			defaultManifest("mitstack-require-resource-limits", "admission/policies/mitstack-require-resource-limits.yaml"),
			defaultManifest("mitstack-restrict-image-registries", "admission/policies/mitstack-restrict-image-registries.yaml"),
		},
		TalosImages: pinnedTalosImages,
		UserVolumes: []string{LocalPathVolume},
	}
}

// defaultManifest panics on a file the embed directive leaves out, which any call of Default in a test reaches first.
func defaultManifest(name, file string) Manifest {
	data, err := defaultFiles.ReadFile(file)
	if err != nil {
		panic(err)
	}
	return Manifest{Name: name, Path: "components/" + file, Bytes: data}
}

// imageLineRe matches an image field, an operator's *_IMAGE_NAME default, a plugin's *_IMAGE default or an --*-image=
// flag, its reference in group 1.
var imageLineRe = regexp.MustCompile(`(?m)^[ \t]*(?:- )?(?:(?:image|[A-Z][A-Z_]*_IMAGE(?:_NAME)?):[ \t]*|--[a-z0-9-]+-image=)"?([^"\s]+)"?[ \t]*$`)

// manifestImages keeps the images of each manifest under its name with the hash of the bytes they were read from, so it
// holds one slot per manifest name however many contents a name has had, such as an install's enrolment token. Scanning a
// manifest takes the better part of a second under the race detector and a cluster set differs from the last in few of its
// manifests.
var manifestImages sync.Map

type scanned struct {
	hash   [sha256.Size]byte
	images []Image
}

func (m Manifest) images() ([]Image, error) {
	hash := sha256.Sum256(m.Bytes)
	if cached, ok := manifestImages.Load(m.Name); ok && cached.(scanned).hash == hash {
		return cached.(scanned).images, nil
	}
	var images []Image
	for _, match := range imageLineRe.FindAllSubmatch(m.Bytes, -1) {
		image, err := ParseImage(string(match[1]))
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", m.Name, err)
		}
		images = append(images, image)
	}
	manifestImages.Store(m.Name, scanned{hash: hash, images: images})
	return images, nil
}

func (s Set) Images() ([]Image, error) {
	images := append(slices.Clone(s.TalosImages), logCollectorImage)
	for _, m := range slices.Concat(s.Served, s.Inline) {
		found, err := m.images()
		if err != nil {
			return nil, err
		}
		images = append(images, found...)
	}
	slices.SortFunc(images, func(a, b Image) int { return strings.Compare(a.Ref(), b.Ref()) })
	return slices.Compact(images), nil
}
