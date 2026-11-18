package robotcluster

// Transition code for a cluster hcloud-hrobot-talos-cluster built before its 0.4.0, to be deleted in one commit once no cluster predates that release.

import (
	"context"
	"fmt"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
)

// refusePredatingZones refuses such a cluster, since this release can neither report its one gateway nor sign another.
func refusePredatingZones(st controlplane.State) error {
	if !st.SingleGateway() {
		return nil
	}
	return handler.Refuse("gateway-predates-zones",
		"the cluster was built by a release before 0.4.0, with one gateway and no kept CA key, so this release cannot carry it on and nothing was changed; unsubscribe and subscribe again to rebuild the cluster")
}

// TypeObservers read a gateway recorded under the server type, as a release before 0.4.0 recorded it.
func (k *Cluster) TypeObservers() map[string]handler.Observer {
	return map[string]handler.Observer{TypeServer: legacyGatewayObserver{k: k}}
}

type legacyGatewayObserver struct{ k *Cluster }

func (o legacyGatewayObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	if r.Desired.Resource != ResourceGateway {
		return operator.Unknown(fmt.Sprintf("the block declares no resource %q", r.Desired.Resource)), nil
	}
	return labelledObserver{k: o.k, kind: "gateway", list: func(st controlplane.State) []int64 {
		return controlplane.IDs(st.Servers[controlplane.RoleGateway], controlplane.ServerID)
	}}.ObserveResource(ctx, r)
}
