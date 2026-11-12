package components_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// clusterHealth is the health argocd-cm's check gives a CloudNativePG Cluster in phase whose conditions are the Lua table conditions.
func clusterHealth(t *testing.T, phase, conditions string) string {
	t.Helper()
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cm")
	check, ok := testutil.Field(t, cm, "data", "resource.customizations.health.postgresql.cnpg.io_Cluster").(string)
	require.True(t, ok, "argocd-cm holds no health check for postgresql.cnpg.io/Cluster")
	l := lua.NewState()
	defer l.Close()
	require.NoError(t, l.DoString(`obj = {status = {phase = "`+phase+`", conditions = `+conditions+`}}`))
	require.NoError(t, l.DoString(check))
	return l.Get(-1).(*lua.LTable).RawGetString("status").String()
}

func TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressingAndAStuckOneAsDegraded(t *testing.T) {
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
			assert.Equal(t, tc.want, clusterHealth(t, tc.phase, tc.conditions))
		})
	}
}

func TestArgoCD_ReadsNoFailedArchiveOrBackupAndNoPluginPhaseOfACloudNativePGClusterAsDegraded(t *testing.T) {
	for name, tc := range map[string]struct{ phase, conditions, want string }{
		"a healthy Cluster whose archiving fails": {"Cluster in healthy state",
			`{{type = "ContinuousArchiving", status = "False", reason = "ContinuousArchivingFailing", message = "no credentials"}}`, "Healthy"},
		"a healthy Cluster whose last backup failed": {"Cluster in healthy state",
			`{{type = "LastBackupSucceeded", status = "False", reason = "LastBackupFailed", message = "no credentials"}}`, "Healthy"},
		"a Cluster that names a plugin the operator cannot find": {"Cluster cannot proceed to reconciliation due to an unknown plugin being required", "{}", "Suspended"},
		"a Cluster whose plugin does not answer":                 {"Cluster cannot proceed to reconciliation due to an error while interacting with plugins", "{}", "Unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			got := clusterHealth(t, tc.phase, tc.conditions)

			assert.Equal(t, tc.want, got, "the node report alone judges a backup and a plugin, and a block fails a deploy on a Degraded object, so a database still waiting for its storage key would be undone")
		})
	}
}

func TestArgoCD_KeepsEachObjectsHealthOnItsApplicationWhereTheNodeReportReadsIt(t *testing.T) {
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cmd-params-cm")

	assert.Equal(t, "true", testutil.Field(t, cm, "data", "controller.resource.health.persist"), "Argo CD 3 keeps object health off the Application otherwise")
}

func TestArgoCD_StartsEveryClusterWithItsLocalAdminAccountOff(t *testing.T) {
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cm")

	enabled := testutil.Field(t, cm, "data", "admin.enabled")

	assert.Equal(t, "false", enabled, "argocd-cm's admin.enabled is %v; the chart turns the local admin account on unless the values set configs.cm.admin.enabled false", enabled)
}
