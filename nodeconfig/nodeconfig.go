// Package nodeconfig renders the Talos machine configuration a cluster's disk image embeds.
package nodeconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"

	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
)

// Profile is the imager profile, HCloud for a server named by Hetzner's metadata service and Metal for a dedicated one.
type Profile string

const (
	HCloud Profile = "hcloud"
	Metal  Profile = "metal"
)

const apiServerPort = 6443

const SubscriptionLabel = "mitstack.dev/subscription"

// Cluster is what every node of one cluster shares.
type Cluster struct {
	Name           string
	SubscriptionID string
	Profile        Profile
	Components     components.Set
	Secrets        *Secrets
	// Network is the cluster's private network, which etcd and the kubelet bind to.
	Network netip.Prefix
	// APIAddresses are the API's private load balancers, one per location, the first the control-plane endpoint.
	APIAddresses []netip.Addr
	Gateways     []netip.Addr
	GatewayCA    []byte
	// Logs is where the node-local Collector exports logs; with none, the node runs no Collector.
	Logs components.LogExport
	// ServiceAccountIssuer is the issuer every service-account token names, where AWS reads the cluster's key set.
	ServiceAccountIssuer *url.URL
}

type Node struct {
	// Hostname names a Metal node, which has no metadata service, and stays empty on a Cloud node.
	Hostname string
}

// Render returns a control-plane node's multi-document machine configuration, validated against Talos's schema.
func Render(c Cluster, n Node) ([]byte, error) {
	if err := c.validate(n); err != nil {
		return nil, err
	}
	base, err := c.generate()
	if err != nil {
		return nil, err
	}
	extra, err := c.documents(n)
	if err != nil {
		return nil, err
	}
	cfg, err := container.New(append(c.adapt(base.Documents()), extra...)...)
	if err != nil {
		return nil, fmt.Errorf("assemble the configuration of cluster %s: %w", c.Name, err)
	}
	if _, err := cfg.Validate(imageMode{}); err != nil {
		return nil, fmt.Errorf("validate the configuration of cluster %s: %w", c.Name, err)
	}
	return cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
}

func (c Cluster) generate() (machineryconfig.Provider, error) {
	sans := make([]string, 0, len(c.APIAddresses))
	for _, addr := range c.APIAddresses {
		sans = append(sans, addr.String())
	}
	input, err := generate.NewInput(c.Name, c.endpoint(), c.Components.Versions.Kubernetes,
		generate.WithVersionContract(versionContract),
		generate.WithSecretsBundle(c.Secrets.bundle),
		generate.WithClusterDiscovery(false),
		generate.WithAllowSchedulingOnControlPlanes(true),
		generate.WithAdditionalSubjectAltNames(sans),
	)
	if err != nil {
		return nil, fmt.Errorf("prepare the configuration of cluster %s: %w", c.Name, err)
	}
	cfg, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("generate the configuration of cluster %s: %w", c.Name, err)
	}
	return cfg, nil
}

func (c Cluster) endpoint() string {
	return "https://" + netip.AddrPortFrom(c.APIAddresses[0], apiServerPort).String()
}

var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

func (c Cluster) validate(n Node) error {
	var errs []error
	if c.Secrets == nil {
		errs = append(errs, errors.New("the cluster has no secrets"))
	}
	if !labelValueRe.MatchString(c.SubscriptionID) {
		errs = append(errs, fmt.Errorf("subscription id %q is not a Kubernetes label value", c.SubscriptionID))
	}
	if !c.Network.IsValid() {
		errs = append(errs, errors.New("the cluster has no private network"))
	}
	if len(c.APIAddresses) == 0 || len(c.Gateways) == 0 {
		errs = append(errs, errors.New("the cluster needs at least one API address and one gateway"))
	}
	if err := check.PEMCertificate(c.GatewayCA); err != nil {
		errs = append(errs, fmt.Errorf("gateway CA: %w", err))
	}
	if err := checkIssuer(c.ServiceAccountIssuer); err != nil {
		errs = append(errs, err)
	}
	if err := c.validateNode(n); err != nil {
		errs = append(errs, err)
	}
	if errs != nil {
		return fmt.Errorf("cluster %q: %w", c.Name, errors.Join(errs...))
	}
	return nil
}

// checkIssuer refuses an issuer the API server serves no discovery for: one not https, or with a query or fragment.
func checkIssuer(issuer *url.URL) error {
	switch {
	case issuer == nil:
		return errors.New("the cluster has no service-account issuer")
	case issuer.Scheme != "https" || issuer.Host == "" || issuer.RawQuery != "" || issuer.Fragment != "":
		return fmt.Errorf("service-account issuer %s is not an https URL with a host and no query or fragment", issuer)
	}
	return nil
}

func (c Cluster) validateNode(n Node) error {
	switch c.Profile {
	case HCloud:
		if n.Hostname != "" {
			return fmt.Errorf("a Cloud node takes its name from Hetzner's metadata service, so hostname %q is refused", n.Hostname)
		}
	case Metal:
		if err := check.DNS1123Label(n.Hostname); err != nil {
			return fmt.Errorf("a dedicated node needs its own hostname: %w", err)
		}
	default:
		return fmt.Errorf("profile %q is neither %q nor %q", c.Profile, HCloud, Metal)
	}
	return nil
}

// imageMode validates a configuration that boots from a disk image, which needs no install section.
type imageMode struct{}

func (imageMode) String() string        { return "cloud" }
func (imageMode) RequiresInstall() bool { return false }
func (imageMode) InContainer() bool     { return false }

// replacedKinds are generated documents the kit drops, as documents() replaces them or the cluster runs without them.
var replacedKinds = []string{"KubeFlannelCNIConfig", "DiscoveryServiceConfig", "ResolverConfig", "HostnameConfig", "VolumeConfig"}

func keepGenerated(doc config.Document) bool {
	return !slices.Contains(replacedKinds, doc.Kind())
}
