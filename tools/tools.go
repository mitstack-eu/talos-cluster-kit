// Package tools holds the talosctl and kubectl pins that the tools image and the gateway both install.
package tools

import (
	_ "embed"
	"strings"
)

// Env is tools.env, the one file that pins both binaries, as the Dockerfile and the gateway's prepare.sh source it.
//
//go:embed tools.env
var Env string

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
