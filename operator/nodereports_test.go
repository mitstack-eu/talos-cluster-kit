package operator_test

// The newest node report a cluster sent, and the nodes it names.

import (
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/observe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/operator"
)

var reportsStart = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func receivedAt(minutes int, body string) observe.NodeReport {
	return observe.NodeReport{ReceivedAt: reportsStart.Add(time.Duration(minutes) * time.Minute), Body: []byte(body)}
}

func TestNewestReport_IsTheLastReportReceivedAfterSinceAndNilWhenNoneIs(t *testing.T) {
	reports := []observe.NodeReport{receivedAt(5, `{"n":5}`), receivedAt(9, `{"n":9}`), receivedAt(7, `{"n":7}`)}

	newest := operator.NewestReport(reports, reportsStart)
	none := operator.NewestReport(reports, reportsStart.Add(9*time.Minute))

	require.NotNil(t, newest)
	assert.JSONEq(t, `{"n":9}`, string(newest.Body), "the report received last, whatever its place in the list")
	assert.Nil(t, none, "no report was received after the newest one")
}

func TestNewestReportNodes_ReadsTheNodesTheNewestReportNamesWithTheirReadiness(t *testing.T) {
	reports := []observe.NodeReport{
		receivedAt(1, `{"nodes":[{"name":"node-fsn1","ready":false}]}`),
		receivedAt(2, `{"nodes":[{"name":"node-fsn1","ready":true},{"name":"wk-hel1","ready":false}]}`),
	}

	nodes, err := operator.NewestReportNodes(reports, reportsStart)

	require.NoError(t, err)
	assert.Equal(t, []operator.ReportedNode{{Name: "node-fsn1", Ready: true}, {Name: "wk-hel1", Ready: false}}, nodes)
}

func TestNewestReportNodes_TellsAReportNamingNoNodeFromNoReportAndRefusesOneThatIsNotJSON(t *testing.T) {
	empty, err := operator.NewestReportNodes([]observe.NodeReport{receivedAt(1, `{}`)}, reportsStart)
	require.NoError(t, err)
	none, err := operator.NewestReportNodes(nil, reportsStart)
	require.NoError(t, err)
	_, broken := operator.NewestReportNodes([]observe.NodeReport{receivedAt(1, `not json`)}, reportsStart)

	assert.Equal(t, []operator.ReportedNode{}, empty, "a report that names no node")
	assert.Nil(t, none, "no report since the time asked for")
	assert.EqualError(t, broken, "the newest node report is not the node report's JSON")
}
