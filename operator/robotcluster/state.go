package robotcluster

import (
	"context"
	"errors"
	"strconv"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

// EnsureResourcesOwning takes one step and then reads back, by label and Robot name, what it converged. The gateway
// CA an install mints leaves as the block's own secret, and since a failed answer drops it, a failure after the
// install answers Wait.
func (k *Cluster) EnsureResourcesOwning(ctx context.Context, d contract.Desired) ([]contract.CreatedResource, map[string]string, error) {
	own, err := k.ensure(ctx, d)
	var created []contract.CreatedResource
	if err = WaitOutRobotLimit(err); err == nil {
		created, err = k.readCreated(ctx, d)
	}
	var w *handler.Waiting
	if err != nil && own != nil && !errors.As(err, &w) {
		return nil, own, handler.Wait("the install minted the gateway CA, which the block keeps, and the call then failed, so the next call carries on: %v", err)
	}
	return created, own, err
}

func (k *Cluster) readCreated(ctx context.Context, d contract.Desired) ([]contract.CreatedResource, error) {
	p := k.PlaneFor(k.cfg.CloudAPI(d.Secrets["hetzner"]["api_token"]), d)
	st, err := p.Read(ctx)
	if err != nil {
		return nil, err
	}
	out := createdResources(st, k.cfg.ClusterType, d.SubscriptionID, clusterrepo.For(d.Secrets["github"]["owner"], d.Params["appName"]))
	t, _, ok := k.robotReader(ctx, d)
	if !ok {
		return out, nil
	}
	robotSt, err := t.read(ctx)
	if err != nil {
		return nil, WaitOutRobotLimit(err)
	}
	return append(out, robotSt.createdResources(t)...), nil
}

// createdResources reports each declared Cloud and GitHub resource by its identity, leaving out one not yet made.
func createdResources(st controlplane.State, clusterType, subID string, repo clusterrepo.Repo) []contract.CreatedResource {
	var out []contract.CreatedResource
	add := func(name, typ, key string, id int64, ok bool) {
		if ok {
			out = append(out, contract.CreatedResource{Name: name, Type: typ, Identity: map[string]string{key: strconv.FormatInt(id, 10)}})
		}
	}
	gw := controlplane.GatewayLocation
	network, networkOK := st.Network()
	add(ResourceNetwork, ResourceNetwork, "networkId", network.ID, networkOK)
	firewall, firewallOK := st.Firewall()
	add(ResourceFirewall, ResourceFirewall, "firewallId", firewall.ID, firewallOK)
	api, apiOK := st.LoadBalancerIn(controlplane.RoleAPI, gw)
	add(Located(ResourceAPILoadBalancer, gw), TypeLoadBalancer, "loadBalancerId", api.ID, apiOK)
	ingress, ingressOK := st.LoadBalancerIn(controlplane.RoleIngress, gw)
	add(Located(ResourceIngressLoadBalancer, gw), TypeLoadBalancer, "loadBalancerId", ingress.ID, ingressOK)
	repoIdentity := map[string]string{"repo": repo.FullName()}
	out = append(out,
		contract.CreatedResource{Name: ResourceRepository, Type: TypeRepository, Identity: repoIdentity},
		contract.CreatedResource{Name: ResourceSeedCommit, Type: ResourceSeedCommit, Identity: repoIdentity})
	for _, loc := range controlplane.Locations {
		gateway, gatewayOK := st.GatewayIn(loc)
		add(Located(ResourceGateway, loc), TypeServer, "serverId", gateway.ID, gatewayOK)
		node, nodeOK := st.NodeIn(loc)
		add(Located(ResourceNode, loc), TypeServer, "serverId", node.ID, nodeOK)
	}
	out = append(out, cloudWorkerResources(st)...)
	return append(out, contract.CreatedResource{Name: clusterType, Type: clusterType, Identity: map[string]string{"subscriptionId": subID}})
}

// createdResources reports the worker's Robot resources once the server carries the subscription's name.
func (st robotState) createdResources(t robotTarget) []contract.CreatedResource {
	if st.server.Name != t.name {
		return nil
	}
	server := map[string]string{"serverNumber": strconv.FormatInt(t.server, 10)}
	out := []contract.CreatedResource{
		{Name: ResourceDedicatedServer, Type: TypeDedicatedServer, Identity: server},
		{Name: ResourceRobotFirewall, Type: TypeRobotFirewall, Identity: server},
	}
	if st.vswitches == 1 {
		out = append(out, contract.CreatedResource{Name: ResourceVSwitch, Type: TypeVSwitch, Identity: map[string]string{"vswitchId": strconv.FormatInt(st.vswitch.ID, 10)}})
	}
	return out
}
