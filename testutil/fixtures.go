// Package testutil holds the fixtures, fakes and decoders this module's tests and a block's tests share.
package testutil

import (
	"encoding/base64"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

// ToolsImage is a tools image reference for fixtures; nothing pulls it.
const ToolsImage = "ghcr.io/mitstack-eu/talos-tools:v0.1.0@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Testdata returns the path of a file in this package's testdata directory, which holds throwaway keys only.
func Testdata(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller found no file for the testutil package")
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

// ReadTestdata returns the bytes of a file in this package's testdata directory.
func ReadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(Testdata(t, name))
	require.NoError(t, err)
	return data
}

// Secrets returns the fixture cluster's throwaway secrets.
func Secrets(t *testing.T) *nodeconfig.Secrets {
	t.Helper()
	s, err := nodeconfig.ParseSecrets(ReadTestdata(t, "secrets.yaml"))
	require.NoError(t, err)
	return s
}

// GatewayTLS returns the fixture gateway's throwaway CA, certificate and key, for 10.0.0.2.
func GatewayTLS(t *testing.T) gateway.TLS {
	t.Helper()
	return gateway.TLS{
		CA:   ReadTestdata(t, "gateway-ca.pem"),
		Cert: ReadTestdata(t, "gateway-cert.pem"),
		Key:  ReadTestdata(t, "gateway-key.pem"),
	}
}

// LogExport returns the fixture log receiver.
func LogExport(t *testing.T) components.LogExport {
	t.Helper()
	return components.LogExport{Endpoint: "otlp.mitstack.dev:4317", CA: ReadTestdata(t, "logs-ca.pem")}
}

// ClusterOption changes one field of the fixture cluster.
type ClusterOption func(*nodeconfig.Cluster)

// WithProfile sets the cluster's imager profile.
func WithProfile(p nodeconfig.Profile) ClusterOption {
	return func(c *nodeconfig.Cluster) { c.Profile = p }
}

// WithLocations gives the cluster one private load balancer and one gateway in each of n locations.
func WithLocations(n int) ClusterOption {
	return func(c *nodeconfig.Cluster) {
		c.APIAddresses, c.Gateways = nil, nil
		for i := range n {
			c.APIAddresses = append(c.APIAddresses, netip.AddrFrom4([4]byte{10, 0, byte(i), 3}))
			c.Gateways = append(c.Gateways, netip.AddrFrom4([4]byte{10, 0, byte(i), 2}))
		}
	}
}

// NewCluster returns a one-location Cloud cluster with the fixture secrets, changed by opts.
func NewCluster(t *testing.T, opts ...ClusterOption) nodeconfig.Cluster {
	t.Helper()
	c := nodeconfig.Cluster{
		Name:           "shop",
		SubscriptionID: "sub_0123456789ab",
		Profile:        nodeconfig.HCloud,
		Components:     components.Default(),
		Secrets:        Secrets(t),
		Network:        netip.MustParsePrefix("10.0.0.0/16"),
		GatewayCA:      ReadTestdata(t, "gateway-ca.pem"),
		Logs:           LogExport(t),
	}
	WithLocations(1)(&c)
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// SecretValues returns every key, token and certificate-authority key in the fixture secrets, base64 and decoded,
// so a test can prove none of them reaches a log.
func SecretValues(t *testing.T) []string {
	t.Helper()
	var bundle map[string]any
	require.NoError(t, yaml.Unmarshal(ReadTestdata(t, "secrets.yaml"), &bundle))
	var values []string
	collectSecrets(bundle, "", &values)
	require.NotEmpty(t, values, "the fixture secrets hold no key")
	return values
}

// collectSecrets walks the bundle and keeps each value under a key, token or secret field.
func collectSecrets(node any, field string, values *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			collectSecrets(child, k, values)
		}
	case string:
		switch field {
		case "key", "token", "secret", "bootstraptoken", "secretboxencryptionsecret", "aescbcencryptionsecret":
			*values = append(*values, v)
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				*values = append(*values, pemBodyLines(decoded)...)
			}
		}
	}
}

// pemBodyLines returns the base64 lines of a PEM block, which appear verbatim however a document indents it.
func pemBodyLines(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) >= minPEMLine && !strings.HasPrefix(line, "-----") {
			lines = append(lines, line)
		}
	}
	return lines
}

// minPEMLine is the shortest PEM body line worth matching, short enough to catch a key's last line.
const minPEMLine = 16
