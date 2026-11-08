package report_test

// The node report lists each image reference its pods and its workloads' pod templates name once, with the digest it runs by.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/report"
)

func TestReport_ListsEachImageReferenceOnceWithItsDigestFromEveryPodAndTheTemplateOfEachDeploymentStatefulSetDaemonSetJobAndCronJob(t *testing.T) {
	_, posted, out, err := runReport(t, kubectlWith(podsRead, "cat testdata/image-pods.json"))
	require.NoError(t, err, out)

	assert.ElementsMatch(t, readJSON(t, "testdata", "golden", "images.json"), posted.body["images"],
		"a tag a pod's status resolves is listed with that digest alone, and one no pod resolves with none")
}

// crowdedPods writes 1000 running pods across 50 namespaces and six nodes, naming 300 distinct images by tag, each
// resolved to its digest by the pod's status.
func crowdedPods(t *testing.T) (path string, images []string) {
	t.Helper()
	items := make([]any, 0, 1000)
	for i := range 1000 {
		image := fmt.Sprintf("ghcr.io/example-org/service-%03d", i%300)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(image)))
		if i < 300 {
			images = append(images, image+":v1.4.2")
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": fmt.Sprintf("service-%03d-7d9c5b6f8-%04d", i%300, i), "namespace": fmt.Sprintf("app-%02d", i%50)},
			"spec": map[string]any{"nodeName": fmt.Sprintf("poc-talos4-w%d", i%6),
				"containers": []any{map[string]any{"name": "app", "image": image + ":v1.4.2"}}},
			"status": map[string]any{"phase": "Running",
				"containerStatuses": []any{map[string]any{"name": "app", "imageID": image + "@" + digest}}},
		})
	}
	data, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	require.NoError(t, err)
	path = filepath.Join(t.TempDir(), "pods.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path, images
}

func TestReport_At1000PodsNaming300DistinctImagesTheBodyStaysInsideThePlatformsLimitWithTheImageListWhole(t *testing.T) {
	pods, images := crowdedPods(t)

	_, posted, out, err := runReport(t, kubectlWith(podsRead, "cat "+pods))

	require.NoError(t, err, out)
	t.Logf("the report body is %d bytes", len(posted.raw))
	assert.LessOrEqual(t, len(posted.raw), 256<<10, "the body fits the platform's limit")
	refs, err := report.Images(posted.raw)
	require.NoError(t, err)
	listed := map[string]bool{}
	for _, ref := range refs {
		listed[ref.Image] = ref.Digest != ""
	}
	for _, image := range images {
		assert.True(t, listed[image], "the image list names %s with its digest", image)
	}
	assert.Len(t, posted.body["pods"], 1000, "every pod stays listed")
}

func TestReport_NamesTheFailedReadInPlaceOfAnImageListAPodOrAWorkloadReadLeavesPartial(t *testing.T) {
	for read, name := range map[string]string{podsRead: "pods", workloadsRead: "workloads"} {
		t.Run(name, func(t *testing.T) {
			_, posted, out, err := runReport(t, kubectlWith(read, `echo 'error: forbidden' >&2; exit 1`))
			require.NoError(t, err, out)

			_, err = report.Images(posted.raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "the report's read of the "+name+" ended with exit status 1")
			assert.Contains(t, err.Error(), "error: forbidden")
		})
	}
}

func TestImages_RefusesAReportThatListsNoImages(t *testing.T) {
	_, err := report.Images([]byte(`{"pods": []}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the report lists no images")
}
