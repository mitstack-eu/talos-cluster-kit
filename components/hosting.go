package components

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
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
)

// OpenBaoVolume is the Talos user volume each node keeps its OpenBao member's Raft data on.
const OpenBaoVolume = "openbao"

// OpenBaoMembers is how many Raft members OpenBao runs, one per node, since a user volume's data is lost with its machine.
const OpenBaoMembers = 3

// SealKeyBytes is the length of the key OpenBao's static seal takes, an AES-256-GCM-96 key.
const SealKeyBytes = 32

// The SealedSecret the hosting GitHub App's key is sealed into, in kubeseal's strict scope.
const (
	SealedAppKeyNamespace = "mitstack-node"
	SealedAppKeyName      = "github-app-key"
	SealedAppKeyItem      = "private-key"
)

// UserVolumePath is where Talos mounts the user volume name on every node.
func UserVolumePath(name string) string {
	return "/var/mnt/" + name
}

// HostingInputs are the secrets a cluster that hosts the platform boots with, and the nodes that keep OpenBao's data.
type HostingInputs struct {
	// SealKey is the key OpenBao's static seal unseals with, SealKeyBytes long.
	SealKey []byte
	// SealedSecretsKeyPEM and SealedSecretsCertPEM are the RSA key pair Sealed Secrets opens with, as PKCS #1 and X.509 PEM.
	SealedSecretsKeyPEM, SealedSecretsCertPEM []byte
	// SealedAppKey is the SealedSecret manifest that opens to the hosting GitHub App's key, sealed to that pair.
	SealedAppKey []byte
	// Nodes are the cluster's node names, one for each OpenBao member.
	Nodes []string
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
	//go:embed hosting/seal-key.yaml
	sealKeyTemplate string
	//go:embed hosting/sealed-secrets-key.yaml
	sealedSecretsKeyTemplate string
	//go:embed hosting/sealed-app-key.yaml
	sealedAppKeyTemplate string
	//go:embed hosting/storage.yaml
	storageTemplate string
)

var (
	sealKey          = tmpl.Must("OpenBao's seal key", sealKeyTemplate)
	sealedSecretsKey = tmpl.Must("the Sealed Secrets key pair", sealedSecretsKeyTemplate)
	sealedAppKey     = tmpl.Must("the sealed App key", sealedAppKeyTemplate)
	storage          = tmpl.Must("OpenBao's volumes", storageTemplate)
)

// Hosting returns the default set with the boot set of a cluster that hosts the platform on top: OpenBao, cert-manager,
// trust-manager, External Secrets, Sealed Secrets and Traefik. Every manifest that holds one of in's secrets is inline,
// since the gateway serves its manifests to every node.
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
	)
	set.Inline = append(slices.Clone(set.Inline), inline...)
	set.UserVolumes = []string{OpenBaoVolume}
	return set, nil
}

