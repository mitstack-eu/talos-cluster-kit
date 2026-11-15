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
	// Repository is a registry and a path with no tag, digest or pattern, in public ghcr.io or a private Amazon ECR registry.
	Repository string
	Issuer     string
	Subject    string
	// PullSecret is the docker config Secret holding a login for an ECR Repository's registry; a ghcr.io entry names none.
	PullSecret SecretRef
}

type SecretRef struct {
	Namespace, Name string
}

const SignaturePolicyName = "mitstack-verify-image-signatures"

// Kyverno reads a registry login only from its own namespace, so External Secrets copies each pull Secret there, reading
// it as SignatureLoginReader, which the namespace that holds the Secret grants get on that Secret alone.
const (
	SignatureLoginNamespace = "kyverno"
	SignatureLoginReader    = "mitstack-signature-logins"
)

// The limits keep the longest list's policy within the serving server's user data, where a build with signers fetches it.
const (
	SignerLimit           = 8
	SignerRepositoryLimit = 128
	SignerIssuerLimit     = 128
	SignerSubjectLimit    = 256
	SignerLoginLimit      = 2
	SignerSecretLimit     = 63
)

const sigstoreTrustRoot = "tuf-repo-cdn.sigstore.dev"

// The copy follows its pull Secret within this interval, so a login renewed this long before it expires reaches Kyverno in time.
const signatureLoginRefresh = "15m"

const signatureLoginPrefix = "mitstack-signature-login-"

// publicRegistryHosts joins a registry only with hosts a test proves sufficient, since a check that cannot read a signature
// refuses every image.
var publicRegistryHosts = map[string][]string{"ghcr.io": {"ghcr.io", "pkg-containers.githubusercontent.com"}}

// The patterns admit no quote, space or line break, so each value sits whole inside the policy's quoted strings.
var (
	signerRepositoryRe = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
	signerIssuerRe     = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(?:/[A-Za-z0-9._~/-]*)?$`)
	signerSubjectRe    = regexp.MustCompile(`^[A-Za-z0-9._~:/@+=-]+$`)
	signerSecretRe     = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	ecrRegistryRe      = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr\.([a-z]{2}(?:-[a-z]+)+-[0-9]+)\.amazonaws\.com$`)
)

//go:embed admission/mitstack-verify-image-signatures.yaml
var signaturePolicyTemplate string

var signaturePolicy = tmpl.Must("the signature policy", signaturePolicyTemplate)

// signatureLogin is a pull Secret a signer names and the copy of it in Kyverno's namespace that the policy reads.
type signatureLogin struct {
	SecretRef
	Copy string
}

// SignaturePolicy renders the policy, the login copies its checks read and the egress limit they need as one manifest,
// and none for an empty list.
func SignaturePolicy(signers []Signer) ([]Manifest, error) {
	if len(signers) == 0 {
		return nil, nil
	}
	hosts, err := signatureHosts(signers)
	if err != nil {
		return nil, fmt.Errorf("the signer list: %w", err)
	}
	logins := signatureLogins(signers)
	if len(logins) > SignerLoginLimit {
		return nil, fmt.Errorf("the signer list names %d pull Secrets, and the kit takes %d at most", len(logins), SignerLoginLimit)
	}
	out, err := tmpl.Execute(signaturePolicy, map[string]any{"Name": SignaturePolicyName, "Signers": signers, "Hosts": hosts,
		"Logins": logins, "Namespace": SignatureLoginNamespace, "Reader": SignatureLoginReader, "Refresh": signatureLoginRefresh})
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
		if slices.Contains(seen, s.Repository) {
			errs = append(errs, fmt.Errorf("repository %s is named twice", s.Repository))
		}
		seen = append(seen, s.Repository)
		errs = append(errs, checkSigner(s)...)
		for _, host := range registryHosts(s.Repository) {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
	}
	return append(hosts, sigstoreTrustRoot), errors.Join(errs...)
}

func checkSigner(s Signer) []error {
	var errs []error
	switch {
	case !signerRepositoryRe.MatchString(s.Repository):
		errs = append(errs, fmt.Errorf("repository %q is not a registry and a path with no tag, digest or pattern", s.Repository))
	case len(s.Repository) > SignerRepositoryLimit:
		errs = append(errs, fmt.Errorf("repository %s is longer than %d characters", s.Repository, SignerRepositoryLimit))
	default:
		errs = append(errs, checkRegistry(s)...)
	}
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
	return errs
}

// checkRegistry holds an entry to a registry the kit holds hosts for, an ECR entry to a Secret Kyverno can read its
// registry with, and a ghcr.io entry to none.
func checkRegistry(s Signer) []error {
	registry, _, _ := strings.Cut(s.Repository, "/")
	secret := s.PullSecret
	switch {
	case registryHosts(s.Repository) == nil:
		return []error{fmt.Errorf("repository %s is in %s, a registry the kit holds no hosts for; it reads signatures from %s with no "+
			"login, and from a private Amazon ECR registry such as 123456789012.dkr.ecr.eu-central-1.amazonaws.com with the entry's "+
			"pull Secret", s.Repository, registry, strings.Join(slices.Sorted(maps.Keys(publicRegistryHosts)), ", "))}
	case publicRegistryHosts[registry] != nil && secret != (SecretRef{}):
		return []error{fmt.Errorf("repository %s is in %s, which Kyverno reads with no login, and the entry names the pull Secret %s/%s",
			s.Repository, registry, secret.Namespace, secret.Name)}
	case publicRegistryHosts[registry] != nil:
		return nil
	case secret == (SecretRef{}):
		return []error{fmt.Errorf("repository %s is in the private registry %s, which Kyverno reads only with a login, and the entry "+
			"names no pull Secret that holds one", s.Repository, registry)}
	}
	var errs []error
	for _, part := range [][2]string{{"namespace", secret.Namespace}, {"name", secret.Name}} {
		if !signerSecretRe.MatchString(part[1]) || len(part[1]) > SignerSecretLimit {
			errs = append(errs, fmt.Errorf("the pull Secret %s %q of %s is not a lowercase name of %d characters at most",
				part[0], part[1], s.Repository, SignerSecretLimit))
		}
	}
	return errs
}

// registryHosts are the hosts Kyverno reads a repository's signatures from: for an ECR registry its own host and the
// bucket it serves every layer from, and none for a registry the kit holds no hosts for.
func registryHosts(repository string) []string {
	registry, _, _ := strings.Cut(repository, "/")
	if hosts, ok := publicRegistryHosts[registry]; ok {
		return hosts
	}
	if m := ecrRegistryRe.FindStringSubmatch(registry); m != nil {
		return []string{registry, "prod-" + m[1] + "-starport-layer-bucket.s3." + m[1] + ".amazonaws.com"}
	}
	return nil
}

func signatureLogins(signers []Signer) []signatureLogin {
	var logins []signatureLogin
	for _, s := range signers {
		named := func(l signatureLogin) bool { return l.SecretRef == s.PullSecret }
		if s.PullSecret == (SecretRef{}) || slices.ContainsFunc(logins, named) {
			continue
		}
		logins = append(logins, signatureLogin{SecretRef: s.PullSecret, Copy: fmt.Sprint(signatureLoginPrefix, len(logins))})
	}
	return logins
}
