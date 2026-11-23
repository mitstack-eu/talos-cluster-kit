package robotcluster

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/components"
)

const (
	signerFields       = 3
	signerFieldsSecret = 4
)

// readSigners reads the signers param, comma-separated repository|issuer|subject entries, with a private registry's
// pull Secret as namespace/name in a fourth field.
func readSigners(d contract.Desired) ([]components.Signer, error) {
	raw := d.Params[ParamSigners]
	if raw == "" {
		return nil, nil
	}
	var signers []components.Signer
	for _, entry := range strings.Split(raw, ",") {
		s, err := readSigner(strings.TrimSpace(entry))
		if err != nil {
			return nil, refuseSigners(raw, err)
		}
		signers = append(signers, s)
	}
	if _, err := components.SignaturePolicy(signers); err != nil {
		return nil, refuseSigners(raw, err)
	}
	if err := pullSecretsNeedHosting(signers, HostsPlatform(d)); err != nil {
		return nil, refuseSigners(raw, err)
	}
	return signers, nil
}

func readSigner(entry string) (components.Signer, error) {
	fields := strings.Split(entry, "|")
	if len(fields) != signerFields && len(fields) != signerFieldsSecret {
		return components.Signer{}, fmt.Errorf("the entry %q holds %d fields, and an entry holds a repository, an issuer and a subject, "+
			"with a private registry's pull Secret as namespace/name after them", entry, len(fields))
	}
	s := components.Signer{Repository: fields[0], Issuer: fields[1], Subject: fields[2]}
	if len(fields) == signerFields {
		return s, nil
	}
	namespace, name, ok := strings.Cut(fields[3], "/")
	if !ok || namespace == "" || name == "" {
		return components.Signer{}, fmt.Errorf("the entry for %s names the pull Secret %q, which is not namespace/name", s.Repository, fields[3])
	}
	s.PullSecret = components.SecretRef{Namespace: namespace, Name: name}
	return s, nil
}

// pullSecretsNeedHosting refuses a pull Secret on a cluster that hosts no platform, since External Secrets copies it for
// Kyverno and only a hosting cluster runs External Secrets.
func pullSecretsNeedHosting(signers []components.Signer, hosting bool) error {
	if hosting {
		return nil
	}
	var errs []error
	for _, s := range signers {
		if s.PullSecret != (components.SecretRef{}) {
			errs = append(errs, fmt.Errorf("the entry for %s names the pull Secret %s/%s, and only a cluster that hosts the platform "+
				"copies a pull Secret for Kyverno", s.Repository, s.PullSecret.Namespace, s.PullSecret.Name))
		}
	}
	return errors.Join(errs...)
}

func refuseSigners(raw string, err error) error {
	return handler.Refuse("invalid-param", "params[%q] is %q: %s", ParamSigners, raw, err)
}
