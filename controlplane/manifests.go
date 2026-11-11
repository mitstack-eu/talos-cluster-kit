package controlplane

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"text/template"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

var (
	//go:embed manifests/cluster-tokens.yaml
	clusterTokensTemplate string
	//go:embed manifests/cluster-application.yaml
	clusterApplicationTemplate string
)

// toolsImage is a variable so a test can stand in a fixed image, since every kit release pins tools.Image anew.
var toolsImage = func() string { return tools.Image }

var (
	clusterTokens      = template.Must(template.New("cluster-tokens").Parse(clusterTokensTemplate))
	clusterApplication = template.Must(template.New("cluster-application").Parse(clusterApplicationTemplate))
)

const (
	clusterReportsPath = "/v1/cluster-reports"
	manifestDir        = "manifests/"
	// clusterTokensScript is where the tools image carries the cluster's credentials script, which the builder's user data has no room for.
	clusterTokensScript = tools.ScriptDir + "/cluster-tokens.sh"
)

// clusterComponents carries the node report, the topology labeller, the enrolment and the Argo CD application inline, so the
// gateway mirrors their image.
func (p Plane) clusterComponents(set components.Set, enrolmentToken string) (components.Set, error) {
	own, data, err := p.ownComponents(enrolmentToken)
	if err != nil {
		return components.Set{}, err
	}
	application, err := execute(clusterApplication, data)
	if err != nil {
		return components.Set{}, err
	}
	set.Inline = append(slices.Clone(set.Inline), own...)
	set.Inline = append(set.Inline, components.Manifest{Name: clusterApplicationManifest, Path: manifestDir + "cluster-application.yaml", Bytes: application})
	return set, nil
}

// clusterApplicationManifest names the cluster's own Application, through which every later release reaches it.
const clusterApplicationManifest = "mitstack-cluster-application"

// ownComponents renders the node report, the topology labeller and the cluster's credentials, with the data the cluster's
// own Application renders from. The report reads each gateway's mirror under p.GatewayCA, so the plane must hold one.
func (p Plane) ownComponents(enrolmentToken string) ([]components.Manifest, map[string]any, error) {
	if p.GatewayCA == nil {
		return nil, nil, errors.New("the node report reads each gateway's mirror under the gateway CA, and the plane holds none")
	}
	reportManifests, err := report.Manifests(report.Inputs{URL: p.PlatformURL + clusterReportsPath, SubscriptionID: p.Subscription,
		ToolsImage: toolsImage(), ProofScript: p.ReportProofScript, Gateways: gatewayAddrs(), GatewayCA: p.GatewayCA.Cert})
	if err != nil {
		return nil, nil, err
	}
	labeller, err := p.topologyManifests()
	if err != nil {
		return nil, nil, err
	}
	data := map[string]any{
		"Namespace":            nodeconfig.TalosAPINamespace,
		"Image":                toolsImage(),
		"Script":               clusterTokensScript,
		"PlatformURL":          p.PlatformURL,
		"SubscriptionID":       p.Subscription,
		"RepoURL":              p.RepoURL,
		"EnrolmentToken":       enrolmentToken,
		"RepositoryCredential": "platform-token",
	}
	if p.Hosting != nil {
		data["RepoURL"] = components.RepositorySSHURL(p.Hosting.Repository)
		data["RepositoryCredential"] = "deploy-key"
	}
	tokens, err := execute(clusterTokens, data)
	if err != nil {
		return nil, nil, err
	}
	return []components.Manifest{
		{Name: "mitstack-node-report", Path: "report/report.yaml", Bytes: reportManifests},
		{Name: "mitstack-node-topology", Path: "topology/topology.yaml", Bytes: labeller},
		{Name: "mitstack-cluster-tokens", Path: manifestDir + "cluster-tokens.yaml", Bytes: tokens},
	}, data, nil
}

func execute(t *template.Template, data map[string]any) ([]byte, error) {
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", t.Name(), err)
	}
	return out.Bytes(), nil
}
