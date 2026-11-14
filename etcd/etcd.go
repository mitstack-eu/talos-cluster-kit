// Package etcd renders a cluster's etcd recovery: the Job that removes a lost node's member with its Node
// object, and the gateway's script that bootstraps etcd empty or from a snapshot.
package etcd

import (
	_ "embed"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

var (
	//go:embed remove-member-job.yaml
	removeMemberJobTemplate string
	//go:embed scripts/remove-member.sh
	removeMemberScript string
	//go:embed scripts/bootstrap.sh
	bootstrapScript []byte
)

var removeMemberJob = tmpl.Must("the etcd member removal Job", removeMemberJobTemplate)

// jobNamePrefix opens the name of every member removal Job, which a Kubernetes label caps at maxLabelLength.
const (
	jobNamePrefix  = "mitstack-etcd-remove-"
	maxLabelLength = 63
)

// RemovalDir is the directory of the cluster repository that the cluster's own Application applies and prunes.
const RemovalDir = "cluster/"

// JobName is the name of the Job that removes hostname's member, by which Argo CD lists it.
func JobName(hostname string) string { return jobNamePrefix + hostname }

// RemovalPath is where the cluster repository holds hostname's removal, so the cluster's own Application applies it
// while main holds the file and prunes its Job once main does not.
func RemovalPath(hostname string) string { return RemovalDir + JobName(hostname) + ".yaml" }

// IsRemovalPath reports whether path is one RemovalPath gives, by which a block lists the removals main holds.
func IsRemovalPath(path string) bool {
	name, inDir := strings.CutPrefix(path, RemovalDir+jobNamePrefix)
	hostname, isYAML := strings.CutSuffix(name, ".yaml")
	return inDir && isYAML && check.DNS1123Label(hostname) == nil
}

// RemoveMember names the lost node whose etcd member and Node object go.
type RemoveMember struct {
	Hostname string
	// ToolsImage is the image built from tools/Dockerfile, by digest, which carries bash, talosctl and kubectl.
	ToolsImage string
	// Report is the newest node report, whose etcd members must show a majority serving without the lost one.
	Report report.Read
	// StepStart is when the lost node's replacement began, so a report read before then proves nothing.
	StepStart time.Time
	// Guard is block-kit's redundancy guard, which the block passes.
	Guard redundant.Guard
}

// RemoveMemberJob renders the objects that remove r's member, in the namespace the node report creates, once r.Guard
// finds a majority of the other members read serving since r.StepStart.
func RemoveMemberJob(r RemoveMember) ([]byte, error) {
	if err := check.DNS1123Label(r.Hostname); err != nil {
		return nil, fmt.Errorf("lost node: %w", err)
	}
	if len(jobNamePrefix)+len(r.Hostname) > maxLabelLength {
		return nil, fmt.Errorf("lost node %q is longer than the %d characters a Job name leaves it", r.Hostname, maxLabelLength-len(jobNamePrefix))
	}
	if err := check.DigestRef(r.ToolsImage); err != nil {
		return nil, fmt.Errorf("tools image: %w", err)
	}
	members, err := r.Report.EtcdMembers()
	if err != nil {
		return nil, err
	}
	if len(members) > 0 && !slices.ContainsFunc(members, func(m redundant.Member) bool { return m.Name == r.Hostname }) {
		// A member the report no longer lists is removed already and only the Node object is left, so the guard judges
		// the others as it would with that member unread.
		members = append(members, redundant.Member{Name: r.Hostname})
	}
	set := redundant.Set{Name: "etcd members", Members: members, Minimum: redundant.Majority}
	if err := r.Guard.Allow(set, r.Hostname, r.StepStart); err != nil {
		return nil, err
	}
	return tmpl.Execute(removeMemberJob, map[string]string{
		"Name":      JobName(r.Hostname),
		"Namespace": nodeconfig.TalosAPINamespace,
		"Hostname":  r.Hostname,
		"Image":     r.ToolsImage,
		"Script":    removeMemberScript,
	})
}

// BootstrapScript returns the script the gateway runs to bootstrap etcd, with the arguments Bootstrap.Args gives.
func BootstrapScript() []byte {
	return bootstrapScript
}

// Bootstrap is one run of the gateway's bootstrap script.
type Bootstrap struct {
	// Node is the private address of the cluster's first control-plane node.
	Node netip.Addr
	// Talosconfig is the path of an admin talosconfig from Secrets.AdminTalosconfig, which the script deletes on exit.
	Talosconfig string
	// Snapshot is the path of an etcd snapshot to restore from, or empty for a new cluster.
	Snapshot string
	// Keep names the nodes of the restored control plane, whose Node objects stay.
	Keep []string
}

// Args returns the script's arguments for b.
func (b Bootstrap) Args() []string {
	args := []string{"--node", b.Node.String(), "--talosconfig", b.Talosconfig}
	if b.Snapshot == "" {
		return args
	}
	args = append(args, "--recover-from", b.Snapshot)
	for _, name := range b.Keep {
		args = append(args, "--keep", name)
	}
	return args
}
