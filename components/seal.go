package components

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

// SealedSecretsKeyBits matches the key the Sealed Secrets controller mints for itself.
const SealedSecretsKeyBits = 4096

const sealedSecretsCertValidity = 10 * 365 * 24 * time.Hour

// NewSealedSecretsKeyPair mints the RSA key Sealed Secrets opens with and its self-signed certificate, as PKCS #1 and
// X.509 PEM.
func NewSealedSecretsKeyPair(now time.Time) (keyPEM, certPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, SealedSecretsKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generating the Sealed Secrets key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("drawing the Sealed Secrets certificate's serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sealed-secret", Organization: []string{"sealed-secret"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(sealedSecretsCertValidity),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("signing the Sealed Secrets certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// Seal seals value as kubeseal does in its strict scope, so only the Secret namespace/name opens it, and returns
// the SealedSecret manifest holding it as item.
func Seal(certPEM []byte, namespace, name, item string, value []byte) ([]byte, error) {
	sealed, err := SealValue(certPEM, namespace, name, value)
	if err != nil {
		return nil, err
	}
	var s sealedSecret
	s.APIVersion, s.Kind = "bitnami.com/v1alpha1", "SealedSecret"
	s.Metadata.Name, s.Metadata.Namespace = name, namespace
	s.Spec.EncryptedData = map[string]string{item: sealed}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encoding the SealedSecret %s/%s: %w", namespace, name, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding the SealedSecret %s/%s: %w", namespace, name, err)
	}
	return out.Bytes(), nil
}

// SealValue seals value as kubeseal does in its strict scope, and returns the base64 value a SealedSecret's
// encryptedData holds.
func SealValue(certPEM []byte, namespace, name string, value []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("the Sealed Secrets certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("the Sealed Secrets certificate does not parse: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("the Sealed Secrets certificate holds no RSA key")
	}
	sessionKey := make([]byte, 32)
	if _, err := rand.Read(sessionKey); err != nil {
		return "", fmt.Errorf("drawing a session key: %w", err)
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, sessionKey, []byte(namespace+"/"+name))
	if err != nil {
		return "", fmt.Errorf("wrapping the session key: %w", err)
	}
	gcm, err := sessionCipher(sessionKey)
	if err != nil {
		return "", fmt.Errorf("the %w", err)
	}
	// The session key seals one value only, so the zero nonce the controller expects is never reused.
	sealed := binary.BigEndian.AppendUint16(nil, uint16(len(wrapped)))
	sealed = append(append(sealed, wrapped...), gcm.Seal(nil, make([]byte, gcm.NonceSize()), value, nil)...)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open opens a value SealValue sealed for the Secret namespace/name, as the controller holding keyPEM does.
func Open(keyPEM []byte, namespace, name, sealed string) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("the Sealed Secrets key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the Sealed Secrets key is not a PKCS #1 RSA key: %w", err)
	}
	return openValue(key, namespace, name, sealed, "the value sealed for "+namespace+"/"+name)
}

// openValue decrypts as the controller does: a length-prefixed RSA-OAEP session key labelled namespace/name, then
// AES-GCM. Its errors call the value what.
func openValue(key *rsa.PrivateKey, namespace, name, sealed, what string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < 2 || len(raw) < 2+int(binary.BigEndian.Uint16(raw)) {
		return nil, fmt.Errorf("%s's value is not a sealed value", what)
	}
	n := 2 + int(binary.BigEndian.Uint16(raw))
	sessionKey, err := rsa.DecryptOAEP(sha256.New(), nil, key, raw[2:n], []byte(namespace+"/"+name))
	if err != nil {
		return nil, fmt.Errorf("%s is not sealed to the Sealed Secrets key pair", what)
	}
	gcm, err := sessionCipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("%s's %w", what, err)
	}
	plain, err := gcm.Open(nil, make([]byte, gcm.NonceSize()), raw[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("%s does not open with its session key", what)
	}
	return plain, nil
}

func sessionCipher(sessionKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	return gcm, nil
}

// DeployKey is an SSH key pair: GitHub lists AuthorizedKey on one repository, and PrivateKeyPEM is sealed into a cluster.
type DeployKey struct {
	AuthorizedKey string
	PrivateKeyPEM []byte
}

// NewDeployKey mints an Ed25519 key, its public half in authorized_keys form and its private half as OpenSSH PEM.
func NewDeployKey(comment string) (DeployKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return DeployKey{}, fmt.Errorf("generating the deploy key %q: %w", comment, err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return DeployKey{}, fmt.Errorf("encoding the deploy key %q: %w", comment, err)
	}
	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return DeployKey{}, fmt.Errorf("encoding the deploy key %q: %w", comment, err)
	}
	return DeployKey{
		AuthorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))),
		PrivateKeyPEM: pem.EncodeToMemory(block),
	}, nil
}

// githubKnownHosts are the host keys Argo CD's own known hosts hold for GitHub's SSH endpoint on port 443, so the
// init Job trusts exactly what Argo CD trusts.
var githubKnownHosts = func() string {
	argocd, err := defaultFiles.ReadFile("argocd/argocd.yaml")
	if err != nil {
		panic(err)
	}
	var lines []string
	for line := range strings.Lines(string(argocd)) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "[ssh.github.com]:443 ") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		panic("Argo CD's known hosts hold no key for ssh.github.com on port 443")
	}
	return strings.Join(lines, "\n") + "\n"
}()
