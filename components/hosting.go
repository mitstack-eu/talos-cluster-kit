package components

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"text/template"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
)

const OpenBaoVolume = "openbao"

// OpenBaoMembers is how many Raft members OpenBao runs, one per node, since a user volume's data is lost with its machine.
const OpenBaoMembers = 3

const SealKeyBytes = 32

// The write deploy key OpenBao's init Job pushes with.
const (
	DeployKeyNamespace = openbaoinit.DeployKeyNamespace
	DeployKeySecret    = openbaoinit.DeployKeySecret
	DeployKeyItem      = openbaoinit.DeployKeyItem
)

// The repository credential is the Secret Argo CD reads the cluster repository with, holding its read-only deploy key.
const (
	RepositoryCredentialNamespace = "argocd"
	RepositoryCredentialName      = "mitstack-cluster-repository"
	RepositoryCredentialItem      = "sshPrivateKey"
)

// RepositorySSHURL is the URL Argo CD and the init Job reach a repository at with its deploy key.
func RepositorySSHURL(repository string) string { return openbaoinit.RepositoryURL(repository) }

func UserVolumePath(name string) string {
	return "/var/mnt/" + name
}

type HostingInputs struct {
	SealKey []byte
	// SealedSecretsKeyPEM and SealedSecretsCertPEM are the RSA key pair Sealed Secrets opens with, as PKCS #1 and X.509 PEM.
	SealedSecretsKeyPEM, SealedSecretsCertPEM []byte
	// SealedWriteKey is the SealedSecret that opens to the cluster repository's write deploy key, as DeployKeyItem.
	SealedWriteKey []byte
	// SealedReadKey is the SealedSecret that opens to the cluster repository's read-only deploy key, as RepositoryCredentialItem.
	SealedReadKey []byte
	Nodes         []string
	// Init takes the cluster repository Argo CD reads; its known hosts are the ones Argo CD trusts.
	Init openbaoinit.Inputs
}

// sealedFile names the one value a block's SealedSecret file may hold, and what an error calls the file.
type sealedFile struct {
	what, namespace, name, item string
}

var (
	writeKeyFile = sealedFile{"the sealed write key", DeployKeyNamespace, DeployKeySecret, DeployKeyItem}
	readKeyFile  = sealedFile{"the sealed read key", RepositoryCredentialNamespace, RepositoryCredentialName, RepositoryCredentialItem}
)

// sealedValues are the sealed values the hosting set renders, each read from its block file.
type sealedValues struct {
	writeKey, readKey string
}

var (
	//go:embed openbao/openbao.yaml
	openBaoManifest []byte
	//go:embed cert-manager/cert-manager.yaml
	certManagerManifest []byte
	//go:embed trust-manager/trust-manager.yaml
	trustManagerManifest []byte
	//go:embed external-secrets/external-secrets.yaml
	externalSecretsManifest []byte
	//go:embed sealed-secrets/sealed-secrets.yaml
	sealedSecretsManifest []byte
	//go:embed traefik/traefik.yaml
	traefikManifest []byte
	//go:embed hosting/tls.yaml
	hostingTLSManifest []byte
	//go:embed hosting/flows.yaml
	hostingFlowsManifest []byte
	//go:embed hosting/mesh.yaml
	hostingMeshManifest []byte
	//go:embed hosting/seal-key.yaml
	sealKeyTemplate string
	//go:embed hosting/sealed-secrets-key.yaml
	sealedSecretsKeyTemplate string
	//go:embed hosting/deploy-key.yaml
	deployKeyTemplate string
	//go:embed hosting/cluster-repository.yaml
	clusterRepositoryTemplate string
	//go:embed hosting/storage.yaml
	storageTemplate string
)

var (
	sealKey           = tmpl.Must("OpenBao's seal key", sealKeyTemplate)
	sealedSecretsKey  = tmpl.Must("the Sealed Secrets key pair", sealedSecretsKeyTemplate)
	deployKey         = tmpl.Must("the sealed write key", deployKeyTemplate)
	clusterRepository = tmpl.Must("Argo CD's cluster repository", clusterRepositoryTemplate)
	storage           = tmpl.Must("OpenBao's volumes", storageTemplate)
)

