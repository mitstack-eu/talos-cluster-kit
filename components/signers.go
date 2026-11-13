package components

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

// Signer is a repository whose images a cluster starts only with the keyless signature of Issuer and Subject.
type Signer struct {
	// Repository is a registry and a path with no tag, digest or pattern, and public, since Kyverno reads it with no login.
	Repository string
	Issuer     string
	Subject    string
}

const SignaturePolicyName = "mitstack-verify-image-signatures"

// The limits keep the longest list's policy within the serving server's user data, where a build with signers fetches it.
const (
	SignerLimit           = 8
	SignerRepositoryLimit = 128
	SignerIssuerLimit     = 128
	SignerSubjectLimit    = 256
)

const sigstoreTrustRoot = "tuf-repo-cdn.sigstore.dev"

// registryHosts joins a registry only with hosts a test proves sufficient, since a check that cannot read a signature refuses every image.
var registryHosts = map[string][]string{"ghcr.io": {"ghcr.io", "pkg-containers.githubusercontent.com"}}

// The patterns admit no quote, space or line break, so each value sits whole inside the policy's quoted strings.
var (
	signerRepositoryRe = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
	signerIssuerRe     = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(?:/[A-Za-z0-9._~/-]*)?$`)
	signerSubjectRe    = regexp.MustCompile(`^[A-Za-z0-9._~:/@+=-]+$`)
)

//go:embed admission/mitstack-verify-image-signatures.yaml
var signaturePolicyTemplate string

var signaturePolicy = tmpl.Must("the signature policy", signaturePolicyTemplate)

// SignaturePolicy renders the policy and the egress limit its checks need as one manifest, and none for an empty list.
func SignaturePolicy(signers []Signer) ([]Manifest, error) {
	if len(signers) == 0 {
		return nil, nil
	}
	hosts, err := signatureHosts(signers)
	if err != nil {
		return nil, fmt.Errorf("the signer list: %w", err)
	}
	out, err := tmpl.Execute(signaturePolicy, map[string]any{"Name": SignaturePolicyName, "Signers": signers, "Hosts": hosts})
	if err != nil {
		return nil, err
	}
	return []Manifest{{Name: SignaturePolicyName, Path: "components/admission/" + SignaturePolicyName + ".yaml", Bytes: out}}, nil
}

func signatureHosts(signers []Signer) ([]string, error) {
	var errs []error
	if len(signers) > SignerLimit {
		errs = append(errs, fmt.Errorf("it names %d signers, and the kit takes %d signers at most", len(signers), SignerLimit))
	}
	var hosts, seen []string
	for _, s := range signers {
		registry, _, _ := strings.Cut(s.Repository, "/")
		switch {
		case !signerRepositoryRe.MatchString(s.Repository):
			errs = append(errs, fmt.Errorf("repository %q is not a registry and a path with no tag, digest or pattern", s.Repository))
		case len(s.Repository) > SignerRepositoryLimit:
			errs = append(errs, fmt.Errorf("repository %s is longer than %d characters", s.Repository, SignerRepositoryLimit))
		case registryHosts[registry] == nil:
			errs = append(errs, fmt.Errorf("repository %s is in %s, a registry the kit holds no hosts and no credential for; it reads signatures from %s",
				s.Repository, registry, strings.Join(slices.Sorted(maps.Keys(registryHosts)), ", ")))
		case slices.Contains(seen, s.Repository):
			errs = append(errs, fmt.Errorf("repository %s is named twice", s.Repository))
		}
		seen = append(seen, s.Repository)
		if !signerIssuerRe.MatchString(s.Issuer) {
			errs = append(errs, fmt.Errorf("the issuer %q of %s is not an https URL", s.Issuer, s.Repository))
		} else if len(s.Issuer) > SignerIssuerLimit {
			errs = append(errs, fmt.Errorf("the issuer of %s is longer than %d characters", s.Repository, SignerIssuerLimit))
		}
		if !signerSubjectRe.MatchString(s.Subject) {
			errs = append(errs, fmt.Errorf("the subject %q of %s is not a certificate identity without a quote or a space", s.Subject, s.Repository))
		} else if len(s.Subject) > SignerSubjectLimit {
			errs = append(errs, fmt.Errorf("the subject of %s is longer than %d characters", s.Repository, SignerSubjectLimit))
		}
		for _, host := range registryHosts[registry] {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
	}
	return append(hosts, sigstoreTrustRoot), errors.Join(errs...)
}
