package report

import (
	"fmt"
	"net/url"
	"strings"
)

// githubAPIHost is GitHub's own API host, the one host the runner sweep sends the leased runner token to.
const githubAPIHost = "api.github.com"

// allowedGitHubHosts lists every host the runner sweep may dial, so no request or input chooses one.
func allowedGitHubHosts() []string { return []string{githubAPIHost} }

// trustedGitHubBase answers the allowlist's own entry for endpoint's host, never a base built from endpoint.
func trustedGitHubBase(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("the GitHub API %q is not an https URL", endpoint)
	}
	for _, allowed := range allowedGitHubHosts() {
		if strings.EqualFold(u.Host, allowed) {
			return "https://" + allowed, nil
		}
	}
	return "", fmt.Errorf("the GitHub API host %q is not one the runner sweep may send its token to: %v", u.Host, allowedGitHubHosts())
}