// Hosting renders every manifest that holds one of in's secrets inline, since the gateway serves its manifests to every node.
func Hosting(in HostingInputs) (Set, error) {
	sealed, err := in.validate()
	if err != nil {
		return Set{}, fmt.Errorf("the hosting set: %w", err)
	}
	inline, err := in.inlineManifests(sealed)
	if err != nil {
		return Set{}, fmt.Errorf("the hosting set: %w", err)
	}
	set := Default()
	set.Served = append(slices.Clone(set.Served),
		Manifest{Name: "openbao", Path: "components/openbao/openbao.yaml", Bytes: openBaoManifest},
		Manifest{Name: "cert-manager", Path: "components/cert-manager/cert-manager.yaml", Bytes: certManagerManifest},
		Manifest{Name: "trust-manager", Path: "components/trust-manager/trust-manager.yaml", Bytes: trustManagerManifest},
		Manifest{Name: "external-secrets", Path: "components/external-secrets/external-secrets.yaml", Bytes: externalSecretsManifest},
		Manifest{Name: "sealed-secrets", Path: "components/sealed-secrets/sealed-secrets.yaml", Bytes: sealedSecretsManifest},
		Manifest{Name: "traefik", Path: "components/traefik/traefik.yaml", Bytes: traefikManifest},
		Manifest{Name: "mitstack-hosting-tls", Path: "components/hosting/tls.yaml", Bytes: hostingTLSManifest},
		Manifest{Name: "mitstack-hosting-flows", Path: "components/hosting/flows.yaml", Bytes: hostingFlowsManifest},
		Manifest{Name: "mitstack-hosting-mesh", Path: "components/hosting/mesh.yaml", Bytes: hostingMeshManifest},
	)
	set.Inline = append(slices.Clone(set.Inline), inline...)
	set.UserVolumes = append(slices.Clone(set.UserVolumes), OpenBaoVolume)
	return set, nil
}

func (in HostingInputs) inlineManifests(sealed sealedValues) ([]Manifest, error) {
	parts := []struct {
		name, path string
		t          *template.Template
		data       any
	}{
		{"mitstack-openbao-seal-key", "components/hosting/seal-key.yaml", sealKey, map[string]string{"Key": base64.StdEncoding.EncodeToString(in.SealKey)}},
		{"mitstack-sealed-secrets-key", "components/hosting/sealed-secrets-key.yaml", sealedSecretsKey, map[string]string{"Cert": string(in.SealedSecretsCertPEM), "Key": string(in.SealedSecretsKeyPEM)}},
		{"mitstack-openbao-deploy-key", "components/hosting/deploy-key.yaml", deployKey, map[string]string{
			"Namespace": DeployKeyNamespace, "Name": DeployKeySecret, "Item": DeployKeyItem, "Sealed": sealed.writeKey,
		}},
		{"mitstack-cluster-repository", "components/hosting/cluster-repository.yaml", clusterRepository, map[string]string{
			"Namespace": RepositoryCredentialNamespace, "Name": RepositoryCredentialName, "Item": RepositoryCredentialItem,
			"Sealed": sealed.readKey, "URL": RepositorySSHURL(in.Init.Repository),
		}},
		{"mitstack-openbao-storage", "components/hosting/storage.yaml", storage, map[string]any{"Nodes": in.Nodes, "Path": UserVolumePath(OpenBaoVolume)}},
	}
	manifests := make([]Manifest, 0, len(parts))
	for _, p := range parts {
		out, err := tmpl.Execute(p.t, p.data)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, Manifest{Name: p.name, Path: p.path, Bytes: out})
	}
	init := in.Init
	init.KnownHosts = githubKnownHosts
	initJob, err := openbaoinit.Manifests(init)
	if err != nil {
		return nil, fmt.Errorf("OpenBao's init Job: %w", err)
	}
	return append(manifests, Manifest{Name: "mitstack-openbao-init", Path: "openbaoinit/openbao-init.yaml", Bytes: initJob}), nil
}

