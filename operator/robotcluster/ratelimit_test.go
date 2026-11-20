package robotcluster_test

// Once Hetzner refuses a Cloud call for its rate limit, the cluster sends no other call with that token until the reset Hetzner names.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/reqcache"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCluster_HoldsCloudCallsAfterARateLimitRefusal(t *testing.T) {
	e := onDedicatedServer(t)
	obs, err := e.Settle()
	require.NoError(t, err, "the subscribe: %s", obs.StatusDetail)
	reset := time.Now().Add(30 * time.Second).Truncate(time.Second)
	e.Cloud.FailNext = &cloudtest.Refused{Status: http.StatusTooManyRequests, Code: "rate_limit_exceeded",
		Message: "limit of 3600 requests per hour reached", Reset: reset}
	cfg := e.Config
	cfg.CloudAPI = robotcluster.CloudAPIAt(e.Cloud.Serve(t), cfg.Block)
	k := robotcluster.New(cfg)
	d := e.Desired()
	// block-kit keeps a refused token's hold for the whole process until the reset, so each run refuses a token of its own.
	d.Secrets["hetzner"]["api_token"] = fmt.Sprintf("hetzner-token-refused-at-%d", time.Now().UnixNano())
	before := e.Cloud.Reads()

	_, err = k.PlanReplacements(reqcache.With(t.Context()), d)
	assertHeldUntil(t, "the validate's replacement plan", err, reset)
	_, _, err = k.EnsureResourcesOwning(reqcache.With(t.Context()), d)
	assertHeldUntil(t, "the subscribe", err, reset)
	assert.Equal(t, cloudtest.Reads{"ListServers": 1}, e.Cloud.Reads().Since(before), "the Cloud calls the validate and the subscribe sent")
}

// assertHeldUntil checks that err asks the platform to call again at reset, which block-kit's handler answers unknown on a
// validate and waiting on a subscribe, and that it names reset in UTC.
func assertHeldUntil(t *testing.T, call string, err error, reset time.Time) {
	t.Helper()
	var w *handler.Waiting
	require.ErrorAs(t, err, &w, "%s returned %v", call, err)
	assert.True(t, w.LookAgainAt.Equal(reset), "%s looks again at %s, not at the reset %s", call, w.LookAgainAt, reset)
	assert.Contains(t, w.Detail, reset.UTC().Format(time.RFC3339), call)
}
