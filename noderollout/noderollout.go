// Package noderollout renders the Job that gives every running node of a cluster the machine configuration a kit
// release renders for it, without a reboot, from inside the cluster, so no configuration and no key leaves it.
package noderollout

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

var (
	//go:embed job.yaml
	jobTemplate string
	//go:embed scripts/apply-machine-config.sh
	script string
)

var job = tmpl.Must("the machine configuration rollout Job", jobTemplate)

// jobNamePrefix opens the name of every rollout Job, which shortDigestLen hex digits of its release close.
const (
	jobNamePrefix  = "mitstack-machine-config-"
	shortDigestLen = 12
	digestPrefix   = "sha256:"
)

var shortDigestRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Release is what a node's nodeconfig.MachineConfigAnnotation names once it runs the configuration of the kit release
// whose tools image is toolsImage: that image's digest, since the image carries the machine-config that renders it.
func Release(toolsImage string) (string, error) {
	if err := check.DigestRef(toolsImage); err != nil {
		return "", fmt.Errorf("tools image: %w", err)
	}
	_, digest, _ := strings.Cut(toolsImage, "@")
	return digest, nil
}

// JobName is the name of the Job that gives each node release, by which Argo CD lists it.
func JobName(release string) string {
	hex := strings.TrimPrefix(release, digestPrefix)
	return jobNamePrefix + hex[:min(shortDigestLen, len(hex))]
}

// Path is where the cluster repository holds release's rollout, so the cluster's own Application applies it while main
// holds the file and prunes its Job once main does not.
func Path(release string) string { return etcd.RemovalDir + JobName(release) + ".yaml" }

// IsPath reports whether path is one Path gives, by which a block lists the rollouts main holds.
func IsPath(path string) bool {
	name, inDir := strings.CutPrefix(path, etcd.RemovalDir+jobNamePrefix)
	short, isYAML := strings.CutSuffix(name, ".yaml")
	return inDir && isYAML && shortDigestRe.MatchString(short)
}

// Rollout names the release every node takes and the node report that must show etcd able to lose any one member.
type Rollout struct {
	// ToolsImage is the image built from tools/Dockerfile, by digest, which carries talosctl, kubectl, jq and the
	// machine-config of the release it names.
	ToolsImage string
	// Report is the newest node report, whose etcd members must each read serving.
	Report report.Read
	// StepStart is when the Update began, so a report read before then proves nothing.
	StepStart time.Time
	// Guard is block-kit's redundancy guard, which the block passes.
	Guard redundant.Guard
}

// Job renders the objects that give each node r's release, in the namespace the node report creates, once r.Guard finds
// that etcd keeps every member but any one serving, since the Job changes a control-plane node at a time, and on a
// plane of one member once that member reads serving since r.StepStart.
func Job(r Rollout) ([]byte, error) {
	release, err := Release(r.ToolsImage)
	if err != nil {
		return nil, err
	}
	members, err := r.Report.EtcdMembers()
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, errors.New("the node report lists no etcd member, so no node's configuration changes")
	}
	if err := allowChanges(r, members); err != nil {
		return nil, err
	}
	return tmpl.Execute(job, map[string]string{
		"Name":      JobName(release),
		"Namespace": nodeconfig.TalosAPINamespace,
		"Release":   release,
		"Image":     r.ToolsImage,
		"Script":    script,
	})
}

const etcdMembers = "etcd members"

// allowChanges asks r.Guard to allow changing each etcd member in turn, and judges the only member of a plane of one by
// its own reading, since the guard counts only the members a change leaves and a set of one leaves none.
func allowChanges(r Rollout, members []report.Member) error {
	set := redundant.Set{Name: etcdMembers, Members: members, Minimum: redundant.AllButOne}
	if len(members) == 1 {
		set.Minimum = redundant.NoRedundancy
	}
	for _, m := range members {
		if err := r.Guard.Allow(set, m.Name, r.StepStart); err != nil {
			return err
		}
	}
	if len(members) == 1 {
		return onlyMemberServing(members[0], r.StepStart)
	}
	return nil
}

// onlyMemberServing refuses, in the guard's held form, a change to m unless m reads serving since stepStart.
func onlyMemberServing(m report.Member, stepStart time.Time) error {
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	var reading string
	switch {
	case m.Reading == nil:
		reading = fmt.Sprintf("%q has no reading", m.Name)
	case m.Reading.At.Before(stepStart):
		reading = fmt.Sprintf("%q read at %s, before the step began", m.Name, at(m.Reading.At))
	case !m.Reading.Serving:
		reading = fmt.Sprintf("%q read not serving at %s", m.Name, at(m.Reading.At))
	default:
		return nil
	}
	return fmt.Errorf("refused:%s: changing %q, the only one of the %s, needs it serving, judged by readings since %s: %s",
		redundant.ClassBelowMinimum, m.Name, etcdMembers, at(stepStart), reading)
}
