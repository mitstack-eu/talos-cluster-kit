package controlplane

// The cluster's keys exist only in this call's memory, the builder's user data, the snapshot it becomes and whatever
// the block's Installed hook makes from them, so the gateways and the builder are made in the one call that mints them.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

const (
	gatewayCAValidity   = 10 * 365 * 24 * time.Hour
	gatewayCertValidity = 365 * 24 * time.Hour
	// adminTalosconfigTTL covers the build, the snapshot and the node's first boot, after which the gateway's copy is worthless.
	adminTalosconfigTTL = 2 * time.Hour
)

// installation is what one install renders, with no serving server for a cluster that hosts no platform. Each
// location's gateway has its own user data and certificate.
type installation struct {
	builderUserData, servingUserData, workerBuilderUserData string
	gatewayUserData                                         map[string]string
	certificateEvents                                       []string
	gatewayCA                                               gateway.CA
	cluster                                                 nodeconfig.Cluster
	enrolmentDigest, issuerBucket, listDigest               string
}

// install creates every server stopped and off the network, since Hetzner gives a server joining with no address the
// first free one, which would be the gateway's own.
func (p Plane) install(ctx context.Context, s site) error {
	addr, err := netip.ParseAddr(s.apiAddress)
	if err != nil {
		return fmt.Errorf("the API load balancer has no address on network %d: %w", s.network.ID, err)
	}
	ingress, err := netip.ParseAddr(s.ingressAddress)
	if err != nil {
		return fmt.Errorf("the ingress load balancer has no public address: %w", err)
	}
	inst, err := p.render(ctx, addr, ingress, p.now())
	if err != nil {
		return err
	}
	orderable := &OrderableTypes{API: p.API}
	var specs []ServerSpec
	for _, loc := range Locations {
		spec, err := p.gatewaySpec(ctx, orderable, loc, inst.gatewayUserData[loc], s, inst.enrolmentDigest, inst.issuerBucket, inst.listDigest)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
	}
	for _, b := range []struct {
		role     Role
		userData string
	}{{RoleBuilder, inst.builderUserData}, {RoleWorkerBuilder, inst.workerBuilderUserData}} {
		spec, err := p.builderSpec(ctx, orderable, b.role, b.userData, s)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
	}
	if inst.servingUserData != "" {
		specs = append(specs, ServerSpec{
			Name:        p.Name(RoleServing),
			ServerType:  specs[0].ServerType,
			Location:    GatewayLocation,
			Image:       gatewayImage,
			UserData:    inst.servingUserData,
			Labels:      p.Labels(RoleServing),
			FirewallIDs: []int64{s.firewall.ID},
			Stopped:     true,
		})
	}
	if _, err := p.API.CreateServers(ctx, specs...); err != nil {
		return p.rollBackInstall(ctx, fmt.Errorf("creating the gateways and the builders: %w", err))
	}
	if p.Installed != nil {
		if err := p.Installed(ctx, s.network, inst.cluster, inst.gatewayCA); err != nil {
			return p.rollBackInstall(ctx, err)
		}
	}
	for _, event := range inst.certificateEvents {
		p.notify(ctx, "%s", event)
	}
	return nil
}

func (p Plane) builderSpec(ctx context.Context, orderable *OrderableTypes, r Role, userData string, s site) (ServerSpec, error) {
	serverType, err := orderable.First(ctx, string(r), p.ServerTypes, GatewayLocation)
	if err != nil {
		return ServerSpec{}, err
	}
	return ServerSpec{
		Name:        p.Name(r),
		ServerType:  serverType,
		Location:    GatewayLocation,
		Image:       builderImage,
		UserData:    userData,
		Labels:      p.Labels(r),
		FirewallIDs: []int64{s.firewall.ID},
		Stopped:     true,
	}, nil
}

