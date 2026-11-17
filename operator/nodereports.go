package operator

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/mitstack-eu/block-kit/observe"
)

// ReportedNode is one node as the kit's node report names it.
type ReportedNode struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// NewestReport is the newest report received after since, or nil when there is none.
func NewestReport(reports []observe.NodeReport, since time.Time) *observe.NodeReport {
	var newest *observe.NodeReport
	for i, r := range reports {
		if r.ReceivedAt.After(since) && (newest == nil || r.ReceivedAt.After(newest.ReceivedAt)) {
			newest = &reports[i]
		}
	}
	return newest
}

// NewestReportNodes is the nodes of the newest report received after since, empty for a report that names none, and
// nil when there is no such report.
func NewestReportNodes(reports []observe.NodeReport, since time.Time) ([]ReportedNode, error) {
	newest := NewestReport(reports, since)
	if newest == nil {
		return nil, nil
	}
	var body struct {
		Nodes []ReportedNode `json:"nodes"`
	}
	if err := json.Unmarshal(newest.Body, &body); err != nil {
		return nil, errors.New("the newest node report is not the node report's JSON")
	}
	if body.Nodes == nil {
		body.Nodes = []ReportedNode{}
	}
	return body.Nodes, nil
}
