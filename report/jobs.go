package report

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// AppliedJob is what Argo CD keeps of a Job an Application applied: its health, and the message of a failed one.
type AppliedJob struct{ Health, Message string }

const (
	// JobCompleted and JobFailed are the health Argo CD reads of a Job that completed and of one that failed.
	JobCompleted = "Healthy"
	JobFailed    = "Degraded"
	jobKind      = "Job"
)

// AppliedJobs reads Argo CD's health of each Job application applied, and false when r lacks any object's health.
func (r Read) AppliedJobs(application string) (map[string]AppliedJob, bool, error) {
	var body struct {
		ArgoCD struct {
			Applications []struct {
				Name      string            `json:"name"`
				Resources *[]map[string]any `json:"resources"`
			} `json:"applications"`
		} `json:"argocd"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		return nil, false, fmt.Errorf("the node report read at %s cannot be read: %w", stamp(r.At), err)
	}
	for _, app := range body.ArgoCD.Applications {
		if app.Name != application || app.Resources == nil {
			continue
		}
		jobs := map[string]AppliedJob{}
		for _, res := range *app.Resources {
			health, kept := res["health"]
			if !kept {
				return nil, false, nil
			}
			if res["kind"] == jobKind {
				name, _ := res["name"].(string)
				status, _ := health.(string)
				message, _ := res["message"].(string)
				jobs[name] = AppliedJob{Health: status, Message: message}
			}
		}
		return jobs, true, nil
	}
	return nil, false, nil
}

// UnreadJobs says that r carries no health of what application applied, for a wait or a refusal to name.
func (r Read) UnreadJobs(application string) string {
	return fmt.Sprintf("the node report read at %s carries no health of what the Application %s applied", stamp(r.At), application)
}

var attemptSuffix = regexp.MustCompile(`^[a-z0-9]{5}$`)

// JobFailure is the message a failed attempt's pod of the Job left in the report, and empty while none left one.
func (r Read) JobFailure(namespace, job string) (string, error) {
	m, err := r.members()
	if err != nil {
		return "", err
	}
	for _, p := range m.Pods {
		suffix, ok := strings.CutPrefix(p.Name, job+"-")
		if !ok || p.Namespace != namespace || p.Phase != "Failed" || !attemptSuffix.MatchString(suffix) {
			continue
		}
		for _, c := range p.Containers {
			if c.Message != "" {
				return c.Message, nil
			}
		}
	}
	return "", nil
}

// ListedNodes reads every Node object the report lists, serving when it reads Ready.
func (r Read) ListedNodes() ([]Member, error) {
	m, err := r.members()
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(m.Nodes))
	for _, n := range m.Nodes {
		out = append(out, Member{Name: n.Name, Reading: r.reading(n.Ready)})
	}
	return out, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
