// Package serve renders the user data of a short-lived Hetzner server that hands one file, once, over TLS 1.3 to
// the one address allowed to fetch it, behind a key the fetching side pins.
package serve

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
)

// Program is the serving program, run by Command.
//
//go:embed scripts/serve.py
var Program string

const (
	DefaultPort = 8443
	// Dir holds every file the server writes, so one directory is all it serves from.
	Dir         = "/root/mitstack-serve"
	CertPath    = Dir + "/tls.crt"
	KeyPath     = Dir + "/tls.key"
	ProgramPath = "/usr/local/bin/mitstack-serve"
	// GzipBase64 is the write_files encoding cloud-init decodes as base64 and then gunzips.
	GzipBase64 = "gz+b64"
	// certLifetime outlasts an install; the fetching side pins the key and ignores the validity.
	certLifetime = 24 * time.Hour
)

// Key is the server's TLS key, its self-signed certificate and the pin curl takes for its public key.
type Key struct {
	CertPEM, KeyPEM, Pin string
}

func NewKey(commonName string, now time.Time) (Key, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("generating the %s's TLS key: %w", commonName, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return Key{}, fmt.Errorf("drawing the %s's certificate serial: %w", commonName, err)
	}
	template := x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(certLifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &private.PublicKey, private)
	if err != nil {
		return Key{}, fmt.Errorf("signing the %s's certificate: %w", commonName, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return Key{}, fmt.Errorf("encoding the %s's TLS key: %w", commonName, err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return Key{}, fmt.Errorf("encoding the %s's public key: %w", commonName, err)
	}
	sum := sha256.Sum256(spki)
	return Key{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		KeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		Pin:     "sha256//" + base64.StdEncoding.EncodeToString(sum[:]),
	}, nil
}

// Files are the certificate, the key and the program, as the server writes them.
func (k Key) Files() []File {
	return []File{
		{Path: CertPath, Permissions: "0600", Content: k.CertPEM},
		{Path: KeyPath, Permissions: "0600", Content: k.KeyPEM},
		{Path: ProgramPath, Permissions: "0755", Content: Program},
	}
}

// File is a cloud-init write_files entry, its content in the encoding it names.
type File struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Encoding    string `yaml:"encoding,omitempty"`
}

// Gzipped is content gzipped and base64-encoded, for a File in the GzipBase64 encoding.
func Gzipped(content []byte) (string, error) {
	var zipped bytes.Buffer
	w, err := gzip.NewWriterLevel(&zipped, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("compressing a file: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		return "", fmt.Errorf("compressing a file: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("compressing a file: %w", err)
	}
	return base64.StdEncoding.EncodeToString(zipped.Bytes()), nil
}

// Command serves file once to client on port with the key Files writes.
func Command(client netip.Addr, port int, file string) []string {
	return []string{"python3", "-I", ProgramPath, client.String(), strconv.Itoa(port), CertPath, KeyPath, file}
}

type cloudConfig struct {
	WriteFiles []File     `yaml:"write_files"`
	RunCmd     [][]string `yaml:"runcmd"`
}

// UserData writes files and then runs commands in order, and refuses a result past Hetzner's user data limit.
func UserData(files []File, commands ...[]string) (string, error) {
	out, err := yaml.Marshal(cloudConfig{WriteFiles: files, RunCmd: commands})
	if err != nil {
		return "", fmt.Errorf("encoding a serving server's cloud-config: %w", err)
	}
	userData := "#cloud-config\n" + string(out)
	if len(userData) > gateway.UserDataLimit {
		return "", fmt.Errorf("the serving server's user data is %d bytes, over Hetzner's %d", len(userData), gateway.UserDataLimit)
	}
	return userData, nil
}
