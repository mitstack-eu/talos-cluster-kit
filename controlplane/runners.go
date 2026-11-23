package controlplane

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

//go:embed manifests/runner-token.yaml
var runnerTokenTemplate string

var runnerToken = template.Must(template.New("runner-token").Parse(runnerTokenTemplate))

// CauseRunnerCacheDown is the cause a plane with runners reads unhealthy with while Argo CD reads a package cache as
// other than healthy, word for word as the catalogue type lists it under failures.
const CauseRunnerCacheDown = "a runner package cache does not serve, so each job's npm install fails while its Go modules and Docker Hub images come from the internet; read the cache pod's log"

// withRunners adds the runner services, the scale set and its package caches to the set of a plane that asks for
// runners, so the build and every release render the same runner manifests.
func (p Plane) withRunners(set components.Set) (components.Set, error) {
	if p.Runners == nil {
		return set, nil
	}
	return set.WithRunners(*p.Runners, toolsImage())
}

// runnerCacheDown names each package cache the newest node report reads Argo CD's health of as other than healthy,
// and nothing for a plane without runners or a report that cannot judge the caches.
func (p Plane) runnerCacheDown(ctx context.Context) (cause, detail string, err error) {
	if p.Runners == nil || p.ReadReport == nil {
		return "", "", nil
	}
	read, err := p.ReadReport(ctx, time.Time{})
	if err != nil || read == nil {
		return "", "", err
	}
	deployments, readable, err := read.AppliedResources(rollout.ApplicationPrefix+components.RunnerCachesName, "Deployment")
	if err != nil || !readable {
		return "", "", err
	}
	var down []string
	for _, cache := range components.RunnerCaches {
		applied, ok := deployments[cache]
		switch {
		case !ok:
			down = append(down, fmt.Sprintf("the caches' Application lists no Deployment %s", cache))
		case applied.Health != "Healthy":
			reading := fmt.Sprintf("Argo CD reads %s %s", cache, applied.Health)
			if applied.Message != "" {
				reading += ": " + applied.Message
			}
			down = append(down, reading)
		}
	}
	if len(down) == 0 {
		return "", "", nil
	}
	return CauseRunnerCacheDown, fmt.Sprintf("the node report read at %s: %s", stampOf(read.At), strings.Join(down, "; ")), nil
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
		"Namespace":         nodeconfig.TalosAPINamespace,
		"RunnersNamespace":  components.RunnersNamespace,
		"TokenSecret":       components.RunnerTokenSecret,
		"SweepTokenSecret":  components.RunnerSweepTokenSecret,
		"Image":             toolsImage(),
		"Script":            clusterTokensScript,
		"PlatformURL":       p.PlatformURL,
		"LeaseResource":     p.Runners.LeaseResource,
		"BurstLabel":        components.BurstLabel,
		"ControlPlaneLabel": components.ControlPlaneLabel,
	})
	if err != nil {
		return nil, err
	}
	return []components.Manifest{{Name: components.RunnerTokenName, Path: manifestDir + "runner-token.yaml", Bytes: out}}, nil
}
