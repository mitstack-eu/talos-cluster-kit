// Package report renders the node report, which a CronJob in the cluster posts to the platform.
package report

import (
	"bytes"
	"embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

var (
	//go:embed report.yaml
	reportTemplate string
	//go:embed mesh-probe.yaml
	meshProbeTemplate string
	//go:embed admission-probe.yaml
	admissionProbeTemplate string
	//go:embed security-scan.yaml
	securityScanTemplate string
	//go:embed admission/*.yaml
	admissionFixtures embed.FS
)

var (
	manifests               = tmpl.Must("the node report", reportTemplate)
	meshProbeManifests      = tmpl.Must("the mesh probe", meshProbeTemplate)
	admissionProbeManifests = tmpl.Must("the admission probe", admissionProbeTemplate)
	securityScanManifests   = tmpl.Must("the security scan", securityScanTemplate)
)

// The report dry-runs each fixture of one ConfigMap in the probe namespace, and reads what the scan last kept from another.
const (
	admissionProbeNamespace = "mitstack-admission-probe"
	admissionFixturesName   = "admission-fixtures"
	securityScanNamespace   = "kube-system"
	securityScansConfigMap  = "mitstack-security-scans"
)

// fixture is one admission fixture, under the name of the rule it breaks.
type fixture struct {
	Rule, Manifest string
}

// fixtures reads each embedded fixture as one line of JSON, in the order of its rule's name. A fixture's images exist only
// to be refused, and the gateway mirrors every image line of a manifest, so the ConfigMap carries none.
func fixtures() ([]fixture, error) {
	entries, err := admissionFixtures.ReadDir("admission")
	if err != nil {
		return nil, err
	}
	out := make([]fixture, 0, len(entries))
	for _, entry := range entries {
		data, err := admissionFixtures.ReadFile("admission/" + entry.Name())
		if err != nil {
			return nil, err
		}
		var manifest map[string]any
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("admission fixture %s: %w", entry.Name(), err)
		}
		line, err := json.Marshal(manifest)
		if err != nil {
			return nil, fmt.Errorf("admission fixture %s: %w", entry.Name(), err)
		}
		out = append(out, fixture{Rule: strings.TrimSuffix(entry.Name(), ".yaml"), Manifest: string(line)})
	}
	return out, nil
}

// TokenSecret is the Secret the block writes into the report's namespace, holding the bearer token under the key token.
const TokenSecret = "mitstack-report-token"

// The outside probe runs in kube-system, which the mesh leaves out, and writes its calls' outcomes into one ConfigMap:
// the mesh probe's target's and each gateway mirror's, since ztunnel would turn a mirror's closed port into a reset.
const (
	outsideProbeNamespace = "kube-system"
	outsideProbeConfigMap = "mitstack-mesh-probe"
)

// Inputs are what one cluster's report needs.
type Inputs struct {
	// URL is the platform endpoint the report posts to, over HTTPS.
	URL string
	// SubscriptionID names the cluster's subscription in every report.
	SubscriptionID string
	// ToolsImage is the image built from tools/Dockerfile, by digest, which carries the report's and its probe's scripts.
	ToolsImage string
	// ProofScript signs each report; a block passes block-kit's reportproof.TokenRequestScript, so this kit needs no framework.
	ProofScript string
	// Gateways are the gateways' private addresses, whose mirrors the outside probe reads under GatewayCA.
	Gateways []netip.Addr
	// GatewayCA is the PEM certificate of the CA that signs every gateway's certificate, without its key.
	GatewayCA []byte
}

// egressURL is Let's Encrypt's ACME directory, which cert-manager reaches for every app's certificate, so a TLS
// connection to it reads the egress the network's default route gives the nodes.
const egressURL = "https://acme-v02.api.letsencrypt.org/directory"

// gateways names each gateway as address=URL, the URL its mirror's certificate holds.
func (in Inputs) gateways() string {
	named := make([]string, 0, len(in.Gateways))
	for _, addr := range in.Gateways {
		named = append(named, addr.String()+"=https://"+addr.String())
	}
	return strings.Join(named, " ")
}

var subscriptionIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// openBaoMembers names each OpenBao member as name=URL, at the name its certificate holds on the headless Service.
func openBaoMembers() string {
	members := make([]string, components.OpenBaoMembers)
	for i := range members {
		members[i] = fmt.Sprintf("openbao-%d=https://openbao-%d.openbao-internal.openbao.svc:8200", i, i)
	}
	return strings.Join(members, " ")
}

// Manifests renders the report's namespace, identities and CronJob, and the outside probe whose result it carries.
func Manifests(in Inputs) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	report, err := tmpl.Execute(manifests, map[string]string{
		"Namespace":          nodeconfig.TalosAPINamespace,
		"Image":              in.ToolsImage,
		"ReportURL":          in.URL,
		"SubscriptionID":     in.SubscriptionID,
		"TokenSecret":        TokenSecret,
		"Script":             tools.ScriptDir + "/report.sh",
		"ProofScript":        in.ProofScript,
		"MeshProbeURL":       fmt.Sprintf("http://%s:%d/", components.MeshProbeHost, components.MeshProbePort),
		"MeshProbeAnswer":    components.MeshProbeAnswer,
		"MeshProbeIdentity":  components.MeshProbeIdentity,
		"MeshProbeConfigMap": outsideProbeNamespace + "/" + outsideProbeConfigMap,
		"OpenBaoMembers":     openBaoMembers(),
		"Gateways":           in.gateways(),
		"EgressURL":          egressURL,
	})
	if err != nil {
		return nil, err
	}
	probe, err := tmpl.Execute(meshProbeManifests, map[string]any{
		"Namespace":       outsideProbeNamespace,
		"ConfigMap":       outsideProbeConfigMap,
		"ReportNamespace": nodeconfig.TalosAPINamespace,
		"Image":           in.ToolsImage,
		"Script":          tools.ScriptDir + "/mesh-probe.sh",
		"Host":            components.MeshProbeHost,
		"Port":            components.MeshProbePort,
		"TargetNamespace": components.MeshProbeNamespace,
		"Gateways":        in.gateways(),
		"GatewayAddrs":    in.Gateways,
		"GatewayCA":       string(in.GatewayCA),
	})
	if err != nil {
		return nil, err
	}
	probeFixtures, err := fixtures()
	if err != nil {
		return nil, err
	}
	admission, err := tmpl.Execute(admissionProbeManifests, map[string]any{
		"Namespace":       admissionProbeNamespace,
		"ConfigMap":       admissionFixturesName,
		"ReportNamespace": nodeconfig.TalosAPINamespace,
		"Fixtures":        probeFixtures,
	})
	if err != nil {
		return nil, err
	}
	scan, err := tmpl.Execute(securityScanManifests, map[string]any{
		"Namespace":        securityScanNamespace,
		"ConfigMap":        securityScansConfigMap,
		"ReportNamespace":  nodeconfig.TalosAPINamespace,
		"Image":            in.ToolsImage,
		"Script":           tools.ScriptDir + "/security-scan.sh",
		"ScriptDir":        tools.ScriptDir,
		"KubeBenchVersion": tools.Pin("KUBE_BENCH_VERSION"),
		"ControlsVersion":  tools.Pin("KUBESCAPE_CONTROLS_VERSION"),
	})
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{report, probe, admission, scan}, []byte("---\n")), nil
}

func (in Inputs) validate() error {
	u, err := url.Parse(in.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("report URL %q is not an https URL", in.URL)
	}
	if !subscriptionIDRe.MatchString(in.SubscriptionID) {
		return fmt.Errorf("subscription id %q is not a label value", in.SubscriptionID)
	}
	if err := check.DigestRef(in.ToolsImage); err != nil {
		return fmt.Errorf("tools image: %w", err)
	}
	if strings.TrimSpace(in.ProofScript) == "" {
		return errors.New("the report has no proof script to sign it with")
	}
	if len(in.Gateways) == 0 {
		return nil
	}
	if block, rest := pem.Decode(in.GatewayCA); block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) > 0 {
		return errors.New("the gateway CA is not one PEM certificate, so the report could not read the gateways' mirrors")
	}
	return nil
}
