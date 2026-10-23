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

//go:embed admission/policies argocd cilium cloudnative-pg kyverno local-path-provisioner metadata network-policies
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
		},
		Inline: []Manifest{
			defaultManifest("mitstack-metadata-block", "metadata/metadata-block.yaml"),
			defaultManifest("mitstack-require-app-label", "admission/policies/mitstack-require-app-label.yaml"),
			defaultManifest("mitstack-generate-default-deny", "admission/policies/mitstack-generate-default-deny.yaml"),
			defaultManifest("cloudnative-pg-namespace", "cloudnative-pg/namespace.yaml"),
			defaultManifest("mitstack-database-slices", "network-policies/mitstack-database-slices.yaml"),
			defaultManifest("local-path-provisioner-namespace", "local-path-provisioner/namespace.yaml"),
			defaultManifest("mitstack-local-path-flows", "local-path-provisioner/flows.yaml"),
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

// imageLineRe matches an image field, an operator's *_IMAGE_NAME default or an --*-image= flag, its reference in group 1.
var imageLineRe = regexp.MustCompile(`(?m)^[ \t]*(?:- )?(?:(?:image|[A-Z][A-Z_]*_IMAGE_NAME):[ \t]*|--[a-z0-9-]+-image=)"?([^"\s]+)"?[ \t]*$`)

func (s Set) Images() ([]Image, error) {
	images := append(slices.Clone(s.TalosImages), logCollectorImage)
	for _, m := range slices.Concat(s.Served, s.Inline) {
		for _, match := range imageLineRe.FindAllSubmatch(m.Bytes, -1) {
			image, err := ParseImage(string(match[1]))
			if err != nil {
				return nil, fmt.Errorf("manifest %s: %w", m.Name, err)
			}
			images = append(images, image)
		}
	}
	slices.SortFunc(images, func(a, b Image) int { return strings.Compare(a.Ref(), b.Ref()) })
	return slices.Compact(images), nil
}
