// Package gateway renders the cloud-config of a cluster's gateway, the one server with a public address,
// which forwards and records the nodes' traffic and serves their image mirror and manifests.
package gateway

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

// UserDataLimit is the most user data Hetzner Cloud accepts for a server, in bytes.
const UserDataLimit = 32 * 1024

// httpsPort is the port the nodes reach GitHub, the platform and the cluster's other HTTPS peers on.
const httpsPort = 443

// The gateway's own containers, by digest.
const (
	registryImage = "docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	caddyImage    = "docker.io/library/caddy:2.10.2-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d"
	// skopeoImage copies the mirror, since Ubuntu 24.04's skopeo 1.13.3 cannot copy an OCI index with its digests kept.
	skopeoImage = "quay.io/skopeo/stable:v1.20.0@sha256:47853bb9fb24202af9110531ebd6e43c5f97701254ca290596640290d17942f4"
)

var (
	//go:embed cloud-config.yaml
	cloudConfigTemplate string
	//go:embed scripts/prepare.sh
	prepareScript string
)

var cloudConfig = tmpl.Must("the gateway's cloud-config", cloudConfigTemplate)

// Inputs are what one gateway needs.
type Inputs struct {
	// Address is the gateway's private address, which the network's default route points at.
	Address netip.Addr
	// Network is the cluster's private network.
	Network netip.Prefix
	TLS     TLS
	// Components is the cluster's component set, whose images the gateway mirrors and whose served manifests it fetches.
	Components components.Set
	// ExtraImages are images the nodes pull beyond the component set, such as the tools image.
	ExtraImages []components.Image
	// ManifestBaseURL is the release of this kit the served manifests are fetched from, as ReleaseURL returns.
	ManifestBaseURL string
	// Logs is the receiver the node-local Collectors export to, whose port the gateway forwards when there is one.
	Logs components.LogExport
}

// ReleaseURL is where a tagged release of this kit serves its files, as v0.1.0.
func ReleaseURL(version string) string {
	return "https://raw.githubusercontent.com/mitstack-eu/talos-cluster-kit/" + version
}

// Render returns the gateway's cloud-config, refusing one past Hetzner's user data limit.
func Render(in Inputs) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	data, err := in.templateData()
	if err != nil {
		return nil, err
	}
	out, err := tmpl.Execute(cloudConfig, data)
	if err != nil {
		return nil, err
	}
	if len(out) > UserDataLimit {
		return nil, fmt.Errorf("the gateway's cloud-config is %d bytes, over Hetzner's %d", len(out), UserDataLimit)
	}
	return out, nil
}

func (in Inputs) validate() error {
	var errs []error
	if !in.Network.IsValid() || !in.Network.Contains(in.Address) {
		errs = append(errs, fmt.Errorf("address %s is not inside network %s", in.Address, in.Network))
	}
	if err := in.TLS.validate(); err != nil {
		errs = append(errs, err)
	}
	if !strings.HasPrefix(in.ManifestBaseURL, "https://") {
		errs = append(errs, fmt.Errorf("manifest base URL %q is not https", in.ManifestBaseURL))
	}
	if errs != nil {
		return fmt.Errorf("gateway %s: %w", in.Address, errors.Join(errs...))
	}
	return nil
}

func (in Inputs) templateData() (map[string]string, error) {
	images, err := mirror.List(in.Components, in.ExtraImages...)
	if err != nil {
		return nil, err
	}
	ports, err := in.egressPorts()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"Address":         in.Address.String(),
		"Network":         in.Network.String(),
		"Resolvers":       joinAddrs(nodeconfig.HetznerResolvers),
		"EgressPorts":     ports,
		"CA":              string(in.TLS.CA),
		"Cert":            string(in.TLS.Cert),
		"Key":             string(in.TLS.Key),
		"Images":          string(mirror.Render(images)),
		"Manifests":       in.manifestList(),
		"ToolsEnv":        tools.Env + "SKOPEO_IMAGE=" + skopeoImage + "\n",
		"PrepareScript":   prepareScript,
		"BootstrapScript": string(etcd.BootstrapScript()),
		"RegistryImage":   registryImage,
		"CaddyImage":      caddyImage,
	}, nil
}

// egressPorts are the TCP ports the nodes may reach past the gateway: HTTPS, and the log receiver's port if any.
func (in Inputs) egressPorts() (string, error) {
	if in.Logs.IsZero() {
		return strconv.Itoa(httpsPort), nil
	}
	_, port, err := net.SplitHostPort(in.Logs.Endpoint)
	if err != nil {
		return "", fmt.Errorf("log export endpoint %q: %w", in.Logs.Endpoint, err)
	}
	logPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", fmt.Errorf("log export endpoint %q has no port number: %w", in.Logs.Endpoint, err)
	}
	ports := []string{strconv.Itoa(httpsPort)}
	if logPort != httpsPort {
		ports = append(ports, strconv.FormatUint(logPort, 10))
	}
	return strings.Join(ports, ", "), nil
}

// manifestList is one "<file> <sha256> <url>" line per served manifest, the form prepare.sh reads.
func (in Inputs) manifestList() string {
	var lines []string
	for _, m := range in.Components.Served {
		lines = append(lines, fmt.Sprintf("%s.yaml %s %s/%s", m.TalosName(), m.SHA256(), in.ManifestBaseURL, m.Path))
	}
	return strings.Join(lines, "\n")
}

func joinAddrs(addrs []netip.Addr) string {
	s := make([]string, 0, len(addrs))
	for _, a := range addrs {
		s = append(s, a.String())
	}
	return strings.Join(s, ", ")
}

// TLS is the gateway's certificate authority and its serving certificate and key, all PEM.
type TLS struct {
	CA   []byte
	Cert []byte
	Key  []byte
}

func (t TLS) validate() error {
	if err := check.PEMCertificate(t.CA); err != nil {
		return fmt.Errorf("gateway CA: %w", err)
	}
	if err := check.PEMCertificate(t.Cert); err != nil {
		return fmt.Errorf("gateway certificate: %w", err)
	}
	if !slices.Contains([]string{"EC PRIVATE KEY", "PRIVATE KEY"}, pemType(t.Key)) {
		return errors.New("gateway key is not a PEM private key")
	}
	return nil
}
