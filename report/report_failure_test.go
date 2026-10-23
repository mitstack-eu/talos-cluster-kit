package report_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	nodesRead        = "get nodes -o json"
	applicationsRead = "get applications.argoproj.io -n argocd -o json"

	// failedRead writes 2000 bytes of noise, its message, a line holding the report token, a bearer header, a
	// JWT and a NUL to stderr, then exits 3.
	failedRead = `head -c 2000 /dev/zero | tr '\0' x >&2; printf '\nerror: the server is gone\n' >&2
printf 'token ` + reportToken + `\nAuthorization: Bearer abc.def\nsigned eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln\n\0' >&2
exit 3`
	slowRead = `sleep 10`
)

// kubectlAnswering answers each kubectl read from the fixture cluster, except those overrides replaces.
func kubectlAnswering(overrides map[string]string) string {
	answers := map[string]string{
		nodesRead:         "cat testdata/nodes.json",
		applicationsRead:  "cat testdata/applications.json",
		keySetRead:        "cat testdata/jwks.json",
		cloudNativePGRead: "cat testdata/cloudnative-pg.json",
	}
	for read, answer := range overrides {
		answers[read] = answer
	}
	var script strings.Builder
	script.WriteString("case \"$*\" in\n")
	for read, answer := range answers {
		script.WriteString("  \"" + read + "\")\n" + answer + "\n;;\n")
	}
	script.WriteString("  *) exit 1 ;;\nesac\n")
	return script.String()
}

func TestReport_ThatCannotReadPostsOneReportNamingTheReadAndExitsNonZero(t *testing.T) {
	cases := map[string]struct {
		kubectl  map[string]string
		talosctl string
		want     map[string]any
	}{
		"the nodes read fails": {
			kubectl: map[string]string{nodesRead: failedRead},
			want:    map[string]any{"read": "kubectl get nodes", "exitStatus": float64(3)},
		},
		"the applications read fails": {
			kubectl: map[string]string{applicationsRead: failedRead},
			want:    map[string]any{"read": "kubectl get applications", "exitStatus": float64(3)},
		},
		"the key set read fails": {
			kubectl: map[string]string{keySetRead: failedRead},
			want:    map[string]any{"read": "kubectl get --raw /openid/v1/jwks", "exitStatus": float64(3)},
		},
		"the etcd members read fails": {
			talosctl: failedRead,
			want:     map[string]any{"read": "talosctl etcd members", "exitStatus": float64(3)},
		},
		"the key set read times out": {
			kubectl: map[string]string{keySetRead: slowRead},
			want:    map[string]any{"read": "kubectl get --raw /openid/v1/jwks", "timedOut": true},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			talosctl := fakeTalosctl
			if tc.talosctl != "" {
				talosctl = tc.talosctl
			}

			_, posted, out, err := runReportWith(t,
				map[string]string{"kubectl": kubectlAnswering(tc.kubectl), "talosctl": talosctl}, "REPORT_READ_SECONDS=1")

			require.Error(t, err, "the report exits non-zero once it has posted the error")
			require.Equal(t, 1, posted.posts, out)
			assertErrorReport(t, posted)
			stderr := testutil.Field(t, posted.body, "error", "stderr").(string)
			delete(posted.body["error"].(map[string]any), "stderr")
			assert.Equal(t, tc.want, posted.body["error"])
			if tc.want["timedOut"] == nil {
				assert.Len(t, stderr, 1024, "the error carries the last 1 KiB of the read's stderr")
				assert.True(t, strings.HasSuffix(stderr, "error: the server is gone\n"), "stderr ends with the read's message: %q", stderr)
			}
		})
	}
}

func TestReport_WhoseBodyCannotBeBuiltPostsOneReportNamingTheBody(t *testing.T) {
	_, posted, out, err := runReportWith(t, map[string]string{
		"kubectl":  kubectlAnswering(map[string]string{nodesRead: "echo 'not json'"}),
		"talosctl": fakeTalosctl,
	})

	require.Error(t, err, "the report exits non-zero once it has posted the error")
	require.Equal(t, 1, posted.posts, out)
	assertErrorReport(t, posted)
	assert.Equal(t, "body", testutil.Field(t, posted.body, "error", "read"))
	assert.NotZero(t, testutil.Field(t, posted.body, "error", "exitStatus"))
	assert.Contains(t, testutil.Field(t, posted.body, "error", "stderr"), "jq")
}

func TestReport_WhoseCloudNativePGReadTimesOutStillPostsItsNodes(t *testing.T) {
	_, posted, out, err := runReportWith(t, map[string]string{
		"kubectl":  kubectlAnswering(map[string]string{cloudNativePGRead: slowRead}),
		"talosctl": fakeTalosctl,
	}, "REPORT_READ_SECONDS=1")

	require.NoError(t, err, out)
	assert.Len(t, posted.body["nodes"], 3)
	assert.Equal(t, float64(124), testutil.Field(t, posted.body, "cloudNativePG", "exitStatus"))
}

func TestReport_ThatReadsEverythingPostsTheBodyThePreviousPatchReleasePosted(t *testing.T) {
	_, posted, out, err := runReportWith(t, map[string]string{
		"kubectl":  fakeKubectl,
		"talosctl": fakeTalosctl,
		"date":     "echo 2026-10-23T15:45:00Z",
	})

	require.NoError(t, err, out)
	testutil.Golden(t, filepath.Join("testdata", "golden", "healthy-body.json"), posted.raw)
}

// assertErrorReport checks the report holds only who and when it is and the error, and no token anywhere.
func assertErrorReport(t *testing.T, posted *postedReport) {
	t.Helper()
	assert.Equal(t, "Bearer "+reportToken, posted.authorization)
	assert.Equal(t, "sub_0123456789ab", posted.body["subscriptionId"])
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, posted.body["reportedAt"])
	assert.ElementsMatch(t, []string{"subscriptionId", "reportedAt", "error"}, keys(posted.body))
	for _, leak := range []string{reportToken, "Bearer", "eyJ", "\\u0000"} {
		assert.NotContains(t, string(posted.raw), leak)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
