package report

import (
	"encoding/json"
	"net/netip"
)

// Gateway reads a report's gateways member for the gateway at address: whether the report names it, whether its mirror
// answered, and why not. A report that cannot be read names no gateway.
func Gateway(body []byte, address netip.Addr) (named, passed bool, reason string) {
	var report struct {
		Gateways []struct {
			Address string `json:"address"`
			Passed  bool   `json:"passed"`
			Reason  string `json:"reason"`
		} `json:"gateways"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return false, false, "the report cannot be read: " + err.Error()
	}
	for _, g := range report.Gateways {
		if g.Address == address.String() {
			return true, g.Passed, g.Reason
		}
	}
	return false, false, ""
}
