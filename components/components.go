// Package components holds the pinned component set every Talos cluster starts with, and every image it pulls.
package components

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Versions are the Talos and Kubernetes releases a cluster runs.
type Versions struct {
	Talos      string
	Kubernetes string
}

// PinnedVersions are the releases this kit's image digests belong to.
var PinnedVersions = Versions{Talos: "v1.14.4", Kubernetes: "1.37.1"}

// Manifest is one file of Kubernetes objects a node applies.
type Manifest struct {
	// Name is the manifest's name in the machine configuration, a DNS-1123 label.
	Name string
	// Path is where the file sits in this repository, and so in a release of it.
	Path string
	// Bytes is the file's content.
	Bytes []byte
}

// SHA256 returns the hex SHA-256 of the manifest's bytes.
func (m Manifest) SHA256() string {
	sum := sha256.Sum256(m.Bytes)
	return hex.EncodeToString(sum[:])
}

// Set is the component set of one cluster.
type Set struct {
	Versions Versions
	// Served are the manifests too large for the machine configuration, which the gateway serves and each node fetches.
	Served []Manifest
	// Inline are the manifests the machine configuration carries.
	Inline []Manifest
	// TalosImages are the images Talos itself pulls for the Kubernetes control plane and the kubelet.
	TalosImages []Image
}

var (
	//go:embed cilium/cilium.yaml
	ciliumManifest []byte
	//go:embed cilium/policy-crd.yaml
	policyCRDManifest []byte
	//go:embed argocd/argocd.yaml
	argoCDManifest []byte
	//go:embed metadata/metadata-block.yaml
	metadataBlockManifest []byte
	//go:embed kyverno/kyverno.yaml
	kyvernoManifest []byte
	//go:embed admission/policies/mitstack-require-app-label.yaml
	requireAppLabelPolicy []byte
	//go:embed admission/policies/mitstack-generate-default-deny.yaml
	generateDefaultDenyPolicy []byte
	//go:embed cloudnative-pg/cloudnative-pg.yaml
	cloudNativePGManifest []byte
	//go:embed cloudnative-pg/namespace.yaml
	cloudNativePGNamespace []byte
	//go:embed network-policies/mitstack-database-slices.yaml
	databaseSlicesPolicy []byte
)

// Default returns the component set at the pinned versions: Cilium with the CRD of its cluster-wide policy,
// Argo CD, the policy that keeps pods from Hetzner's metadata service, Kyverno with the policies that require
// the mitstack.dev/app label and deny each app namespace's traffic by default, and the CloudNativePG operator
// with the policy that opens a database slice's Postgres to the other slices.
func Default() Set {
	return Set{
		Versions: PinnedVersions,
		Served: []Manifest{
			{Name: "cilium", Path: "components/cilium/cilium.yaml", Bytes: ciliumManifest},
			{Name: "cilium-policy-crd", Path: "components/cilium/policy-crd.yaml", Bytes: policyCRDManifest},
			{Name: "argocd", Path: "components/argocd/argocd.yaml", Bytes: argoCDManifest},
			{Name: "kyverno", Path: "components/kyverno/kyverno.yaml", Bytes: kyvernoManifest},
			{Name: "cloudnative-pg", Path: "components/cloudnative-pg/cloudnative-pg.yaml", Bytes: cloudNativePGManifest},
		},
		Inline: []Manifest{
			{Name: "mitstack-metadata-block", Path: "components/metadata/metadata-block.yaml", Bytes: metadataBlockManifest},
			{Name: "mitstack-require-app-label", Path: "components/admission/policies/mitstack-require-app-label.yaml", Bytes: requireAppLabelPolicy},
			{Name: "mitstack-generate-default-deny", Path: "components/admission/policies/mitstack-generate-default-deny.yaml", Bytes: generateDefaultDenyPolicy},
			{Name: "cloudnative-pg-namespace", Path: "components/cloudnative-pg/namespace.yaml", Bytes: cloudNativePGNamespace},
			{Name: "mitstack-database-slices", Path: "components/network-policies/mitstack-database-slices.yaml", Bytes: databaseSlicesPolicy},
		},
		TalosImages: pinnedTalosImages,
	}
}

// imageLineRe matches a manifest's image field, or an operator's default image such as CloudNativePG's
// POSTGRES_IMAGE_NAME, with its reference in group 1.
var imageLineRe = regexp.MustCompile(`(?m)^[ \t]*(?:- )?(?:image|[A-Z][A-Z_]*_IMAGE_NAME):[ \t]*"?([^"\s]+)"?[ \t]*$`)

// Images returns every image the set and the node-local Collector pull, sorted by reference with each listed once.
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
