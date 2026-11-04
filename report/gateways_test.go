package report_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mitstack-eu/talos-cluster-kit/report"
)

func TestGateway_ReadsWhetherAReportNamesTheGatewayAndWhetherItsMirrorAnswered(t *testing.T) {
	body := []byte(`{"gateways":[{"address":"10.0.0.2","passed":true},{"address":"10.0.0.248","passed":false,"reason":"the mirror did not answer within 5s"}]}`)
	for _, tc := range []struct {
		name, address, reason string
		named, passed         bool
	}{
		{name: "answered", address: "10.0.0.2", named: true, passed: true},
		{name: "not answered", address: "10.0.0.248", named: true, reason: "the mirror did not answer within 5s"},
		{name: "not named", address: "10.0.0.249"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named, passed, reason := report.Gateway(body, netip.MustParseAddr(tc.address))
			assert.Equal(t, []any{tc.named, tc.passed, tc.reason}, []any{named, passed, reason})
		})
	}
	named, _, reason := report.Gateway([]byte("not json"), netip.MustParseAddr("10.0.0.2"))
	assert.False(t, named, "a report that cannot be read")
	assert.Contains(t, reason, "the report cannot be read")
}
