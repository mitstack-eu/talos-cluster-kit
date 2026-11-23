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
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// staleSweepBound is how old the newest full report may be before the check reads the sweep as stopped, three report
// runs, so one late or failed run alone reads no problem.
const staleSweepBound = 3 * report.Interval

// stuckRunnerBound is how long a runner may read busy with no pod before the check names it, since the sweep removes no
// busy runner and GitHub documents no time within which it fails the job of a runner that went away.
const stuckRunnerBound = 30 * time.Minute

// unremovedRunner is a runner the sweep asked GitHub to remove that GitHub did not answer as gone, with its status
// and message.
type unremovedRunner struct {
	Name    string `json:"name"`
	ID      int64  `json:"id"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// runnerSweep is the part of the node report's runners member that the check reads.
type runnerSweep struct {
	ScaleSet  string            `json:"scaleSet"`
	Listed    *int              `json:"listed"`
	Unread    string            `json:"unread"`
	Skipped   string            `json:"skipped"`
	Unremoved []unremovedRunner `json:"unremoved"`
	Busy      []string          `json:"busy"`
	Unlisted  int               `json:"unlisted"`
}

// RunnersProblem names each way the newest node report that read everything shows the runner sweep failed, stopped or
// blocked, and is "" when it shows none or the cluster runs no scale set, so a backlog the sweep still works through
// reads "".
func (k *Cluster) RunnersProblem(ctx context.Context, d contract.Desired) string {
	runners, err := k.runnersFor(d)
	switch {
	case err != nil:
		return fmt.Sprintf("the cluster's runner services cannot be read, so whether the runner sweep removes dead runners is unknown: %v", err)
	case runners == nil:
		return ""
	}
	reports, err := k.cfg.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return fmt.Sprintf("the node reports cannot be read, so whether the runner sweep removed every dead runner is unknown: %v", err)
	}
	full := fullReports(reports)
	newest := operator.NewestReport(full, time.Time{})
	if newest == nil {
		return "no node report that read everything has reached the platform, so the runner sweep has not reported"
	}
	sweep, err := sweepOf(*newest)
	if err != nil {
		return "the newest node report's runners member is not the runner sweep's JSON"
	}
	if k.cfg.Now().Sub(newest.ReceivedAt) > staleSweepBound {
		return fmt.Sprintf("the newest node report that read everything was received at %s, more than %d minutes ago, so the runner sweep has not reported since",
			newest.ReceivedAt.UTC().Format(time.RFC3339), int(staleSweepBound/time.Minute))
	}
	switch {
	case sweep == nil:
		return "the newest node report carries no runner sweep, though the cluster runs the scale set " + runners.ScaleSet
	case sweep.Unread != "":
		return "the runner sweep removed no runner: " + sweep.Unread
	case sweep.Skipped != "":
		return "the runner sweep has not run: " + sweep.Skipped
	case sweep.ScaleSet != runners.ScaleSet:
		return fmt.Sprintf("the newest runner sweep read the scale set %q, though the cluster runs the scale set %q", sweep.ScaleSet, runners.ScaleSet)
	case sweep.Listed == nil:
		return "the newest runner sweep says neither how many runners it listed nor why it listed none, so whether it removed every dead runner is unknown"
	}
	var problems []string
	if len(sweep.Unremoved) > 0 {
		problems = append(problems, sweep.refusals())
	}
	if sweep.Unlisted > 0 {
		problems = append(problems, fmt.Sprintf("GitHub lists %d more runners in the runner group of the scale set %s than the sweep reads, so a dead runner among them stays listed",
			sweep.Unlisted, sweep.ScaleSet))
	}
	if stuck := stuckRunners(*sweep, full, newest.ReceivedAt); len(stuck) > 0 {
		problems = append(problems, fmt.Sprintf("these offline runners of the scale set %s have read busy with no pod behind them for %d minutes or more, and the sweep removes no busy runner: %s",
			sweep.ScaleSet, int(stuckRunnerBound/time.Minute), strings.Join(stuck, ", ")))
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

func sweepOf(r observe.NodeReport) (*runnerSweep, error) {
	var body struct {
		Runners *runnerSweep `json:"runners"`
	}
	err := json.Unmarshal(r.Body, &body)
	return body.Runners, err
}

// stuckRunners is each runner sweep reads busy that the sweep of a full report received stuckRunnerBound or more
// before at also read busy.
func stuckRunners(sweep runnerSweep, full []observe.NodeReport, at time.Time) []string {
	var stuck []string
	for _, r := range full {
		older, err := sweepOf(r)
		if err != nil || older == nil || r.ReceivedAt.After(at.Add(-stuckRunnerBound)) {
			continue
		}
		for _, name := range sweep.Busy {
			if slices.Contains(older.Busy, name) && !slices.Contains(stuck, name) {
				stuck = append(stuck, name)
			}
		}
	}
	return stuck
}

func (s runnerSweep) refusals() string {
	named := make([]string, 0, len(s.Unremoved))
	for _, r := range s.Unremoved {
		named = append(named, fmt.Sprintf("%s (id %d), HTTP %d: %s", r.Name, r.ID, r.Status, r.Message))
	}
	return fmt.Sprintf("GitHub refused to remove these idle offline runners of the scale set %s, which ARC no longer holds: %s",
		s.ScaleSet, strings.Join(named, "; "))
}
