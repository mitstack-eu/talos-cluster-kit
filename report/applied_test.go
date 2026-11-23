package report_test

// Argo CD's health of each object of one kind an Application applied, and false when the report cannot judge it.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/report"
)

const cachesBody = `{"argocd": {"applications": [
  {"name": "component-mitstack-runner-caches", "health": "Progressing", "resources": [
    {"group": "apps", "kind": "Deployment", "namespace": "mitstack-runners", "name": "go-cache", "sync": "Synced", "health": "Healthy"},
    {"group": "apps", "kind": "Deployment", "namespace": "mitstack-runners", "name": "image-cache", "sync": "Synced", "health": "Progressing",
     "message": "Waiting for rollout to finish: 0 of 1 updated replicas are available..."},
    {"group": "", "kind": "Service", "namespace": "mitstack-runners", "name": "go-cache", "sync": "Synced", "health": "Healthy"},
    {"group": "", "kind": "ConfigMap", "namespace": "mitstack-runners", "name": "image-cache", "sync": "Synced", "health": null}]},
  {"name": "component-other", "resources": [
    {"group": "apps", "kind": "Deployment", "namespace": "other", "name": "npm-cache", "sync": "Synced", "health": "Degraded"}]}]}}`

func TestRead_AppliedResourcesReadsArgoCDsHealthOfEachObjectOfTheKindTheApplicationApplied(t *testing.T) {
	read := report.Read{Body: []byte(cachesBody), At: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}

	deployments, readable, err := read.AppliedResources("component-mitstack-runner-caches", "Deployment")

	require.NoError(t, err)
	assert.True(t, readable, "a report whose Application carries each object's health")
	assert.Equal(t, map[string]report.Applied{
		"go-cache":    {Health: "Healthy"},
		"image-cache": {Health: "Progressing", Message: "Waiting for rollout to finish: 0 of 1 updated replicas are available..."},
	}, deployments, "the Deployments of the caches' Application alone")
}

func TestRead_AppliedResourcesCannotJudgeAnApplicationTheReportLacksOrWhoseHealthArgoCDKeepsOffIt(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no such Application", `{"argocd": {"applications": [{"name": "component-other", "resources": []}]}}`},
		{"an Application with no resources read", `{"argocd": {"applications": [{"name": "component-mitstack-runner-caches"}]}}`},
		{"health kept off the Application", `{"argocd": {"applications": [{"name": "component-mitstack-runner-caches", "resources": [
			{"group": "apps", "kind": "Deployment", "namespace": "mitstack-runners", "name": "go-cache", "sync": "Synced"}]}]}}`},
		{"a report from before Argo CD was read", `{"pods": []}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, readable, err := report.Read{Body: []byte(tc.body)}.AppliedResources("component-mitstack-runner-caches", "Deployment")

			require.NoError(t, err)
			assert.False(t, readable, "whether the report judges the Application's Deployments")
		})
	}
}

func TestRead_AppliedResourcesFailsOnAReportItCannotRead(t *testing.T) {
	_, _, err := report.Read{Body: []byte(`{"argocd": {"applications": {}}}`)}.AppliedResources("component-mitstack-runner-caches", "Deployment")

	require.Error(t, err)
}
