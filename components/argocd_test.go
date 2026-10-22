package components_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// luaValue converts a decoded YAML or JSON value into the Lua value Argo CD hands a health check.
func luaValue(l *lua.LState, v any) lua.LValue {
	switch v := v.(type) {
	case map[string]any:
		table := l.NewTable()
		for k, item := range v {
			table.RawSetString(k, luaValue(l, item))
		}
		return table
	case []any:
		table := l.NewTable()
		for _, item := range v {
			table.Append(luaValue(l, item))
		}
		return table
	case string:
		return lua.LString(v)
	case bool:
		return lua.LBool(v)
	case nil:
		return lua.LNil
	default:
		panic("a fixture holds a value this test does not convert")
	}
}

// clusterHealth runs the argocd-cm health check for a CloudNativePG Cluster against obj, as Argo CD does.
func clusterHealth(t *testing.T, obj map[string]any) string {
	t.Helper()
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cm")
	script, ok := testutil.Field(t, cm, "data", "resource.customizations.health.postgresql.cnpg.io_Cluster").(string)
	require.True(t, ok, "argocd-cm holds no health check for postgresql.cnpg.io/Cluster")
	l := lua.NewState()
	defer l.Close()
	l.SetGlobal("obj", luaValue(l, obj))

	require.NoError(t, l.DoString(script))

	result, ok := l.Get(-1).(*lua.LTable)
	require.True(t, ok, "the health check returned %v, not a table", l.Get(-1))
	return result.RawGetString("status").String()
}

func cluster(phase string, conditions ...any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "shared"},
		"status":   map[string]any{"phase": phase, "phaseReason": "", "conditions": append([]any{}, conditions...)},
	}
}

func TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressing(t *testing.T) {
	for _, phase := range []string{
		"Switchover in progress",
		"Failing over",
		"Upgrading Postgres major version",
		"Online upgrade in progress",
		"Applying configuration",
		"Primary instance is being restarted in-place",
		"Primary instance is being restarted without a switchover",
		"Setting up primary",
	} {
		t.Run(phase, func(t *testing.T) {
			assert.Equal(t, "Progressing", clusterHealth(t, cluster(phase)))
		})
	}
}

func TestArgoCD_ReadsACloudNativePGClusterAsHealthyDegradedOrSuspendedAsItsBuiltInCheckDoes(t *testing.T) {
	hibernated := map[string]any{"type": "cnpg.io/hibernation", "status": "True", "message": "Cluster has been hibernated"}
	for _, tc := range []struct {
		name string
		obj  map[string]any
		want string
	}{
		{"healthy", cluster("Cluster in healthy state"), "Healthy"},
		{"waiting for the user", cluster("Waiting for user action"), "Degraded"},
		{"unrecoverable", cluster("Cluster is unrecoverable and needs manual intervention"), "Suspended"},
		{"hibernated", cluster("Cluster in healthy state", hibernated), "Suspended"},
		{"an unknown phase", cluster("A phase CloudNativePG adds later"), "Unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, clusterHealth(t, tc.obj))
		})
	}
}
