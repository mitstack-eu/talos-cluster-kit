package testutil

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Denies evaluates a DENY AuthorizationPolicy for a call from principal to port, reading the ports, notPorts and
// notPrincipals forms the kit's policies use, where a rule denies when every condition it names holds.
func Denies(t *testing.T, policy map[string]any, principal string, port int) bool {
	t.Helper()
	require.Equal(t, "DENY", Field(t, policy, "spec", "action"))
	has := func(list any, value string) bool { l, _ := list.([]any); return slices.Contains(l, any(value)) }
	for _, rule := range Field(t, policy, "spec", "rules").([]any) {
		r := rule.(map[string]any)
		if to, ok := r["to"]; ok {
			op := Field(t, to, "0", "operation").(map[string]any)
			if ports, ok := op["ports"]; ok && !has(ports, strconv.Itoa(port)) {
				continue
			}
			if has(op["notPorts"], strconv.Itoa(port)) {
				continue
			}
		}
		if from, ok := r["from"]; ok && has(Field(t, from, "0", "source", "notPrincipals"), principal) {
			continue
		}
		return true
	}
	return false
}
