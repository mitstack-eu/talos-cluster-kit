package nodeconfig

import (
	"bytes"
	"fmt"
	"net/netip"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/client/config"
	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/role"
	"go.yaml.in/yaml/v4"
)

// versionContract is the Talos release whose configuration format the render writes.
var versionContract = machineryconfig.TalosVersion1_14

// Secrets holds every certificate authority, key and token of one cluster.
type Secrets struct {
	bundle *secrets.Bundle
}

// NewSecrets mints a new cluster's secrets, with certificates valid from now.
func NewSecrets(now time.Time) (*Secrets, error) {
	bundle, err := secrets.NewBundle(secrets.NewFixedClock(now), versionContract)
	if err != nil {
		return nil, fmt.Errorf("mint the cluster secrets: %w", err)
	}
	return &Secrets{bundle: bundle}, nil
}

// ParseSecrets reads secrets in the form Bytes writes, the form of `talosctl gen secrets`.
func ParseSecrets(data []byte) (*Secrets, error) {
	bundle := &secrets.Bundle{Clock: secrets.NewClock()}
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(bundle); err != nil {
		return nil, fmt.Errorf("read the cluster secrets: %w", err)
	}
	if err := bundle.Validate(versionContract); err != nil {
		return nil, fmt.Errorf("read the cluster secrets: %w", err)
	}
	return &Secrets{bundle: bundle}, nil
}

// Bytes returns the secrets as YAML, which holds every key of the cluster.
func (s *Secrets) Bytes() ([]byte, error) {
	var out bytes.Buffer
	if err := yaml.NewEncoder(&out).Encode(s.bundle); err != nil {
		return nil, fmt.Errorf("write the cluster secrets: %w", err)
	}
	return out.Bytes(), nil
}

// AdminTalosconfig returns a talosconfig for node with the os:admin role, valid from now for ttl, which the etcd
// bootstrap needs because it accepts no narrower role.
func (s *Secrets) AdminTalosconfig(node netip.Addr, ttl time.Duration, now time.Time) ([]byte, error) {
	bundle := *s.bundle
	bundle.Clock = secrets.NewFixedClock(now)
	cert, err := bundle.GenerateTalosAPIClientCertificateWithTTL(role.MakeSet(role.Admin), ttl)
	if err != nil {
		return nil, fmt.Errorf("issue an admin certificate for %s: %w", node, err)
	}
	cfg := config.NewConfig("admin", []string{node.String()}, s.bundle.Certs.OS.Crt, cert)
	cfg.Contexts["admin"].Nodes = []string{node.String()}
	return cfg.Bytes()
}
