// Package gateway renders the cloud-config of a cluster's gateways, one in each location, the only servers with a public address.
package gateway

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

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

const httpsPort = 443

const (
	registryImage = "docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	caddyImage    = "docker.io/library/caddy:2.10.2-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d"
	// skopeoImage copies the mirror, since Ubuntu 24.04's skopeo 1.13.3 cannot copy an OCI index with its digests kept.
	skopeoImage = "quay.io/skopeo/stable:v1.20.0@sha256:47853bb9fb24202af9110531ebd6e43c5f97701254ca290596640290d17942f4"
)

// Each stage of a gateway's build runs under its own bound, which tools.env and the units that run the stages carry,
// and a gateway that fails or overruns a stage powers itself off.
const (
	downloadConnectBound = 15 * time.Second
	// downloadBound bounds each tools and manifest download whole.
	downloadBound = 5 * time.Minute
	// copyBound bounds each image copy into the mirror, skopeo's own retries included.
	copyBound = 15 * time.Minute
	// prepareBound bounds the whole preparation, every download and copy included.
	prepareBound = time.Hour
	// serveBound bounds the TLS server's start, from the mirror's completion to its first answer on the private network.
	serveBound = 2 * time.Minute
	// bootBound is what the plane allows from a gateway's creation to the start of its preparation: its power-on, boot
	// and package install, which the gateway does not bound itself.
	bootBound = 15 * time.Minute
)

// Deadline is how long after its creation a gateway either serves or has powered itself off, the sum of its bounds.
const Deadline = bootBound + prepareBound + serveBound

// A gateway has no login, so a failed build writes why to consolePath, the VGA console Hetzner's web console shows and
// /dev/console may not reach, and to issuePath, which the login prompt shows again on every later boot.
const (
	consolePath = "/dev/tty1"
	issuePath   = "/etc/issue.d/mitstack-gateway.issue"
)

var (
	//go:embed cloud-config.yaml
	cloudConfigTemplate string
	//go:embed scripts/prepare.sh
	prepareScript string
)

var cloudConfig = tmpl.Must("the gateway's cloud-config", cloudConfigTemplate)

type Inputs struct {
	// Address is the gateway's private address, which the network's default route points at.
	Address netip.Addr
	Network netip.Prefix
	// Ingress is the ingress load balancer's public address, the one host the nodes reach on port 80; zero for none.
	Ingress netip.Addr
	TLS     TLS
	// List is what the gateway mirrors and serves, as NewList makes it from the cluster's component set.
	List List
	// ManifestBaseURL is the release of this kit the served manifests are fetched from, as ReleaseURL returns.
	ManifestBaseURL string
	// Logs is the receiver the node-local Collectors export to, whose port the gateway forwards when there is one.
	Logs components.LogExport
	// Gzipped says the caller sends the cloud-config gzipped and judges that against Hetzner's limit itself.
	Gzipped bool
}

func ReleaseURL(version string) string {
	return "https://raw.githubusercontent.com/mitstack-eu/talos-cluster-kit/" + version
}

// Render returns the gateway's cloud-config, refusing one past Hetzner's user data limit unless it travels gzipped.
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
	if !in.Gzipped && len(out) > UserDataLimit {
		return nil, fmt.Errorf("the gateway's cloud-config is %d bytes, over Hetzner's %d", len(out), UserDataLimit)
	}
	return out, nil
}

