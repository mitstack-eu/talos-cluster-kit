package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// DaemonSetPods is the pod of each DaemonSet the plane's release renders, with its placement and what the scheduler
// reserves for it, so the room a new node offers a waiting pod leaves out what the DaemonSets it admits take.
func (p Plane) DaemonSetPods() ([]report.WaitingPod, error) {
	set, err := p.withRunners(p.servedSet())
	if err != nil {
		return nil, err
	}
	telemetry, err := set.TelemetryCollector(p.Telemetry, p.Subscription)
	if err != nil {
		return nil, err
	}
	var pods []report.WaitingPod
	for _, m := range slices.Concat(set.Served, set.Inline, telemetry) {
		found, err := daemonSetPods(m)
		if err != nil {
			return nil, err
		}
		pods = append(pods, found...)
	}
	return pods, nil
}

func daemonSetPods(m components.Manifest) ([]report.WaitingPod, error) {
	var pods []report.WaitingPod
	dec := yaml.NewDecoder(bytes.NewReader(m.Bytes))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec map[string]any `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return pods, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decoding the manifest %s: %w", m.Name, err)
		}
		if doc.Kind != "DaemonSet" {
			continue
		}
		name := doc.Metadata.Namespace + "/" + doc.Metadata.Name
		spec, err := json.Marshal(doc.Spec.Template.Spec)
		if err != nil {
			return nil, fmt.Errorf("reading the pod template of the DaemonSet %s in the manifest %s: %w", name, m.Name, err)
		}
		pod, err := report.TemplatePod(name, spec)
		if err != nil {
			return nil, err
		}
		pods = append(pods, pod)
	}
}
