// Package openbaoinit renders the Job that initialises a hosting cluster's OpenBao once.
package openbaoinit

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// The write deploy key the Job pushes InitFile with, sealed into the namespace the Job runs in.
const (
	DeployKeyNamespace = "mitstack-node"
	DeployKeySecret    = "github-deploy-key"
	DeployKeyItem      = "ssh-privatekey"
)

type Inputs struct {
	// ToolsImage is the image built from tools/Dockerfile, by digest.
	ToolsImage string
	Repository string
	// Recipient is the ASCII-armored OpenPGP public key behind openbaoSealKeyRecipient, which the Job encrypts to.
	Recipient []byte
	// KnownHosts are the host keys the Job accepts for GitHub's SSH endpoint, in known_hosts form.
	KnownHosts string
}

var repositoryRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)

// RepositoryURL reaches a GitHub repository over SSH on port 443, since the gateway forwards no other port.
func RepositoryURL(repository string) string {
	return "ssh://git@ssh.github.com:443/" + repository + ".git"
}

func Manifests(in Inputs) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	return tmpl.Execute(job, map[string]string{
		"Namespace":       DeployKeyNamespace,
		"DeployKeySecret": DeployKeySecret,
		"DeployKeyItem":   DeployKeyItem,
		"Image":           in.ToolsImage,
		"RepositoryURL":   RepositoryURL(in.Repository),
		"Recipient":       string(in.Recipient),
		"KnownHosts":      in.KnownHosts,
		"Script":          initScript,
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
	if strings.TrimSpace(in.KnownHosts) == "" {
		errs = append(errs, errors.New("no host key is known for GitHub's SSH endpoint"))
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
