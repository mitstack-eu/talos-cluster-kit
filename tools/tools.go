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
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.15.0@sha256:7ea47f2a8e6b8ac07eadf224d07314d528d97edc66cef1c8bfa2236aedbc8fb3"

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
