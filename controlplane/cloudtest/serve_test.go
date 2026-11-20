package cloudtest_test

// The fake on the wire answers a server list with the refusal FailNext holds, as Hetzner's error naming its reset.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
)

func TestServe_AnswersAServerListWithTheRefusalFailNextHolds(t *testing.T) {
	reset := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	fake := cloudtest.New(func() time.Time { return reset.Add(-time.Minute) })
	fake.FailNext = &cloudtest.Refused{Status: http.StatusTooManyRequests, Code: "rate_limit_exceeded",
		Message: "Limit of '5000' requests per hour for ip '192.0.2.10' reached.", Reset: reset}
	address := fake.Serve(t)

	resp, err := http.Get(address + "/servers?label_selector=a%3Db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	var body struct {
		Error struct{ Code, Message string }
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body), "the refusal's body")
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "the refused list's status")
	assert.Equal(t, strconv.FormatInt(reset.Unix(), 10), resp.Header.Get("RateLimit-Reset"), "the reset the refusal names, %s", reset)
	assert.Equal(t, "rate_limit_exceeded", body.Error.Code, "the refusal's error code")
	assert.Equal(t, cloudtest.Reads{"ListServers": 1}, fake.Reads(), "the reads the refused list made")
}
