package testutil

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
)

// AppKey is the fixture GitHub App key HostingInputs seals; nothing accepts it.
const AppKey = "-----BEGIN RSA PRIVATE KEY-----\nfixture-github-app-key-for-tests-only\n-----END RSA PRIVATE KEY-----\n"

// HostingImages are the images the hosting set's charts pull, each by the digest its registry serves for the tag.
var HostingImages = []string{
	"quay.io/openbao/openbao:2.7.1@sha256:6d2b93856e3fcf7b18ad855a0b51eaba474dc8b79cf554379ea32034797d2acf",
	"quay.io/jetstack/cert-manager-controller:v1.21.2@sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8",
	"quay.io/jetstack/cert-manager-webhook:v1.21.2@sha256:a60e2dac46dbb8a7f3df95c54ce941012f54c2fe022f0ee55aaa1ab40ed957ae",
	"quay.io/jetstack/cert-manager-cainjector:v1.21.2@sha256:c85268c64f2e0e76684bf5fe8906caff34b82523561c6affe0fae3546bd87562",
	"quay.io/jetstack/cert-manager-acmesolver:v1.21.2@sha256:699b40d622211ab7accad8a21b04c5fbaa1841ef7a12621e8de492dbe27b2503",
	"quay.io/jetstack/trust-manager:v0.25.0@sha256:0521921c2a46d14d0417336b20bcde69215ef3164f2977cc939d53c6e5224a17",
	"ghcr.io/external-secrets/external-secrets:v2.11.0@sha256:66fb710878cbf3eba4a927e35c5d75e29a202c9bab238aad64bfb23b786b962b",
	"docker.io/bitnami/sealed-secrets-controller:0.40.0@sha256:b1ff382e9300dc9e74991f3177b18cafc06ce43b5c21349c86adbdd1d1c177d3",
	"docker.io/traefik:v3.7.13@sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0",
}

// HostingNodes are the fixture cluster's three node names.
var HostingNodes = []string{"shop-node-fsn1", "shop-node-nbg1", "shop-node-hel1"}

// hostingKeyPair is minted once per test binary, since an RSA key takes the longest of any fixture to make.
var hostingKeyPair struct {
	sync.Once
	key             *rsa.PrivateKey
	keyPEM, certPEM []byte
}

// HostingInputs returns throwaway hosting inputs: a seal key, a Sealed Secrets key pair and AppKey sealed to it, and
// InitInputs for OpenBao's init Job.
func HostingInputs(t *testing.T) components.HostingInputs {
	t.Helper()
	sealKey := make([]byte, components.SealKeyBytes)
	_, err := rand.Read(sealKey)
	require.NoError(t, err)
	hostingKeyPair.Do(func() { hostingKeyPair.key, hostingKeyPair.keyPEM, hostingKeyPair.certPEM = SealedSecretsKeyPair(t) })
	key, keyPEM, certPEM := hostingKeyPair.key, hostingKeyPair.keyPEM, hostingKeyPair.certPEM
	return components.HostingInputs{
		SealKey:              sealKey,
		SealedSecretsKeyPEM:  keyPEM,
		SealedSecretsCertPEM: certPEM,
		SealedAppKey:         SealedAppKey(t, &key.PublicKey, AppKey),
		Nodes:                HostingNodes,
		Init:                 InitInputs(t),
	}
}

// The fixture cluster repository and the hosting GitHub App installed on it.
const (
	InitRepository     = "mitstack-eu/shop-cluster"
	InitAppID          = "1234567"
	InitInstallationID = "89012345"
)

// InitInputs returns throwaway inputs for OpenBao's init Job, encrypting to Recipient's key.
func InitInputs(t *testing.T) openbaoinit.Inputs {
	t.Helper()
	_, armored := Recipient(t)
	return openbaoinit.Inputs{
		ToolsImage:     ToolsImage,
		Repository:     InitRepository,
		AppID:          InitAppID,
		InstallationID: InitInstallationID,
		Recipient:      armored,
	}
}

// recipientKey is minted once per test binary, since an OpenPGP key takes about as long to make as an RSA one.
var recipientKey struct {
	sync.Once
	entity  *openpgp.Entity
	armored []byte
}

// Recipient returns a throwaway OpenPGP key, which decrypts what the init Job encrypts, and its armored public key.
func Recipient(t *testing.T) (*openpgp.Entity, []byte) {
	t.Helper()
	recipientKey.Do(func() {
		entity, err := openpgp.NewEntity("Shop cluster owner", "fixture", "owner@example.com", nil)
		require.NoError(t, err)
		recipientKey.entity, recipientKey.armored = entity, ArmoredPublicKey(t, entity)
	})
	return recipientKey.entity, recipientKey.armored
}

// ArmoredPublicKey returns the ASCII-armored public key of entity, as `gpg --armor --export` writes one.
func ArmoredPublicKey(t *testing.T, entity *openpgp.Entity) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := armor.Encode(&out, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.Serialize(w))
	require.NoError(t, w.Close())
	return append(out.Bytes(), '\n')
}

// SealedSecretsKeyPair mints a throwaway RSA key and the self-signed certificate Sealed Secrets seals to.
func SealedSecretsKeyPair(t *testing.T) (*rsa.PrivateKey, []byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sealed-secret"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return key, keyPEM, certPEM
}

// SealedAppKey seals value to pub as kubeseal does in its strict scope, and returns the SealedSecret manifest.
func SealedAppKey(t *testing.T, pub *rsa.PublicKey, value string) []byte {
	t.Helper()
	sessionKey := make([]byte, 32)
	_, err := rand.Read(sessionKey)
	require.NoError(t, err)
	label := []byte(components.SealedAppKeyNamespace + "/" + components.SealedAppKeyName)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, sessionKey, label)
	require.NoError(t, err)
	block, err := aes.NewCipher(sessionKey)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	sealed := binary.BigEndian.AppendUint16(nil, uint16(len(wrapped)))
	sealed = append(append(sealed, wrapped...), gcm.Seal(nil, make([]byte, gcm.NonceSize()), []byte(value), nil)...)
	return fmt.Appendf(nil, `apiVersion: bitnami.com/v1alpha1
kind: SealedSecret
metadata:
  name: %s
  namespace: %s
spec:
  encryptedData:
    %s: %s
`, components.SealedAppKeyName, components.SealedAppKeyNamespace, components.SealedAppKeyItem, base64.StdEncoding.EncodeToString(sealed))
}
