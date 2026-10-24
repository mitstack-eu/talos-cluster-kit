// Package openbaoinit renders the Job that initialises a hosting cluster's OpenBao once.
package openbaoinit

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

var (
	//go:embed openbao-init.yaml
	jobTemplate string
	//go:embed scripts/openbao-init.sh
	initScript string
)

var job = tmpl.Must("OpenBao's init Job", jobTemplate)

const InitFile = "openbao-init.asc"

const (
	AppKeyNamespace = "mitstack-node"
	AppKeySecret    = "github-app-key"
	AppKeyItem      = "private-key"
)

type Inputs struct {
	// ToolsImage is the image built from tools/Dockerfile, by digest.
	ToolsImage            string
	Repository            string
	AppID, InstallationID string
	// Recipient is the ASCII-armored OpenPGP public key behind openbaoSealKeyRecipient, which the Job encrypts to.
	Recipient []byte
}

var repositoryRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)

func Manifests(in Inputs) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	return tmpl.Execute(job, map[string]string{
		"Namespace":      AppKeyNamespace,
		"AppKeySecret":   AppKeySecret,
		"AppKeyItem":     AppKeyItem,
		"Image":          in.ToolsImage,
		"Repository":     in.Repository,
		"AppID":          in.AppID,
		"InstallationID": in.InstallationID,
		"Recipient":      string(in.Recipient),
		"Script":         initScript,
	})
}

func (in Inputs) validate() error {
	var errs []error
	if err := check.DigestRef(in.ToolsImage); err != nil {
		errs = append(errs, fmt.Errorf("tools image: %w", err))
	}
	if !repositoryRe.MatchString(in.Repository) {
		errs = append(errs, fmt.Errorf("repository %q is not owner/name", in.Repository))
	}
	if err := check.GitHubID(in.AppID); err != nil {
		errs = append(errs, fmt.Errorf("GitHub App id %w", err))
	}
	if err := check.GitHubID(in.InstallationID); err != nil {
		errs = append(errs, fmt.Errorf("installation id %w", err))
	}
	if err := canEncryptTo(in.Recipient); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// canEncryptTo also refuses any private key, since the ConfigMap that carries the recipient is no place for one.
func canEncryptTo(recipient []byte) error {
	block, err := armor.Decode(bytes.NewReader(recipient))
	if err != nil || block.Type != openpgp.PublicKeyType {
		return errors.New("the recipient is not an armored OpenPGP public key")
	}
	keys, err := openpgp.ReadKeyRing(block.Body)
	if err != nil || len(keys) != 1 || keys[0].PrivateKey != nil {
		return errors.New("the recipient is not an armored OpenPGP public key")
	}
	if _, ok := keys[0].EncryptionKey(time.Now()); !ok {
		return errors.New("the recipient key has no key that can encrypt")
	}
	return nil
}
