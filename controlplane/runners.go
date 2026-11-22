package controlplane

import (
	_ "embed"
	"text/template"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

//go:embed manifests/runner-token.yaml
var runnerTokenTemplate string

var runnerToken = template.Must(template.New("runner-token").Parse(runnerTokenTemplate))

// withRunners adds the runner services and the scale set to the set of a plane that asks for runners, so the build and
// every release render the same runner manifests.
func (p Plane) withRunners(set components.Set) (components.Set, error) {
	if p.Runners == nil {
		return set, nil
	}
	return set.WithRunners(*p.Runners)
}

// servedSet is the default set, or a hosting cluster's without its keys, which a plane that asks for runners serves
// with the runner services.
func (p Plane) servedSet() components.Set {
	set := components.Default()
	if p.Hosting != nil {
		set = components.HostingServed()
	}
	if p.Runners != nil {
		set = set.WithRunnerServices()
	}
	return set
}

func (p Plane) runnerTokenManifests() ([]components.Manifest, error) {
	if p.Runners == nil {
		return nil, nil
	}
	out, err := execute(runnerToken, map[string]any{
		"Namespace":        nodeconfig.TalosAPINamespace,
		"RunnersNamespace": components.RunnersNamespace,
		"TokenSecret":      components.RunnerTokenSecret,
		"Image":            toolsImage(),
		"Script":           clusterTokensScript,
		"PlatformURL":      p.PlatformURL,
		"LeaseResource":    p.Runners.LeaseResource,
		"BurstLabel":       components.BurstLabel,
	})
	if err != nil {
		return nil, err
	}
	return []components.Manifest{{Name: components.RunnerTokenName, Path: manifestDir + "runner-token.yaml", Bytes: out}}, nil
}
