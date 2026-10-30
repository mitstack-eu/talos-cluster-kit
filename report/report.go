// Package report renders the node report, which a CronJob in the cluster posts to the platform.
package report

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

var (
	//go:embed report.yaml
	reportTemplate string
	//go:embed scripts/report.sh
	reportScript string
)

var manifests = tmpl.Must("the node report", reportTemplate)

// TokenSecret is the Secret the block writes into the report's namespace, holding the bearer token under the key token.
const TokenSecret = "mitstack-report-token"

// Inputs are what one cluster's report needs.
type Inputs struct {
	// URL is the platform endpoint the report posts to, over HTTPS.
	URL string
	// SubscriptionID names the cluster's subscription in every report.
	SubscriptionID string
	// ToolsImage is the image built from tools/Dockerfile, by digest.
	ToolsImage string
	// ProofScript signs each report; a block passes block-kit's reportproof.TokenRequestScript, so this kit needs no framework.
	ProofScript string
}

var subscriptionIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// Manifests renders the report's namespace, identities and CronJob.
func Manifests(in Inputs) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	return tmpl.Execute(manifests, map[string]string{
		"Namespace":      nodeconfig.TalosAPINamespace,
		"Image":          in.ToolsImage,
		"ReportURL":      in.URL,
		"SubscriptionID": in.SubscriptionID,
		"TokenSecret":    TokenSecret,
		"Script":         reportScript,
		"ProofScript":    in.ProofScript,
	})
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