// inlineManifests renders the manifests that hold in's secrets, with sealed the App key's checked sealed value, and
// OpenBao's volumes, which name in's nodes.
func (in HostingInputs) inlineManifests(sealed string) ([]Manifest, error) {
	parts := []struct {
		name, path string
		t          *template.Template
		data       any
	}{
		{"mitstack-openbao-seal-key", "components/hosting/seal-key.yaml", sealKey, map[string]string{"Key": base64.StdEncoding.EncodeToString(in.SealKey)}},
		{"mitstack-sealed-secrets-key", "components/hosting/sealed-secrets-key.yaml", sealedSecretsKey, map[string]string{"Cert": string(in.SealedSecretsCertPEM), "Key": string(in.SealedSecretsKeyPEM)}},
		{"mitstack-sealed-app-key", "components/hosting/sealed-app-key.yaml", sealedAppKey, map[string]string{
			"Namespace": SealedAppKeyNamespace, "Name": SealedAppKeyName, "Item": SealedAppKeyItem, "Sealed": sealed,
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
	return manifests, nil
}

// validate returns the sealed value of in's App key once every input is one the set can boot from.
func (in HostingInputs) validate() (string, error) {
	var errs []error
	if len(in.SealKey) != SealKeyBytes {
		errs = append(errs, fmt.Errorf("the seal key is %d bytes, want %d", len(in.SealKey), SealKeyBytes))
	}
	key, err := sealedSecretsKeyPair(in.SealedSecretsKeyPEM, in.SealedSecretsCertPEM)
	if err != nil {
		errs = append(errs, err)
	}
	sealed, err := readSealedAppKey(in.SealedAppKey)
	if err != nil {
		errs = append(errs, err)
	} else if key != nil {
		if err := opens(key, sealed); err != nil {
			errs = append(errs, err)
		}
	}
	if len(in.Nodes) != OpenBaoMembers {
		errs = append(errs, fmt.Errorf("%d nodes are named, want one for each of OpenBao's %d members", len(in.Nodes), OpenBaoMembers))
	}
	for _, node := range in.Nodes {
		if err := check.DNS1123Label(node); err != nil {
			errs = append(errs, fmt.Errorf("node name: %w", err))
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(in.Nodes)))) != len(in.Nodes) {
		errs = append(errs, errors.New("a node is named twice"))
	}
	return sealed, errors.Join(errs...)
}

// sealedSecretsKeyPair returns keyPEM's RSA key once certPEM is shown to certify it.
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

// sealedSecret is the part of a SealedSecret the hosting set reads.
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

// readSealedAppKey returns the sealed value of the App key's one SealedSecret, which is all the set renders of it,
// since Talos applies inline manifests with cluster-admin rights.
func readSealedAppKey(manifest []byte) (string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	var s sealedSecret
	if err := dec.Decode(&s); err != nil {
		return "", fmt.Errorf("the sealed App key is not YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("the sealed App key holds more than one document")
	}
	if s.APIVersion != "bitnami.com/v1alpha1" || s.Kind != "SealedSecret" || s.Metadata.Name != SealedAppKeyName || s.Metadata.Namespace != SealedAppKeyNamespace {
		return "", fmt.Errorf("the sealed App key is %s %s %s/%s, want bitnami.com/v1alpha1 SealedSecret %s/%s",
			s.APIVersion, s.Kind, s.Metadata.Namespace, s.Metadata.Name, SealedAppKeyNamespace, SealedAppKeyName)
	}
	sealed := s.Spec.EncryptedData[SealedAppKeyItem]
	if len(s.Spec.EncryptedData) != 1 || !encryptedDataRe.MatchString(sealed) {
		return "", fmt.Errorf("the sealed App key's encryptedData holds %d items, want only %s, sealed", len(s.Spec.EncryptedData), SealedAppKeyItem)
	}
	return sealed, nil
}

// opens fails unless key opens the App key's sealed value as the controller does: a length-prefixed RSA-OAEP session
// key labelled with the SealedSecret's namespace and name, then AES-GCM with a zero nonce.
func opens(key *rsa.PrivateKey, sealed string) error {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < 2 || len(raw) < 2+int(binary.BigEndian.Uint16(raw)) {
		return errors.New("the sealed App key's value is not a sealed value")
	}
	n := 2 + int(binary.BigEndian.Uint16(raw))
	label := []byte(SealedAppKeyNamespace + "/" + SealedAppKeyName)
	sessionKey, err := rsa.DecryptOAEP(sha256.New(), nil, key, raw[2:n], label)
	if err != nil {
		return errors.New("the sealed App key is not sealed to the Sealed Secrets key pair")
	}
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return fmt.Errorf("the sealed App key's session key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("the sealed App key's session key: %w", err)
	}
	if _, err := gcm.Open(nil, make([]byte, gcm.NonceSize()), raw[n:], nil); err != nil {
		return errors.New("the sealed App key does not open with its session key")
	}
	return nil
}
