package controlplane

import (
	"bytes"
	_ "embed"
	"fmt"
	"slices"
	"strings"
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
	//go:embed scripts/cluster-tokens.sh
	clusterTokensScript string
)

// toolsImage is a variable so a test can stand in a fixed image, since every kit release pins tools.Image anew.
var toolsImage = func() string { return tools.Image }

var templateFuncs = template.FuncMap{"indent": indent}

var (
	clusterTokens      = template.Must(template.New("cluster-tokens").Funcs(templateFuncs).Parse(clusterTokensTemplate))
	clusterApplication = template.Must(template.New("cluster-application").Funcs(templateFuncs).Parse(clusterApplicationTemplate))
)

const (
	clusterReportsPath = "/v1/cluster-reports"
	manifestDir        = "manifests/"
)

// clusterComponents carries the node report, the enrolment and the Argo CD application inline, so the gateway mirrors their image.
func (p Plane) clusterComponents(set components.Set, enrolmentToken string) (components.Set, error) {
	reportManifests, err := report.Manifests(report.Inputs{URL: p.PlatformURL + clusterReportsPath, SubscriptionID: p.Subscription, ToolsImage: toolsImage()})
	if err != nil {
		return components.Set{}, err
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
		return components.Set{}, err
	}
	application, err := execute(clusterApplication, data)
	if err != nil {
		return components.Set{}, err
	}
	set.Inline = append(slices.Clone(set.Inline),
		components.Manifest{Name: "mitstack-node-report", Path: "report/report.yaml", Bytes: reportManifests},
		components.Manifest{Name: "mitstack-cluster-tokens", Path: manifestDir + "cluster-tokens.yaml", Bytes: tokens},
		components.Manifest{Name: "mitstack-cluster-application", Path: manifestDir + "cluster-application.yaml", Bytes: application},
	)
	return set, nil
}

func execute(t *template.Template, data map[string]any) ([]byte, error) {
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", t.Name(), err)
	}
	return out.Bytes(), nil
}

func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = pad + line
		}
	}
	return strings.Join(lines, "\n")
}
