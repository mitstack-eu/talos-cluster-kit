package talosclusterkit

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

type renderedDoc struct {
	doc    map[string]any
	source string
}

func (d renderedDoc) String() string {
	metadata, _ := d.doc["metadata"].(map[string]any)
	return fmt.Sprintf("%s: %v %v", d.source, d.doc["kind"], metadata["name"])
}

func renderedDocs(t *testing.T) []renderedDoc {
	t.Helper()
	var docs []renderedDoc
	for source, manifest := range renderedManifests(t) {
		for _, doc := range testutil.Documents(t, manifest) {
			docs = append(docs, renderedDoc{doc, source})
		}
	}
	return docs
}

func TestManifests_NoWebhookConfigurationFailsOpen(t *testing.T) {
	var open []string
	for _, d := range renderedDocs(t) {
		if d.doc["kind"] != "ValidatingWebhookConfiguration" && d.doc["kind"] != "MutatingWebhookConfiguration" {
			continue
		}
		for _, w := range d.doc["webhooks"].([]any) {
			if policy := w.(map[string]any)["failurePolicy"]; policy != "Fail" {
				open = append(open, fmt.Sprintf("%s webhook %v has failurePolicy %v", d, w.(map[string]any)["name"], policy))
			}
		}
	}

	slices.Sort(open)
	assert.Empty(t, open, "Argo CD compares the rendered policy, and a controller rewrites a policy of Ignore to Fail once it is ready")
}

func TestManifests_AnAutoscalerNeedsTheMetricsAPIItReadsAndIstiodRunsOneReplicaWithout(t *testing.T) {
	var autoscalers []string
	servesMetrics := false
	for _, d := range renderedDocs(t) {
		switch d.doc["kind"] {
		case "HorizontalPodAutoscaler":
			autoscalers = append(autoscalers, d.String())
		case "APIService":
			servesMetrics = servesMetrics || testutil.Field(t, d.doc, "spec", "group") == "metrics.k8s.io"
		}
	}
	istiod := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "istiod", components.Default().Served).Bytes), "Deployment")[0]

	assert.True(t, servesMetrics || len(autoscalers) == 0, "the set renders %v and registers no APIService for metrics.k8s.io, so each reads Degraded", autoscalers)
	if !servesMetrics {
		assert.EqualValues(t, 1, istiod["spec"].(map[string]any)["replicas"], "without a replica count the Deployment is left to an autoscaler the cluster cannot serve")
	}
}

func TestManifests_DeclareNoEmptyLabelsOrAnnotationsMap(t *testing.T) {
	var empty []string
	for _, d := range renderedDocs(t) {
		metadata, _ := d.doc["metadata"].(map[string]any)
		for _, key := range []string{"labels", "annotations"} {
			if m, ok := metadata[key].(map[string]any); ok && len(m) == 0 {
				empty = append(empty, fmt.Sprintf("%s declares empty %s", d, key))
			}
		}
	}

	slices.Sort(empty)
	assert.Empty(t, empty, "the API server stores no empty map, so the live object never matches the committed one")
}

func TestDropEmptyMetadataMaps(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"drops a map written on two lines", "metadata:\n  labels:\n    {}\n  annotations:\n    {}\n  name: a\n", "metadata:\n  name: a\n"},
		{"drops a map written inline in each document", "---\nmetadata:\n  labels: {}\n  name: a\n---\nmetadata:\n  name: b\n  annotations: {}\nspec: {}\n", "---\nmetadata:\n  name: a\n---\nmetadata:\n  name: b\nspec: {}\n"},
		{"keeps a filled map", "metadata:\n  labels:\n    app: a\n  annotations:\n    {}\n  name: a\n", "metadata:\n  labels:\n    app: a\n  name: a\n"},
		{"keeps an empty map below the top-level metadata", "metadata:\n  name: a\nspec:\n  labels: {}\n  template:\n    metadata:\n      labels:\n        {}\n      annotations: {}\n  properties:\n    labels: {}\ndata:\n  script: |\n    metadata:\n      labels: {}\n", ""},
		{"keeps a null map before the next document", "metadata:\n  name: a\n  labels:\n---\nkind: B\n", ""},
		{"keeps a null map at the end of the file", "metadata:\n  name: a\n  labels:\n", ""},
		{"keeps a null map before another key", "metadata:\n  annotations:\n  name: a\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "rendered.yaml")
			require.NoError(t, os.WriteFile(file, []byte(tc.in), 0o644))

			got, err := testutil.RunBash(t, `. "$1" && drop_empty_metadata_maps "$2" && cat "$2"`, os.Environ(), "scripts/lib/empty-maps.sh", file)

			require.NoError(t, err, got)
			if tc.want == "" {
				tc.want = tc.in
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
