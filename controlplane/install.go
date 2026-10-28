package controlplane

// The cluster's keys exist only in this call's memory, the builder's user data, the snapshot it becomes and whatever
// the block's Installed hook makes from them, so the gateway and the builder are made in the one call that mints them.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

const (
	kitModule = "github.com/mitstack-eu/talos-cluster-kit"

	gatewayCAValidity   = 10 * 365 * 24 * time.Hour
	gatewayCertValidity = 365 * 24 * time.Hour
	// adminTalosconfigTTL covers the build, the snapshot and the node's first boot, after which the gateway's copy is worthless.
	adminTalosconfigTTL = 2 * time.Hour
)

// installation is what one install renders, with no serving server for a cluster that hosts no platform.
type installation struct {
	gatewayUserData, builderUserData, servingUserData, workerBuilderUserData string
	cluster                                                                  nodeconfig.Cluster
	enrolmentDigest, issuerBucket                                            string
}

// install creates every server stopped and off the network, since Hetzner gives a server joining with no address the
// first free one, which would be the gateway's own.
func (p Plane) install(ctx context.Context, s site) error {
	addr, err := netip.ParseAddr(s.apiAddress)
	if err != nil {
		return fmt.Errorf("the API load balancer has no address on network %d: %w", s.network.ID, err)
	}
	inst, err := p.render(ctx, addr, p.now())
	if err != nil {
		return err
	}
	gatewayType, err := p.orderableGatewayType(ctx)
	if err != nil {
		return err
	}
	gatewayLabels := p.Labels(RoleGateway)
	gatewayLabels[KeyEnrolmentSHA256] = inst.enrolmentDigest
	gatewayLabels[KeyIssuerBucket] = inst.issuerBucket
	specs := []ServerSpec{{
		Name:        p.Name(RoleGateway),
		ServerType:  gatewayType,
		Location:    GatewayLocation,
		Image:       gatewayImage,
		UserData:    inst.gatewayUserData,
		Labels:      gatewayLabels,
		FirewallIDs: []int64{s.firewall.ID},
		PublicIPv4:  true,
		Stopped:     true,
	}, p.builderSpec(RoleBuilder, inst.builderUserData, s), p.builderSpec(RoleWorkerBuilder, inst.workerBuilderUserData, s)}
	if inst.servingUserData != "" {
		specs = append(specs, ServerSpec{
			Name:        p.Name(RoleServing),
			ServerType:  gatewayType,
			Location:    GatewayLocation,
			Image:       gatewayImage,
			UserData:    inst.servingUserData,
			Labels:      p.Labels(RoleServing),
			FirewallIDs: []int64{s.firewall.ID},
			Stopped:     true,
		})
	}
	if _, err := p.API.CreateServers(ctx, specs...); err != nil {
		return p.rollBackInstall(ctx, fmt.Errorf("creating the gateway and the builders: %w", err))
	}
	if p.Installed != nil {
		if err := p.Installed(ctx, s.network, inst.cluster); err != nil {
			return p.rollBackInstall(ctx, err)
		}
	}
	return nil
}

func (p Plane) builderSpec(r Role, userData string, s site) ServerSpec {
	return ServerSpec{
		Name:        p.Name(r),
		ServerType:  p.ServerType,
		Location:    GatewayLocation,
		Image:       builderImage,
		UserData:    userData,
		Labels:      p.Labels(r),
		FirewallIDs: []int64{s.firewall.ID},
		Stopped:     true,
	}
}

// orderableGatewayType is the first of GatewayServerTypes Hetzner reads as orderable in the gateway's location.
func (p Plane) orderableGatewayType(ctx context.Context) (string, error) {
	available, err := p.API.AvailableServerTypes(ctx, GatewayLocation)
	if err != nil {
		return "", err
	}
	for _, t := range GatewayServerTypes {
		if slices.Contains(available, t) {
			return t, nil
		}
	}
	return "", refuse("server-unavailable",
		"Hetzner can order none of the gateway's server types %s in %s now; subscribe again later",
		strings.Join(GatewayServerTypes, ", "), GatewayLocation)
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
func (p Plane) render(ctx context.Context, apiAddr netip.Addr, now time.Time) (installation, error) {
	secrets, err := nodeconfig.NewSecrets(now)
	if err != nil {
		return installation{}, err
	}
	gatewayCA, err := gateway.NewCA(now, gatewayCAValidity)
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
	set, err := p.clusterComponents(base, token)
	if err != nil {
		return installation{}, err
	}
	gatewayAddr := netip.MustParseAddr(GatewayAddress)
	cluster := nodeconfig.Cluster{
		Name:                 p.clusterName(),
		SubscriptionID:       p.Subscription,
		Profile:              nodeconfig.HCloud,
		Components:           set,
		Secrets:              secrets,
		Network:              netip.MustParsePrefix(NetworkIPRange),
		APIAddresses:         []netip.Addr{apiAddr},
		Gateways:             []netip.Addr{gatewayAddr},
		GatewayCA:            gatewayCA.Cert,
		ServiceAccountIssuer: issuerURL(bucket, p.Subscription),
		AppsOnWorkers:        p.AppsOnWorkers,
	}
	inst := installation{cluster: cluster, enrolmentDigest: enrolmentTokenDigest(token), issuerBucket: bucket}
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
	if inst.builderUserData, err = builderUserData("builder", config, fetch); err != nil {
		return installation{}, err
	}
	workerConfig, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{})
	if err != nil {
		return installation{}, err
	}
	if inst.workerBuilderUserData, err = builderUserData("worker builder", workerConfig, nil); err != nil {
		return installation{}, err
	}
	if inst.gatewayUserData, err = p.renderGateway(apiAddr, now, gatewayCA, set, secrets); err != nil {
		return installation{}, err
	}
	return inst, nil
}

func (p Plane) renderGateway(apiAddr netip.Addr, now time.Time, ca gateway.CA, set components.Set, secrets *nodeconfig.Secrets) (string, error) {
	gatewayAddr := netip.MustParseAddr(GatewayAddress)
	tls, err := ca.Issue(gatewayAddr, now, gatewayCertValidity)
	if err != nil {
		return "", err
	}
	release, err := kitRelease()
	if err != nil {
		return "", err
	}
	kitPart, err := gateway.Render(gateway.Inputs{
		Address:         gatewayAddr,
		Network:         netip.MustParsePrefix(NetworkIPRange),
		TLS:             tls,
		Components:      set,
		ManifestBaseURL: gateway.ReleaseURL(release),
	})
	if err != nil {
		return "", err
	}
	// The address only fills the talosconfig's defaults, since the bootstrap names the node it finds on every call.
	talosconfig, err := secrets.AdminTalosconfig(apiAddr, adminTalosconfigTTL, now)
	if err != nil {
		return "", err
	}
	return p.gatewayUserData(kitPart, firstNodeBootstrap{talosconfig: talosconfig, deadline: now.Add(adminTalosconfigTTL)})
}

// kitRelease is the talos-cluster-kit tag the binary is built against, whose files the gateway fetches, or the
// main module's version when the binary is the kit's own test.
func kitRelease() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", errors.New("the binary carries no build information, so the talos-cluster-kit release is unknown")
	}
	if info.Main.Path == kitModule {
		return info.Main.Version, nil
	}
	for _, dep := range info.Deps {
		if dep.Path == kitModule {
			return dep.Version, nil
		}
	}
	return "", fmt.Errorf("the binary is not built against %s", kitModule)
}
