// Package report renders the node report, which a CronJob in the cluster posts to the platform.
package report

import (
	"bytes"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

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
)

var (
	manifests          = tmpl.Must("the node report", reportTemplate)
	meshProbeManifests = tmpl.Must("the mesh probe", meshProbeTemplate)
)

// TokenSecret is the Secret the block writes into the report's namespace, holding the bearer token under the key token.
const TokenSecret = "mitstack-report-token"

// The outside probe runs in kube-system, which the mesh leaves out, and writes its calls' outcomes into one ConfigMap.
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
	// Gateways are the gateways' private addresses, whose mirrors each report reads under GatewayCA.
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
		"GatewayCA":          string(in.GatewayCA),
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
	})
	if err != nil {
		return nil, err
	}
	return append(append(report, "---\n"...), probe...), nil
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
