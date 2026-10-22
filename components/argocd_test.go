package components_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressingAndAStuckOneAsDegraded(t *testing.T) {
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cm")
	check, ok := testutil.Field(t, cm, "data", "resource.customizations.health.postgresql.cnpg.io_Cluster").(string)
	require.True(t, ok, "argocd-cm holds no health check for postgresql.cnpg.io/Cluster")
	const hibernated = `{{type = "cnpg.io/hibernation", status = "True", message = "Cluster has been hibernated"}}`
	for _, tc := range []struct{ phase, conditions, want string }{
		{"Switchover in progress", "{}", "Progressing"},
		{"Failing over", "{}", "Progressing"},
		{"Upgrading Postgres major version", "{}", "Progressing"},
		{"Online upgrade in progress", "{}", "Progressing"},
		{"Applying configuration", "{}", "Progressing"},
		{"Primary instance is being restarted in-place", "{}", "Progressing"},
		{"Primary instance is being restarted without a switchover", "{}", "Progressing"},
		{"Cluster in healthy state", "{}", "Healthy"},
		{"Waiting for user action", "{}", "Degraded"},
		{"Cluster in healthy state", hibernated, "Suspended"},
	} {
		t.Run(tc.phase+" "+tc.conditions, func(t *testing.T) {
			l := lua.NewState()
			defer l.Close()
			require.NoError(t, l.DoString(`obj = {status = {phase = "`+tc.phase+`", conditions = `+tc.conditions+`}}`))

			require.NoError(t, l.DoString(check))

			assert.Equal(t, tc.want, l.Get(-1).(*lua.LTable).RawGetString("status").String())
		})
	}
}
