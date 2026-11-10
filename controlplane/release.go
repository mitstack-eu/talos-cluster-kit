package controlplane

import (
	"errors"
	"slices"

	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// Release is every manifest the build applies but those only an install's keys render and the cluster's own
// Application, through which the carriage runs, with the credentials rendered without the enrolment token.
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
