// Package tools holds the talosctl and kubectl pins that the tools image carries and the gateway copies out of it, the
// jq pin the tools image alone installs, and the tools image a release of this kit published.
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

// ScriptDir is where the tools image carries the scripts the kit's Jobs and CronJobs run, so a node's machine
// configuration, which the builder's user data caps, carries none of them.
const ScriptDir = "/usr/local/lib/mitstack"

// ManifestPath is where the tools image carries the manifest the repository holds at path, which a gateway copies out.
func ManifestPath(path string) string { return ScriptDir + "/" + path }

// Image is the tools image this release published and signed, which the release job writes into the commit it tags.
const Image = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.61.0@sha256:df4b772ab6d98a2b4877820bfbb4feb541501c9703aaa83716e5d9d3e2c483e0"

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
