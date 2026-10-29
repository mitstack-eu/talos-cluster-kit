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
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.24.0@sha256:4c7728b7d094b84d7c28eb3a89df1e6f812d9b8500f4e8eaa5b5fec1a32425b5"

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
