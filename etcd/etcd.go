// Package etcd renders a cluster's etcd recovery: the Job that removes a lost node's member with its Node
// object, and the gateway's script that bootstraps etcd empty or from a snapshot.
package etcd

import (
	_ "embed"
	"fmt"
	"net/netip"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
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

// RemoveMember names the lost node whose etcd member and Node object go.
type RemoveMember struct {
	Hostname string
	// ToolsImage is the image built from tools/Dockerfile, by digest, which carries bash, talosctl and kubectl.
	ToolsImage string
}

// RemoveMemberJob renders the objects that remove r's member, in the namespace the node report creates.
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
	return tmpl.Execute(removeMemberJob, map[string]string{
		"Name":      jobNamePrefix + r.Hostname,
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
