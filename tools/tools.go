// Package tools holds the talosctl and kubectl pins that the tools image and the gateway both install, the jq pin
// the tools image alone installs, and the tools image a release of this kit published.
package tools

import (
	_ "embed"
	"strings"
)

// Env is tools.env, the one file that pins both binaries, as the Dockerfile and the gateway's prepare.sh source it.
//
//go:embed tools.env
var Env string

// jqEnv is jq.env, kept out of Env because the gateway runs no jq.
//
//go:embed jq.env
var jqEnv string

// Image is the tools image this release published and signed, which the release job writes into the commit it tags.
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.20.0@sha256:85b6d91998c74b405f55a13e2b0afba00433dd53fa5048462f563802f67f5b6d"

// Pin returns the value tools.env or jq.env gives name, or "" when neither names it.
func Pin(name string) string {
	for _, line := range strings.Split(Env+jqEnv, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}
