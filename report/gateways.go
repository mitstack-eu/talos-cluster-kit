package report

import (
	"encoding/json"
	"errors"
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

// ImageRef is one image reference a report lists: the image as a pod or a pod template names it, and the digest it runs
// by, empty when neither the reference nor a pod's status names one.
type ImageRef struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// Images reads a report's image list, and refuses a report that lists none or whose reads behind the list failed.
func Images(body []byte) ([]ImageRef, error) {
	var report struct {
		Images json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, fmt.Errorf("the report cannot be read: %w", err)
	}
	if len(report.Images) == 0 || string(report.Images) == "null" {
		return nil, errors.New("the report lists no images, as a report made before the image list was added")
	}
	var refs []ImageRef
	if err := json.Unmarshal(report.Images, &refs); err == nil {
		return refs, nil
	}
	var failed struct {
		Read       string `json:"read"`
		ExitStatus int    `json:"exitStatus"`
		Stderr     string `json:"stderr"`
	}
	if err := json.Unmarshal(report.Images, &failed); err != nil || failed.Read == "" {
		return nil, errors.New("the report's images member is neither a list nor a failed read")
	}
	return nil, fmt.Errorf("the report's read of the %s ended with exit status %d, so its image list is not whole: %s",
		failed.Read, failed.ExitStatus, failed.Stderr)
}
