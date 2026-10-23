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
	// UserVolumes are the Talos user volumes the set's components keep data on, each mounted at UserVolumePath.
	UserVolumes []string
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
	//go:embed local-path-provisioner/local-path-provisioner.yaml
	localPathProvisionerManifest []byte
	//go:embed local-path-provisioner/namespace.yaml
	localPathNamespace []byte
	//go:embed local-path-provisioner/flows.yaml
	localPathFlowsPolicy []byte
)

// LocalPathVolume is the user volume the default StorageClass provisions each claim's directory on.
const LocalPathVolume = "local-path-provisioner"

// Default returns the component set at the pinned versions, from Cilium and Argo CD to Kyverno's admission
// policies, the CloudNativePG operator and the local path provisioner behind the default StorageClass.
func Default() Set {
	return Set{
		Versions: PinnedVersions,
		Served: []Manifest{
			{Name: "cilium", Path: "components/cilium/cilium.yaml", Bytes: ciliumManifest},
			{Name: "cilium-policy-crd", Path: "components/cilium/policy-crd.yaml", Bytes: policyCRDManifest},
			{Name: "argocd", Path: "components/argocd/argocd.yaml", Bytes: argoCDManifest},
			{Name: "kyverno", Path: "components/kyverno/kyverno.yaml", Bytes: kyvernoManifest},
			{Name: "cloudnative-pg", Path: "components/cloudnative-pg/cloudnative-pg.yaml", Bytes: cloudNativePGManifest},
			{Name: "local-path-provisioner", Path: "components/local-path-provisioner/local-path-provisioner.yaml", Bytes: localPathProvisionerManifest},
		},
		Inline: []Manifest{
			{Name: "mitstack-metadata-block", Path: "components/metadata/metadata-block.yaml", Bytes: metadataBlockManifest},
			{Name: "mitstack-require-app-label", Path: "components/admission/policies/mitstack-require-app-label.yaml", Bytes: requireAppLabelPolicy},
			{Name: "mitstack-generate-default-deny", Path: "components/admission/policies/mitstack-generate-default-deny.yaml", Bytes: generateDefaultDenyPolicy},
			{Name: "cloudnative-pg-namespace", Path: "components/cloudnative-pg/namespace.yaml", Bytes: cloudNativePGNamespace},
			{Name: "mitstack-database-slices", Path: "components/network-policies/mitstack-database-slices.yaml", Bytes: databaseSlicesPolicy},
			{Name: "local-path-provisioner-namespace", Path: "components/local-path-provisioner/namespace.yaml", Bytes: localPathNamespace},
			{Name: "mitstack-local-path-flows", Path: "components/local-path-provisioner/flows.yaml", Bytes: localPathFlowsPolicy},
		},
		TalosImages: pinnedTalosImages,
		UserVolumes: []string{LocalPathVolume},
	}
}

// imageLineRe matches a manifest's image field, an operator's default image such as CloudNativePG's
// POSTGRES_IMAGE_NAME, or an image flag such as cert-manager's --acme-http01-solver-image, with its reference in group 1.
var imageLineRe = regexp.MustCompile(`(?m)^[ \t]*(?:- )?(?:(?:image|[A-Z][A-Z_]*_IMAGE_NAME):[ \t]*|--[a-z0-9-]+-image=)"?([^"\s]+)"?[ \t]*$`)

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
