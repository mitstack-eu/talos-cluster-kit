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

// installation is what one install renders: both servers' user data, the cluster, the enrolment digest and the issuer bucket.
type installation struct {
	gatewayUserData, builderUserData string
	cluster                          nodeconfig.Cluster
	enrolmentDigest, issuerBucket    string
}

// install creates both servers stopped and off the network, since Hetzner gives a server joining with no address the
// first free one, which would be the gateway's own.
func (p Plane) install(ctx context.Context, s site) error {
	addr, err := netip.ParseAddr(s.apiAddress)
	if err != nil {
		return fmt.Errorf("the API load balancer has no address on network %d: %w", s.network.ID, err)
	}
	inst, err := p.render(addr, p.now())
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
	_, err = p.API.CreateServers(ctx, ServerSpec{
		Name:        p.Name(RoleGateway),
		ServerType:  gatewayType,
		Location:    GatewayLocation,
		Image:       gatewayImage,
		UserData:    inst.gatewayUserData,
		Labels:      gatewayLabels,
		FirewallIDs: []int64{s.firewall.ID},
		PublicIPv4:  true,
		Stopped:     true,
	}, ServerSpec{
		Name:        p.Name(RoleBuilder),
		ServerType:  p.ServerType,
		Location:    GatewayLocation,
		Image:       builderImage,
		UserData:    inst.builderUserData,
		Labels:      p.Labels(RoleBuilder),
		FirewallIDs: []int64{s.firewall.ID},
		Stopped:     true,
	})
	if err != nil {
		return p.rollBackInstall(ctx, fmt.Errorf("creating the gateway and the image builder: %w", err))
	}
	if p.Installed != nil {
		if err := p.Installed(ctx, s.network, inst.cluster); err != nil {
			return p.rollBackInstall(ctx, err)
		}
	}
	return nil
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

// rollBackInstall deletes whatever gateway and builder a failed install made, since the keys they were made with
// are gone with this call, so the next call installs anew.
func (p Plane) rollBackInstall(ctx context.Context, cause error) error {
	errs := []error{cause}
	for _, r := range []Role{RoleGateway, RoleBuilder} {
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

// render mints the cluster's keys, gateway CA, enrolment token and issuer bucket, and renders both user data.
func (p Plane) render(apiAddr netip.Addr, now time.Time) (installation, error) {
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
	set, err := p.clusterComponents(token)
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
	config, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	if err != nil {
		return installation{}, err
	}
	gatewayData, err := p.renderGateway(apiAddr, now, gatewayCA, set, secrets)
	if err != nil {
		return installation{}, err
	}
	builderData, err := builderUserData(config)
	if err != nil {
		return installation{}, err
	}
	return installation{
		gatewayUserData: gatewayData, builderUserData: builderData, cluster: cluster,
		enrolmentDigest: enrolmentTokenDigest(token), issuerBucket: bucket,
	}, nil
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
