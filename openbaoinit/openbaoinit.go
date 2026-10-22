// Package openbaoinit renders the Job that initialises a hosting cluster's OpenBao once, hands its root token and
// recovery key to the cluster repository only as PGP messages, and sets up what each slice's External Secrets reads.
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

// InitFile is the file the Job commits at the top of the cluster repository: the root token's PGP message, then the
// recovery key's, each after a line naming it.
const InitFile = "openbao-init.asc"

// The Secret the hosting GitHub App's key opens into, which the Job mints its repository tokens with.
const (
	AppKeyNamespace = "mitstack-node"
	AppKeySecret    = "github-app-key"
	AppKeyItem      = "private-key"
)

// Inputs are what one cluster's init Job needs.
type Inputs struct {
	// ToolsImage is the image built from tools/Dockerfile, by digest.
	ToolsImage string
	// Repository is the cluster repository, as owner/name, that the Job commits InitFile to.
	Repository string
	// AppID and InstallationID name the hosting GitHub App and its installation on Repository.
	AppID, InstallationID string
	// Recipient is the ASCII-armored OpenPGP public key behind openbaoSealKeyRecipient, which the Job encrypts to.
	Recipient []byte
}

var (
	repositoryRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
	githubIDRe   = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

// Manifests renders the Job with its namespace, its service account and the ConfigMap that holds the recipient key.
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
	if !githubIDRe.MatchString(in.AppID) {
		errs = append(errs, fmt.Errorf("GitHub App id %q is not a number", in.AppID))
	}
	if !githubIDRe.MatchString(in.InstallationID) {
		errs = append(errs, fmt.Errorf("installation id %q is not a number", in.InstallationID))
	}
	if err := canEncryptTo(in.Recipient); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// canEncryptTo fails unless recipient is one armored public key with a key that can encrypt, since the ConfigMap that
// carries it is no place for a private key.
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
