package controlplane

import (
	"errors"
	"slices"

	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// Release is what an upgrade carries to the running cluster through its repository: every manifest the build applies
// but the hosting set's inline ones, which only an install's keys render, and the cluster's own Application, through
// which the carriage runs. It renders the credentials without the enrolment token, whose Secret rollout leaves with the
// build as it does every Secret holding data.
func (p Plane) Release() (rollout.Release, error) {
	if p.Hosting == nil && p.RepoURL == "" || p.Hosting != nil && p.Hosting.Repository == "" {
		return rollout.Release{}, errors.New("the cluster's credentials name the cluster repository, and the plane names none")
	}
	set, _, err := p.gatewayComponents()
	if err != nil {
		return rollout.Release{}, err
	}
	own, _, err := p.ownComponents("")
	if err != nil {
		return rollout.Release{}, err
	}
	return rollout.Release{Components: slices.Concat(set.Served, set.Inline, own)}, nil
}
