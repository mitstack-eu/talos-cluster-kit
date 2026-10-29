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
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.25.0@sha256:5311eddd2a02812c748b26e61ed01bd9d5050cdc1eed86c4c96699eb7e5bc680"

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
