// Package topology renders the labeller that labels each Cloud node with the zone and the region it runs in.
package topology

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

//go:embed topology.yaml
var labellerTemplate string

var labeller = tmpl.Must("the topology labeller", labellerTemplate)

const (
	ZoneLabel   = "topology.kubernetes.io/zone"
	RegionLabel = "topology.kubernetes.io/region"
	// FieldManager is the manager each labelled Node's managedFields names for the two labels.
	FieldManager = "mitstack-node-topology"
)

// Node is one node the labeller labels, by the name Kubernetes knows it by.
type Node struct {
	Name string
	Zone string
}

// Inputs are what one cluster's labeller needs.
type Inputs struct {
	Region string
	Nodes  []Node
	// ToolsImage is the image built from tools/Dockerfile, by digest, which carries bash, kubectl and the labeller's script.
	ToolsImage string
}

// Labels are the two labels a node in zone of region carries.
func Labels(zone, region string) map[string]string {
	return map[string]string{ZoneLabel: zone, RegionLabel: region}
}

// Manifests renders the labeller's identity, its right to get and patch the nodes it names, and its CronJob, in the
// namespace the node report creates.
func Manifests(in Inputs) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(in.Nodes))
	pairs := make([]string, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		names = append(names, n.Name)
		pairs = append(pairs, n.Name+"="+n.Zone)
	}
	return tmpl.Execute(labeller, map[string]any{
		"Name":         FieldManager,
		"Namespace":    nodeconfig.TalosAPINamespace,
		"Image":        in.ToolsImage,
		"NodeNames":    names,
		"Nodes":        strings.Join(pairs, " "),
		"Region":       in.Region,
		"FieldManager": FieldManager,
		"Script":       tools.ScriptDir + "/label-nodes.sh",
	})
}

func (in Inputs) validate() error {
	if len(in.Nodes) == 0 {
		return errors.New("the labeller has no node to label")
	}
	if err := check.LabelValue(in.Region); err != nil {
		return fmt.Errorf("region: %w", err)
	}
	seen := map[string]bool{}
	for _, n := range in.Nodes {
		if err := check.DNS1123Label(n.Name); err != nil {
			return fmt.Errorf("node: %w", err)
		}
		if seen[n.Name] {
			return fmt.Errorf("node %s is named twice", n.Name)
		}
		seen[n.Name] = true
		if err := check.LabelValue(n.Zone); err != nil {
			return fmt.Errorf("node %s zone: %w", n.Name, err)
		}
	}
	if err := check.DigestRef(in.ToolsImage); err != nil {
		return fmt.Errorf("tools image: %w", err)
	}
	return nil
}
