package robotcluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
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
}

// RunnersProblem names each runner the newest node report's sweep could not remove, or why the sweep removed none, or
// is "" when it removed every runner it found dead, waits for its first token or has not reported yet.
func (k *Cluster) RunnersProblem(ctx context.Context, d contract.Desired) string {
	reports, err := k.cfg.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return fmt.Sprintf("the node reports cannot be read, so whether the runner sweep removed every dead runner is unknown: %v", err)
	}
	newest := operator.NewestReport(reports, time.Time{})
	if newest == nil {
		return ""
	}
	var body struct {
		Runners *runnerSweep `json:"runners"`
	}
	if err := json.Unmarshal(newest.Body, &body); err != nil {
		return "the newest node report's runners member is not the runner sweep's JSON"
	}
	switch sweep := body.Runners; {
	case sweep == nil:
		return ""
	case sweep.Unread != "":
		return "the runner sweep removed no runner: " + sweep.Unread
	case len(sweep.Unremoved) > 0:
		return sweep.refusals()
	}
	return ""
}

func (s runnerSweep) refusals() string {
	named := make([]string, 0, len(s.Unremoved))
	for _, r := range s.Unremoved {
		named = append(named, fmt.Sprintf("%s (id %d), HTTP %d: %s", r.Name, r.ID, r.Status, r.Message))
	}
	return fmt.Sprintf("GitHub refused to remove these offline runners of the scale set %s that no pod runs: %s",
		s.ScaleSet, strings.Join(named, "; "))
}
