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

// scanEnv is scan.env, which pins the scanners and the NSA framework the tools image carries for the security scan.
//
//go:embed scan.env
var scanEnv string

// ScriptDir is where the tools image carries the scripts the kit's CronJobs run, so a node's machine configuration, which
// the builder's user data caps, carries none of them.
const ScriptDir = "/usr/local/lib/mitstack"

// Image is the tools image this release published and signed, which the release job writes into the commit it tags.
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.42.0@sha256:fca366e9f5bde2db8135fe4df0070ef40e1eff381236000742f3374b9cc88413"

// Pin returns the value tools.env, jq.env or scan.env gives name, or "" when none names it.
func Pin(name string) string {
	for _, line := range strings.Split(Env+jqEnv+scanEnv, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}
