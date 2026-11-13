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
	gateways                                                map[string]renderedGateway
	gatewayCA                                               gateway.CA
	cluster                                                 nodeconfig.Cluster
	enrolmentDigest, issuerBucket, listDigest               string
}

// renderedGateway keeps the issued certificate beside the user data, which holds it only as PEM, so the gateway's label
// and event can name it.
type renderedGateway struct {
	userData    string
	certificate issuedCertificate
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
		spec, err := p.gatewaySpec(ctx, orderable, loc, inst.gateways[loc], s, inst.enrolmentDigest, inst.issuerBucket, inst.listDigest)
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
	for _, loc := range Locations {
		p.notify(ctx, "%s", inst.gateways[loc].certificate.event)
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

// mintSecrets mints a cluster's keys, which a test swaps, since generating each RSA key takes seconds under the race detector.
var mintSecrets = nodeconfig.NewSecrets

// render mints the cluster's keys, gateway CA, enrolment token and issuer bucket, and renders every server's user data.
func (p Plane) render(ctx context.Context, apiAddr, ingressAddr netip.Addr, now time.Time) (installation, error) {
	secrets, err := mintSecrets(now)
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
	// The node report this install renders reads the gateways' mirrors under the CA it mints, and p is this call's copy.
	p.GatewayCA = &gatewayCA
	inst, set, err := p.renderNodes(ctx, apiAddr, now, secrets, token, bucket)
	if err != nil {
		return installation{}, err
	}
	if err := p.renderGateways(&inst, set, apiAddr, ingressAddr, now, secrets); err != nil {
		return installation{}, err
	}
	return inst, nil
}

// buildSet is the component set an install applies, and the base set it renders it from: the default set, or a hosting
// cluster's with its keys.
func (p Plane) buildSet(ctx context.Context, now time.Time, token string) (set, base components.Set, err error) {
	base = components.Default()
	if p.Hosting != nil {
		if base, err = p.hostingSet(ctx, now); err != nil {
			return components.Set{}, components.Set{}, err
		}
	}
	set, err = p.clusterComponents(base, token)
	return set, base, err
}

// renderNodes renders the cluster under the plane's gateway CA, and the user data of each short-lived server of the build.
func (p Plane) renderNodes(ctx context.Context, apiAddr netip.Addr, now time.Time, secrets *nodeconfig.Secrets, token, bucket string) (installation, components.Set, error) {
	set, base, err := p.buildSet(ctx, now, token)
	if err != nil {
		return installation{}, components.Set{}, err
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
		GatewayCA:            p.GatewayCA.Cert,
		ServiceAccountIssuer: issuerURL(bucket, p.Subscription),
		AppsOnWorkers:        p.AppsOnWorkers,
	}
	inst := installation{
		cluster: cluster, enrolmentDigest: enrolmentTokenDigest(token), issuerBucket: bucket, gatewayCA: *p.GatewayCA,
		gateways: map[string]renderedGateway{},
	}
	carried := cluster
	var fetched []components.Manifest
	carried.Components.Inline, fetched = splitFetched(set, base)
	config, err := nodeconfig.Render(carried, nodeconfig.Node{})
	if err != nil {
		return installation{}, components.Set{}, err
	}
	var fetch *servingFetch
	if len(fetched) > 0 {
		if inst.servingUserData, fetch, err = servingUserData(fetched, now); err != nil {
			return installation{}, components.Set{}, err
		}
	}
	if inst.builderUserData, err = builderUserData("builder", config, p.GatewayCA.Cert, fetch); err != nil {
		return installation{}, components.Set{}, err
	}
	workerConfig, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{})
	if err != nil {
		return installation{}, components.Set{}, err
	}
	if inst.workerBuilderUserData, err = builderUserData("worker builder", workerConfig, p.GatewayCA.Cert, nil); err != nil {
		return installation{}, components.Set{}, err
	}
	return inst, set, nil
}

// renderGateways renders each location's gateway from the release's list, the first with the etcd bootstrap.
func (p Plane) renderGateways(inst *installation, set components.Set, apiAddr, ingressAddr netip.Addr, now time.Time, secrets *nodeconfig.Secrets) error {
	list, err := p.gatewayList()
	if err != nil {
		return err
	}
	if err := sameList(set, list); err != nil {
		return err
	}
	inst.listDigest = list.Digest()
	// The address only fills the talosconfig's defaults, since the bootstrap names the node it finds on every call.
	talosconfig, err := secrets.AdminTalosconfig(apiAddr, adminTalosconfigTTL, now)
	if err != nil {
		return err
	}
	bootstrap := &firstNodeBootstrap{talosconfig: talosconfig, deadline: now.Add(adminTalosconfigTTL)}
	for _, loc := range Locations {
		if loc != GatewayLocation {
			bootstrap = nil
		}
		rendered, err := p.renderGateway(loc, ingressAddr, now, *p.GatewayCA, list, bootstrap)
		if err != nil {
			return err
		}
		inst.gateways[loc] = rendered
	}
	return nil
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
func (p Plane) renderGateway(loc string, ingressAddr netip.Addr, now time.Time, ca gateway.CA, list gateway.List, bootstrap *firstNodeBootstrap) (renderedGateway, error) {
	address := netip.MustParseAddr(GatewayAddresses[loc])
	tls, err := ca.Issue(address, now, gatewayCertValidity)
	if err != nil {
		return renderedGateway{}, err
	}
	certificate, err := issued(loc, tls)
	if err != nil {
		return renderedGateway{}, err
	}
	tools, err := components.ParseImage(toolsImage())
	if err != nil {
		return renderedGateway{}, fmt.Errorf("the tools image: %w", err)
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
		return renderedGateway{}, err
	}
	userData, err := p.gatewayUserData(kitPart, bootstrap)
	return renderedGateway{userData: userData, certificate: certificate}, err
}