// rollBackInstall deletes whatever servers a failed install made, since the keys they were made with are gone with
// this call, so the next call installs anew.
func (p Plane) rollBackInstall(ctx context.Context, cause error) error {
	errs := []error{cause}
	for _, r := range []Role{RoleGateway, RoleBuilder, RoleServing, RoleWorkerBuilder} {
		servers, err := p.API.ListServers(ctx, p.RoleSelector(r))
		if err != nil {
			errs = append(errs, fmt.Errorf("listing the %s of the failed install: %w", r, err))
			continue
		}
		for _, s := range servers {
			if err := p.API.DeleteServer(ctx, s.ID); err != nil {
				errs = append(errs, fmt.Errorf("deleting the %s %d of the failed install: %w", r, s.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// render mints the cluster's keys, gateway CA, enrolment token and issuer bucket, and renders every server's user data.
func (p Plane) render(ctx context.Context, apiAddr, ingressAddr netip.Addr, now time.Time) (installation, error) {
	secrets, err := nodeconfig.NewSecrets(now)
	if err != nil {
		return installation{}, err
	}
	gatewayCA, err := gateway.NewCA(now, gatewayCAValidity, gatewayAddrs())
	if err != nil {
		return installation{}, err
	}
	token, err := mintEnrolmentToken()
	if err != nil {
		return installation{}, err
	}
	bucket, err := mintIssuerBucket()
	if err != nil {
		return installation{}, err
	}
	base := components.Default()
	if p.Hosting != nil {
		if base, err = p.hostingSet(ctx, now); err != nil {
			return installation{}, err
		}
	}
	// The node report trusts the CA this install mints for the gateways' mirrors.
	p.GatewayCA = &gatewayCA
	set, err := p.clusterComponents(base, token)
	if err != nil {
		return installation{}, err
	}
	cluster := nodeconfig.Cluster{
		Name:                 p.clusterName(),
		SubscriptionID:       p.Subscription,
		Profile:              nodeconfig.HCloud,
		Components:           set,
		Secrets:              secrets,
		Network:              netip.MustParsePrefix(NetworkIPRange),
		APIAddresses:         []netip.Addr{apiAddr},
		Gateways:             gatewayAddrs(),
		GatewayCA:            gatewayCA.Cert,
		ServiceAccountIssuer: issuerURL(bucket, p.Subscription),
		AppsOnWorkers:        p.AppsOnWorkers,
	}
	inst := installation{
		cluster: cluster, enrolmentDigest: enrolmentTokenDigest(token), issuerBucket: bucket, gatewayCA: gatewayCA,
		gatewayUserData: map[string]string{},
	}
	carried, fetched := cluster, []components.Manifest(nil)
	if p.Hosting != nil {
		carried.Components.Inline, fetched = splitHosting(set, base)
	}
	config, err := nodeconfig.Render(carried, nodeconfig.Node{})
	if err != nil {
		return installation{}, err
	}
	var fetch *hostingFetch
	if p.Hosting != nil {
		if inst.servingUserData, fetch, err = servingUserData(fetched, now); err != nil {
			return installation{}, err
		}
	}
	if inst.builderUserData, err = builderUserData("builder", config, gatewayCA.Cert, fetch); err != nil {
		return installation{}, err
	}
	workerConfig, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{})
	if err != nil {
		return installation{}, err
	}
	if inst.workerBuilderUserData, err = builderUserData("worker builder", workerConfig, gatewayCA.Cert, nil); err != nil {
		return installation{}, err
	}
	list, err := p.gatewayList()
	if err != nil {
		return installation{}, err
	}
	if err := sameList(set, list); err != nil {
		return installation{}, err
	}
	inst.listDigest = list.Digest()
	// The address only fills the talosconfig's defaults, since the bootstrap names the node it finds on every call.
	talosconfig, err := secrets.AdminTalosconfig(apiAddr, adminTalosconfigTTL, now)
	if err != nil {
		return installation{}, err
	}
	bootstrap := &firstNodeBootstrap{talosconfig: talosconfig, deadline: now.Add(adminTalosconfigTTL)}
	for _, loc := range Locations {
		if loc != GatewayLocation {
			bootstrap = nil
		}
		userData, tls, err := p.renderGateway(loc, ingressAddr, now, gatewayCA, list, bootstrap)
		if err != nil {
			return installation{}, err
		}
		event, err := certificateEvent(loc, tls)
		if err != nil {
			return installation{}, err
		}
		inst.gatewayUserData[loc], inst.certificateEvents = userData, append(inst.certificateEvents, event)
	}
	return inst, nil
}

// sameList refuses an install whose gateways would mirror or serve other than the cluster's whole set needs, since a
// replacement renders the gateways' list alone.
func sameList(cluster components.Set, gateways gateway.List) error {
	want, err := gateway.ListDigest(cluster)
	if err != nil {
		return err
	}
	if gateways.Digest() != want {
		return errors.New("the gateways' set mirrors or serves other than the cluster's whole set, so a replacement gateway would not serve what the nodes pull")
	}
	return nil
}

// renderGateway issues the location's certificate from ca and renders its user data, with the first-node bootstrap
// only when bootstrap is set.
func (p Plane) renderGateway(loc string, ingressAddr netip.Addr, now time.Time, ca gateway.CA, list gateway.List, bootstrap *firstNodeBootstrap) (string, gateway.TLS, error) {
	address := netip.MustParseAddr(GatewayAddresses[loc])
	tls, err := ca.Issue(address, now, gatewayCertValidity)
	if err != nil {
		return "", gateway.TLS{}, err
	}
	tools, err := components.ParseImage(toolsImage())
	if err != nil {
		return "", gateway.TLS{}, fmt.Errorf("the tools image: %w", err)
	}
	kitPart, err := gateway.Render(gateway.Inputs{
		Address: address,
		Network: netip.MustParsePrefix(NetworkIPRange),
		Ingress: ingressAddr,
		TLS:     tls,
		List:    list,
		Tools:   tools,
		Gzipped: true,
	})
	if err != nil {
		return "", gateway.TLS{}, err
	}
	userData, err := p.gatewayUserData(kitPart, bootstrap)
	return userData, tls, err
}
