package testutil

import (
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

	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
)

// AppKey is the fixture GitHub App key HostingInputs seals; nothing accepts it.
const AppKey = "-----BEGIN RSA PRIVATE KEY-----\nfixture-github-app-key-for-tests-only\n-----END RSA PRIVATE KEY-----\n"

// HostingNodes are the fixture cluster's three node names.
var HostingNodes = []string{"shop-node-fsn1", "shop-node-nbg1", "shop-node-hel1"}

// hostingKeyPair is minted once per test binary, since an RSA key takes the longest of any fixture to make.
var hostingKeyPair struct {
	sync.Once
	key             *rsa.PrivateKey
	keyPEM, certPEM []byte
}

// HostingInputs returns throwaway hosting inputs: a seal key, a Sealed Secrets key pair and AppKey sealed to it.
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
	}
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
