package report

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"time"
)

// Gateway reads a report's gateways member for the gateway at address: whether the report names a reading of it made
// after since, whether its mirror served, and why not. A report that cannot be read names no gateway. The outside
// probe makes each reading before the report carries it, under probedAt, and a reading without one is the report's own.
func Gateway(body []byte, address netip.Addr, since time.Time) (named, passed bool, reason string) {
	var report struct {
		Gateways []struct {
			Address  string `json:"address"`
			Passed   bool   `json:"passed"`
			Reason   string `json:"reason"`
			ProbedAt string `json:"probedAt"`
		} `json:"gateways"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return false, false, "the report cannot be read: " + err.Error()
	}
	for _, g := range report.Gateways {
		if g.Address != address.String() {
			continue
		}
		if g.ProbedAt == "" {
			return true, g.Passed, g.Reason
		}
		at, err := time.Parse(time.RFC3339, g.ProbedAt)
		if err != nil {
			return false, false, fmt.Sprintf("its reading of the gateway at %s names no time it was probed: %v", address, err)
		}
		if !at.After(since) {
			return false, false, fmt.Sprintf("the outside probe last read the gateway at %s at %s, not after %s",
				address, g.ProbedAt, since.UTC().Format(time.RFC3339))
		}
		return true, g.Passed, g.Reason
	}
	return false, false, ""
}