func (in HostingInputs) validate() (sealedValues, error) {
	var errs []error
	if len(in.SealKey) != SealKeyBytes {
		errs = append(errs, fmt.Errorf("the seal key is %d bytes, want %d", len(in.SealKey), SealKeyBytes))
	}
	key, err := sealedSecretsKeyPair(in.SealedSecretsKeyPEM, in.SealedSecretsCertPEM)
	errs = append(errs, err)
	var sealed sealedValues
	sealed.writeKey, err = readOpenable(key, in.SealedWriteKey, writeKeyFile)
	errs = append(errs, err)
	sealed.readKey, err = readOpenable(key, in.SealedReadKey, readKeyFile)
	errs = append(errs, err)
	errs = append(errs, validateNodes(in.Nodes))
	return sealed, errors.Join(errs...)
}

func validateNodes(nodes []string) error {
	var errs []error
	if len(nodes) != OpenBaoMembers {
		errs = append(errs, fmt.Errorf("%d nodes are named, want one for each of OpenBao's %d members", len(nodes), OpenBaoMembers))
	}
	for _, node := range nodes {
		if err := check.DNS1123Label(node); err != nil {
			errs = append(errs, fmt.Errorf("node name: %w", err))
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(nodes)))) != len(nodes) {
		errs = append(errs, errors.New("a node is named twice"))
	}
	return errors.Join(errs...)
}

// readOpenable reads file's one sealed value and, once the key pair has parsed, checks that the value opens with it.
func readOpenable(key *rsa.PrivateKey, file []byte, want sealedFile) (string, error) {
	sealed, err := readSealed(file, want)
	if err != nil || key == nil {
		return sealed, err
	}
	return sealed, opens(key, sealed, want)
}

func sealedSecretsKeyPair(keyPEM, certPEM []byte) (*rsa.PrivateKey, error) {
	keyBlock, _ := pem.Decode(keyPEM)
	certBlock, _ := pem.Decode(certPEM)
	if keyBlock == nil || certBlock == nil {
		return nil, errors.New("the Sealed Secrets key or certificate is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the Sealed Secrets key is not a PKCS #1 RSA key: %w", err)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the Sealed Secrets certificate does not parse: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("the Sealed Secrets certificate certifies another key")
	}
	return key, nil
}

type sealedSecret struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		EncryptedData map[string]string `yaml:"encryptedData"`
	} `yaml:"spec"`
}

var encryptedDataRe = regexp.MustCompile(`^[A-Za-z0-9+/]+=*$`)

// readSealed renders nothing of the file but its one sealed value, since Talos applies inline manifests as cluster-admin.
func readSealed(manifest []byte, want sealedFile) (string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	var s sealedSecret
	if err := dec.Decode(&s); err != nil {
		return "", fmt.Errorf("%s is not YAML: %w", want.what, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%s holds more than one document", want.what)
	}
	if s.APIVersion != "bitnami.com/v1alpha1" || s.Kind != "SealedSecret" || s.Metadata.Name != want.name || s.Metadata.Namespace != want.namespace {
		return "", fmt.Errorf("%s is %s %s %s/%s, want bitnami.com/v1alpha1 SealedSecret %s/%s",
			want.what, s.APIVersion, s.Kind, s.Metadata.Namespace, s.Metadata.Name, want.namespace, want.name)
	}
	sealed := s.Spec.EncryptedData[want.item]
	if len(s.Spec.EncryptedData) != 1 || !encryptedDataRe.MatchString(sealed) {
		return "", fmt.Errorf("%s's encryptedData holds %d items, want only %s, sealed", want.what, len(s.Spec.EncryptedData), want.item)
	}
	return sealed, nil
}

// opens checks that sealed opens with key under want's Secret.
func opens(key *rsa.PrivateKey, sealed string, want sealedFile) error {
	_, err := openValue(key, want.namespace, want.name, sealed, want.what)
	return err
}
