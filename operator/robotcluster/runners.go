package robotcluster

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
)

// unremovedRunner is a runner the report's sweep asked GitHub to remove, with how GitHub refused.
type unremovedRunner struct {
	Name    string `json:"name"`
	ID      int64  `json:"id"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// runnerSweep is the node report's runners member, which a cluster that runs a runner scale set reports.
type runnerSweep struct {
	ScaleSet  string            `json:"scaleSet"`
	Unread    string            `json:"unread"`
	Unremoved []unremovedRunner `json:"unremoved"`
	Unlisted  int               `json:"unlisted"`
}

// RunnersProblem names each runner the newest full report's sweep could not remove, how many runners it left unread, or why it removed none, and is "" otherwise.
func (k *Cluster) RunnersProblem(ctx context.Context, d contract.Desired) string {
	reports, err := k.cfg.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return fmt.Sprintf("the node reports cannot be read, so whether the runner sweep removed every dead runner is unknown: %v", err)
	}
	newest := operator.NewestReport(fullReports(reports), time.Time{})
	if newest == nil {
		return ""
	}
	var body struct {
		Runners *runnerSweep `json:"runners"`
	}
	if err := json.Unmarshal(newest.Body, &body); err != nil {
		return "the newest node report's runners member is not the runner sweep's JSON"
	}
	sweep := body.Runners
	switch {
	case sweep == nil:
		return ""
	case sweep.Unread != "":
		return "the runner sweep removed no runner: " + sweep.Unread
	}
	var problems []string
	if len(sweep.Unremoved) > 0 {
		problems = append(problems, sweep.refusals())
	}
	if sweep.Unlisted > 0 {
		problems = append(problems, fmt.Sprintf("GitHub lists %d more runners in the runner group of the scale set %s than the sweep reads, so a dead runner among them stays listed",
			sweep.Unlisted, sweep.ScaleSet))
	}
	return strings.Join(problems, "; ")
}

// fullReports drops each report that names a failed read, since such a report carries no runner sweep and would hide
// the sweep of the report before it.
func fullReports(reports []observe.NodeReport) []observe.NodeReport {
	return slices.DeleteFunc(slices.Clone(reports), func(r observe.NodeReport) bool {
		var body struct {
			Error json.RawMessage `json:"error"`
		}
		return json.Unmarshal(r.Body, &body) == nil && body.Error != nil
	})
}

func (s runnerSweep) refusals() string {
	named := make([]string, 0, len(s.Unremoved))
	for _, r := range s.Unremoved {
		named = append(named, fmt.Sprintf("%s (id %d), HTTP %d: %s", r.Name, r.ID, r.Status, r.Message))
	}
	return fmt.Sprintf("GitHub refused to remove these idle offline runners of the scale set %s, which ARC no longer holds: %s",
		s.ScaleSet, strings.Join(named, "; "))
}
