package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"time"
)

// serialBits is the size of a certificate's random serial number.
const serialBits = 128

// CA is a cluster's gateway certificate authority in PEM, whose Cert the nodes trust and whose Key signs each gateway's certificate.
type CA struct {
	Cert []byte
	Key  []byte
}

// NewCA mints a gateway CA valid from now for validity. Every node trusts it as a system-wide root, so its critical name
// constraints permit only the gateways' addresses and exclude every host name, mail address and URI.
func NewCA(now time.Time, validity time.Duration, gateways []netip.Addr) (CA, error) {
	if len(gateways) == 0 {
		return CA{}, errors.New("the gateway CA names no gateway address to permit")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CA{}, fmt.Errorf("mint the gateway CA key: %w", err)
	}
	template, err := certificate(pkix.Name{CommonName: "mitstack gateway CA"}, now, validity)
	if err != nil {
		return CA{}, err
	}
	template.IsCA, template.BasicConstraintsValid = true, true
	template.KeyUsage = x509.KeyUsageCertSign
	// An empty constraint matches every name, and PermittedDNSDomainsCritical marks the whole extension critical.
	template.PermittedDNSDomainsCritical = true
	template.ExcludedDNSDomains, template.ExcludedEmailAddresses, template.ExcludedURIDomains = []string{""}, []string{""}, []string{""}
	for _, g := range gateways {
		template.PermittedIPRanges = append(template.PermittedIPRanges, &net.IPNet{IP: g.AsSlice(), Mask: net.CIDRMask(g.BitLen(), g.BitLen())})
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return CA{}, fmt.Errorf("sign the gateway CA: %w", err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return CA{}, err
	}
	return CA{Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: keyPEM}, nil
}

// Issue signs a serving certificate for a gateway at address, valid from now for validity.
func (ca CA) Issue(address netip.Addr, now time.Time, validity time.Duration) (TLS, error) {
	caCert, caKey, err := ca.parse()
	if err != nil {
		return TLS{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLS{}, fmt.Errorf("mint the gateway key for %s: %w", address, err)
	}
	// OpenSSL checks a common name shaped like a host name against the excluded host names, so this one holds a space.
	template, err := certificate(pkix.Name{CommonName: "mitstack gateway " + address.String()}, now, validity)
	if err != nil {
		return TLS{}, err
	}
	template.IPAddresses = []net.IP{address.AsSlice()}
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return TLS{}, fmt.Errorf("sign the gateway certificate for %s: %w", address, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return TLS{}, err
	}
	return TLS{CA: ca.Cert, Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: keyPEM}, nil
}

// Leaf reads the serving certificate, whose serial and validity a block's events name.
func (t TLS) Leaf() (*x509.Certificate, error) {
	block, _ := pem.Decode(t.Cert)
	if block == nil {
		return nil, errors.New("the gateway certificate is not PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func (ca CA) parse() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(ca.Cert)
	keyBlock, _ := pem.Decode(ca.Key)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("the gateway CA is not a PEM certificate and key")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("read the gateway CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("read the gateway CA key: %w", err)
	}
	return cert, key, nil
}

func certificate(subject pkix.Name, now time.Time, validity time.Duration) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("draw a serial number for %s: %w", subject.CommonName, err)
	}
	return &x509.Certificate{SerialNumber: serial, Subject: subject, NotBefore: now, NotAfter: now.Add(validity)}, nil
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode a gateway key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func pemType(data []byte) string {
	block, _ := pem.Decode(data)
	if block == nil {
		return ""
	}
	return block.Type
}
