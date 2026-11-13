package components_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressingAStuckOneAsDegradedAndNoFailedBackupOrPluginPhaseAsDegraded(t *testing.T) {
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
		{"Cluster in healthy state", `{{type = "ContinuousArchiving", status = "False", message = "archive failed"}, {type = "LastBackupSucceeded", status = "False", reason = "LastBackupFailed", message = "backup failed"}}`, "Healthy"},
		{"Cluster cannot proceed to reconciliation due to an unknown plugin being required", "{}", "Suspended"},
		{"Cluster cannot proceed to reconciliation due to an error while interacting with plugins", "{}", "Unknown"},
	} {
		t.Run(tc.phase+" "+tc.conditions, func(t *testing.T) {
			l := lua.NewState()
			defer l.Close()
			require.NoError(t, l.DoString(`obj = {status = {phase = "`+tc.phase+`", conditions = `+tc.conditions+`}}`))

			require.NoError(t, l.DoString(check))

			assert.Equal(t, tc.want, l.Get(-1).(*lua.LTable).RawGetString("status").String(), "a block fails a deploy on a Degraded object, so the node report judges a backup and a plugin, and this check neither")
		})
	}
}

func TestArgoCD_KeepsEachObjectsHealthOnItsApplicationAndEndsASyncOperationAfterAnHour(t *testing.T) {
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cmd-params-cm")

	assert.Equal(t, "true", testutil.Field(t, cm, "data", "controller.resource.health.persist"), "Argo CD 3 keeps object health off the Application otherwise, and the node report reads it there")
	assert.Equal(t, "3600", testutil.Field(t, cm, "data", "controller.sync.timeout.seconds"), "the sync timeout the application controller reads as ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT; an operation that began without retry.refresh retries its first commit without end otherwise")
}

func TestArgoCD_StartsEveryClusterWithItsLocalAdminAccountOff(t *testing.T) {
	cm := namedDocument(t, manifest(t, "argocd"), "ConfigMap", "argocd-cm")

	enabled := testutil.Field(t, cm, "data", "admin.enabled")

	assert.Equal(t, "false", enabled, "argocd-cm's admin.enabled is %v; the chart turns the local admin account on unless the values set configs.cm.admin.enabled false", enabled)
}
