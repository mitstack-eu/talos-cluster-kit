// Package check holds the input checks more than one of the kit's packages applies.
package check

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
)

// PEMCertificate fails unless data opens with a PEM block holding an X.509 certificate.
func PEMCertificate(data []byte) error {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("not a certificate: %w", err)
	}
	return nil
}

var (
	digestRefRe = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+(/[a-z0-9._-]+)+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
	dns1123Re   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	githubIDRe  = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

// GitHubID fails unless id is the positive decimal number GitHub gives an App or an installation.
func GitHubID(id string) error {
	if !githubIDRe.MatchString(id) {
		return fmt.Errorf("%q is not a number", id)
	}
	return nil
}

// DigestRef fails unless ref names an image as registry/repository:tag@sha256:digest.
func DigestRef(ref string) error {
	if !digestRefRe.MatchString(ref) {
		return fmt.Errorf("image %q is not registry/repository:tag@sha256:digest", ref)
	}
	return nil
}

// DNS1123Label fails unless name is a lower-case DNS label, the form of a node name.
func DNS1123Label(name string) error {
	if !dns1123Re.MatchString(name) {
		return fmt.Errorf("%q is not a DNS-1123 label", name)
	}
	return nil
}
