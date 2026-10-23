// Package tools holds the talosctl and kubectl pins that the tools image and the gateway both install, and the
// tools image a release of this kit published.
package tools

import (
	_ "embed"
	"strings"
)

// Env is tools.env, the one file that pins both binaries, as the Dockerfile and the gateway's prepare.sh source it.
//
//go:embed tools.env
var Env string

// Image is the tools image this release published and signed, which the release job writes into the commit it tags.
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.8.0@sha256:7c584a98ad3cb896594d4e69b178c227c00401f426a84fb859bf61b301ba8dfa"

// Pin returns the value tools.env gives name, or "" when it names none.
func Pin(name string) string {
	for _, line := range strings.Split(Env, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}
