package components_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// renderedDocuments are the documents of every manifest the default and hosting sets ship, each under its manifest's name.
func renderedDocuments(t *testing.T) map[string][]map[string]any {
	t.Helper()
	set := components.HostingServed()
	docs := map[string][]map[string]any{}
	for _, m := range slices.Concat(set.Served, set.Inline) {
		docs[m.Name] = testutil.Documents(t, m.Bytes)
	}
	return docs
}

func objectName(doc map[string]any) string {
	metadata, _ := doc["metadata"].(map[string]any)
	if namespace, ok := metadata["namespace"]; ok {
		return fmt.Sprintf("%v %v/%v", doc["kind"], namespace, metadata["name"])
	}
	return fmt.Sprintf("%v %v", doc["kind"], metadata["name"])
}

func TestComponentSet_RendersNoWebhookConfigurationThatFailsOpen(t *testing.T) {
	var failingOpen []string
	for name, docs := range renderedDocuments(t) {
		for _, doc := range docs {
			if doc["kind"] != "ValidatingWebhookConfiguration" && doc["kind"] != "MutatingWebhookConfiguration" {
				continue
			}
			webhooks, _ := doc["webhooks"].([]any)
			for _, w := range webhooks {
				webhook := w.(map[string]any)
				if webhook["failurePolicy"] != "Fail" {
					failingOpen = append(failingOpen, fmt.Sprintf("%s: %s webhook %v has failurePolicy %v", name, objectName(doc), webhook["name"], webhook["failurePolicy"]))
				}
			}
		}
	}

	slices.Sort(failingOpen)
	assert.Empty(t, failingOpen, "Argo CD compares the rendered policy, and a controller rewrites a policy of Ignore to Fail once it is ready")
}

func TestComponentSet_RendersAHorizontalPodAutoscalerOnlyWhereItRegistersTheMetricsAPI(t *testing.T) {
	var autoscalers []string
	registersMetricsAPI := false
	for name, docs := range renderedDocuments(t) {
		for _, doc := range docs {
			switch doc["kind"] {
			case "HorizontalPodAutoscaler":
				autoscalers = append(autoscalers, name+": "+objectName(doc))
			case "APIService":
				registersMetricsAPI = registersMetricsAPI || testutil.Field(t, doc, "spec", "group") == "metrics.k8s.io"
			}
		}
	}

	slices.Sort(autoscalers)
	if len(autoscalers) > 0 {
		assert.True(t, registersMetricsAPI, "the set renders %v and registers no APIService for metrics.k8s.io, so each reads Degraded", autoscalers)
	}
}

func TestComponentSet_RunsIstiodAtOneFixedReplica(t *testing.T) {
	istiod := namedDocument(t, manifest(t, "istiod"), "Deployment", "istiod")

	assert.EqualValues(t, 1, istiod["spec"].(map[string]any)["replicas"], "without a replica count the Deployment is left to an autoscaler the cluster cannot serve")
}

func TestComponentSet_DeclaresNoEmptyLabelsOrAnnotationsMap(t *testing.T) {
	var empty []string
	for name, docs := range renderedDocuments(t) {
		for _, doc := range docs {
			metadata, _ := doc["metadata"].(map[string]any)
			for _, key := range []string{"labels", "annotations"} {
				if m, ok := metadata[key].(map[string]any); ok && len(m) == 0 {
					empty = append(empty, fmt.Sprintf("%s: %s declares empty %s", name, objectName(doc), key))
				}
			}
		}
	}

	slices.Sort(empty)
	assert.Empty(t, empty, "the API server stores no empty map, so the live object never matches the committed one")
}

const emptyMapsLib = "../scripts/lib/empty-maps.sh"

func dropEmptyMaps(t *testing.T, input string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "rendered.yaml")
	require.NoError(t, os.WriteFile(file, []byte(input), 0o644))
	out, err := testutil.RunBash(t, `. "$1" && drop_empty_metadata_maps "$2" && cat "$2"`, os.Environ(), emptyMapsLib, file)
	require.NoError(t, err, out)
	return out
}

func TestDropEmptyMetadataMaps_RemovesAnEmptyLabelsOrAnnotationsMapFromEachDocumentsMetadata(t *testing.T) {
	input := `---
apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    {}
  annotations:
    {}
  name: first
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  labels: {}
  name: second
  annotations: {}
spec:
  replicas: 1
`

	got := dropEmptyMaps(t, input)

	assert.Equal(t, `---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: first
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: second
spec:
  replicas: 1
`, got)
}

func TestDropEmptyMetadataMaps_KeepsEveryOtherEmptyMapAndEveryFilledOne(t *testing.T) {
	input := `---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: kept
  annotations:
    {}
  name: first
spec:
  labels: {}
  template:
    metadata:
      labels:
        {}
      annotations: {}
  schema:
    properties:
      labels: {}
      annotations:
        {}
data:
  script: |
    metadata:
      labels: {}
`

	got := dropEmptyMaps(t, input)

	assert.Equal(t, `---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: kept
  name: first
spec:
  labels: {}
  template:
    metadata:
      labels:
        {}
      annotations: {}
  schema:
    properties:
      labels: {}
      annotations:
        {}
data:
  script: |
    metadata:
      labels: {}
`, got)
}
