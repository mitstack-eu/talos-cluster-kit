package components

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

// distinctZones are the zones of locations, each once in sorted order, so a cluster spreads by how many zones it spans.
func distinctZones(locations []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(locations)))
}

// databaseOperatorReplicas is one standby beside the replica that holds the lease, which a third would add nothing to,
// since only the lease holder acts.
const databaseOperatorReplicas = 2

const (
	zoneTopologyKey = "topology.kubernetes.io/zone"
	yamlIndent      = 2
	// zoneTermIndent places the zone term under the pod template's requiredDuringSchedulingIgnoredDuringExecution.
	zoneTermIndent = 12
)

// databaseOperator is a Deployment the release runs twice once its pods may run in two zones, named as its manifest is.
type databaseOperator struct {
	name string
	// rollsOneAtATime replaces the replicas one by one. The Barman Cloud plugin keeps its chart's Recreate, which its
	// installation guide keeps so no two versions of the plugin serve the operator at once.
	rollsOneAtATime bool
}

var databaseOperators = []databaseOperator{{name: "cloudnative-pg", rollsOneAtATime: true}, {name: "plugin-barman-cloud"}}

// DatabaseOperatorsAcrossZones is the set whose CloudNativePG operator and Barman Cloud plugin, where it holds them, run
// two replicas in two distinct zones of zones under a disruption budget of one, so losing a zone leaves one to promote
// a standby. For zones that name fewer than two, it is the set as it is, since a second replica could never schedule.
func (s Set) DatabaseOperatorsAcrossZones(zones []string) (Set, error) {
	if len(distinctZones(zones)) < databaseOperatorReplicas {
		return s, nil
	}
	served := slices.Clone(s.Served)
	for i, m := range served {
		j := slices.IndexFunc(databaseOperators, func(op databaseOperator) bool { return op.name == m.Name })
		if j < 0 {
			continue
		}
		spread, err := databaseOperators[j].spread(m.Bytes)
		if err != nil {
			return Set{}, fmt.Errorf("spreading %s over the zones %v: %w", m.Name, distinctZones(zones), err)
		}
		served[i].Bytes = spread
	}
	s.Served = served
	return s, nil
}

// deployment holds the parts of a rendered Deployment the spread selects its pods by.
type deployment struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Selector map[string]any `yaml:"selector"`
	} `yaml:"spec"`
}

// spread rewrites the Deployment's replicas and affinity in place, so every other byte stays as the chart rendered it,
// and appends a disruption budget over the Deployment's own pod selector.
func (op databaseOperator) spread(manifest []byte) ([]byte, error) {
	d, err := op.deployment(manifest)
	if err != nil {
		return nil, err
	}
	replicas := fmt.Sprintf("  replicas: %d\n", databaseOperatorReplicas)
	if op.rollsOneAtATime {
		replicas += "  strategy:\n    type: RollingUpdate\n    rollingUpdate:\n      maxSurge: 0\n      maxUnavailable: 1\n"
	}
	zoneTerm, err := marshal([]map[string]any{{"labelSelector": d.Spec.Selector, "topologyKey": zoneTopologyKey}})
	if err != nil {
		return nil, err
	}
	affinity := "      affinity:\n        podAntiAffinity:\n          requiredDuringSchedulingIgnoredDuringExecution:\n" +
		tmpl.Indent(zoneTermIndent, string(zoneTerm)) + "\n"
	out := manifest
	for _, edit := range []struct{ field, anchor, replacement string }{
		{"replicas", "\n  replicas: 1\n", "\n" + replicas},
		{"affinity", "\n      affinity:\n", "\n" + affinity},
	} {
		if n := bytes.Count(out, []byte(edit.anchor)); n != 1 {
			return nil, fmt.Errorf("the manifest holds a Deployment's %s %d times, want once", edit.field, n)
		}
		out = bytes.Replace(out, []byte(edit.anchor), []byte(edit.replacement), 1)
	}
	budget, err := disruptionBudget(d)
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return append(append(out, "---\n"...), budget...), nil
}

// deployment finds the operator's Deployment among the manifest's documents.
func (op databaseOperator) deployment(manifest []byte) (deployment, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var d deployment
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return deployment{}, fmt.Errorf("the manifest holds no Deployment %s", op.name)
		}
		if err != nil {
			return deployment{}, fmt.Errorf("decoding the manifest for the Deployment %s: %w", op.name, err)
		}
		if d.Kind == "Deployment" && d.Metadata.Name == op.name {
			if len(d.Spec.Selector) == 0 {
				return deployment{}, fmt.Errorf("the Deployment %s selects no pods", op.name)
			}
			return d, nil
		}
	}
}

// disruptionBudget lets one of the Deployment's pods be evicted at a time, so a node drain never takes both.
func disruptionBudget(d deployment) ([]byte, error) {
	return marshal(map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "PodDisruptionBudget",
		"metadata":   map[string]any{"name": d.Metadata.Name, "namespace": d.Metadata.Namespace},
		"spec":       map[string]any{"maxUnavailable": 1, "selector": d.Spec.Selector},
	})
}

func marshal(v any) ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(yamlIndent)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding %v: %w", v, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding %v: %w", v, err)
	}
	return out.Bytes(), nil
}
