package controlplane

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
)

// ErrNoIngressAddress is why a release cannot be rendered yet: Traefik publishes the ingress load balancer's public
// address, and the state holds none a block could commit.
var ErrNoIngressAddress = errors.New("the release's Traefik has no ingress address to publish")

// Release is every manifest the build applies but those only an install's keys render and the cluster's own
// Application, through which the carriage runs, with the credentials rendered without the enrolment token and with
// Traefik publishing the address st holds for the ingress load balancer.
func (p Plane) Release(st State) (rollout.Release, error) {
	if p.Hosting == nil && p.RepoURL == "" || p.Hosting != nil && p.Hosting.Repository == "" {
		return rollout.Release{}, errors.New("the cluster's credentials name the cluster repository, and the plane names none")
	}
	set, _, err := p.gatewayComponents()
	if err != nil {
		return rollout.Release{}, err
	}
	if set, err = publishingIngress(set, st); err != nil {
		return rollout.Release{}, err
	}
	own, _, err := p.ownComponents("")
	if err != nil {
		return rollout.Release{}, err
	}
	return rollout.Release{Components: slices.Concat(set.Served, set.Inline, own), ToolsImage: toolsImage()}, nil
}

// The gateways serve the set without an address, since one tools image is built for every cluster.
func publishingIngress(set components.Set, st State) (components.Set, error) {
	if !set.RunsTraefik() {
		return set, nil
	}
	byLocation := st.IngressAddresses()
	var addresses []netip.Addr
	for _, loc := range slices.Sorted(maps.Keys(byLocation)) {
		address, err := netip.ParseAddr(byLocation[loc])
		if err != nil {
			return components.Set{}, fmt.Errorf("%w: the state holds %q as the public address of the ingress load balancer in %s", ErrNoIngressAddress, byLocation[loc], loc)
		}
		addresses = append(addresses, address)
	}
	return set.PublishingIngress(addresses)
}