func (in Inputs) validate() error {
	var errs []error
	if !in.Network.IsValid() || !in.Network.Contains(in.Address) {
		errs = append(errs, fmt.Errorf("address %s is not inside network %s", in.Address, in.Network))
	}
	if in.Ingress.IsValid() && in.Network.Contains(in.Ingress) {
		errs = append(errs, fmt.Errorf("ingress address %s is inside network %s", in.Ingress, in.Network))
	}
	if err := in.TLS.validate(); err != nil {
		errs = append(errs, err)
	}
	if in.List.digest == "" {
		errs = append(errs, errors.New("the gateway has no list; build one with NewList"))
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
	ports, err := in.egressPorts()
	if err != nil {
		return nil, err
	}
	var ingress string
	if in.Ingress.IsValid() {
		ingress = in.Ingress.String()
	}
	return map[string]string{
		"Address":         in.Address.String(),
		"Network":         in.Network.String(),
		"Resolvers":       joinAddrs(nodeconfig.HetznerResolvers),
		"EgressPorts":     ports,
		"Ingress":         ingress,
		"CA":              string(in.TLS.CA),
		"Cert":            string(in.TLS.Cert),
		"Key":             string(in.TLS.Key),
		"Images":          string(mirror.Render(in.List.mirrored())),
		"Manifests":       in.manifestList(),
		"ToolsEnv":        tools.Env + "SKOPEO_IMAGE=" + skopeoImage + "\n" + boundsEnv(),
		"PrepareSeconds":  seconds(prepareBound),
		"ServeSeconds":    seconds(serveBound),
		"Console":         consolePath,
		"Issue":           issuePath,
		"PrepareScript":   prepareScript,
		"BootstrapScript": string(etcd.BootstrapScript()),
		"RegistryImage":   registryImage,
		"CaddyImage":      caddyImage,
	}, nil
}

// boundsEnv is the lines of tools.env that bound each download and copy prepare.sh runs, and name where it writes a
// failure.
func boundsEnv() string {
	return "DOWNLOAD_CONNECT_SECONDS=" + seconds(downloadConnectBound) + "\nDOWNLOAD_SECONDS=" + seconds(downloadBound) +
		"\nCOPY_SECONDS=" + seconds(copyBound) + "\nGATEWAY_CONSOLE=" + consolePath + "\nGATEWAY_ISSUE=" + issuePath + "\n"
}

func seconds(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }

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

// List is the images a gateway mirrors and the manifests it serves, read out of a component set once, and the images it
// keeps beside them because the cluster still runs them.
type List struct {
	images []components.Image
	served []components.Manifest
	digest string
	kept   []components.Image
}

// Keeping is the list that also mirrors each of images it lacks, with its digest still the release's list alone, so a
// kept image never marks a gateway stale.
func (l List) Keeping(images []components.Image) List {
	var kept []components.Image
	for _, image := range images {
		if !slices.Contains(l.images, image) && !slices.Contains(kept, image) {
			kept = append(kept, image)
		}
	}
	slices.SortFunc(kept, func(a, b components.Image) int { return strings.Compare(a.Ref(), b.Ref()) })
	l.kept = kept
	return l
}

// Kept is each image the list mirrors beyond the release's, in reference order.
func (l List) Kept() []components.Image { return slices.Clone(l.kept) }

func (l List) mirrored() []components.Image {
	all := append(slices.Clone(l.images), l.kept...)
	slices.SortFunc(all, func(a, b components.Image) int { return strings.Compare(a.Ref(), b.Ref()) })
	return all
}

// NewList reads the images out of every manifest of set, which takes the better part of a second, so one list serves
// every gateway an install renders.
func NewList(set components.Set, extra ...components.Image) (List, error) {
	images, err := mirror.List(set, extra...)
	if err != nil {
		return List{}, err
	}
	sum := sha256.New()
	sum.Write(mirror.Render(images))
	for _, m := range set.Served {
		fmt.Fprintf(sum, "%s %s\n", m.TalosName(), m.SHA256())
	}
	digest := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
	return List{images: images, served: slices.Clone(set.Served), digest: digest}, nil
}

// Digest names the list's images and manifests by their digests, leaving out the release the manifests are fetched
// from, so a gateway built from it is replaced only when the pinned list changes.
func (l List) Digest() string { return l.digest }

// ListDigest is the digest of the list NewList makes from set and extra.
func ListDigest(set components.Set, extra ...components.Image) (string, error) {
	l, err := NewList(set, extra...)
	if err != nil {
		return "", err
	}
	return l.Digest(), nil
}

// manifestList is one "<file> <sha256> <url>" line per served manifest, the form prepare.sh reads.
func (in Inputs) manifestList() string {
	var lines []string
	for _, m := range in.List.served {
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
