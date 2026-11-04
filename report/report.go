// Package report renders the node report, which a CronJob in the cluster posts to the platform.
package report

import (
	_ "embed"
	"errors"
	"fmt"
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
}

var subscriptionIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

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
	return nil
}
