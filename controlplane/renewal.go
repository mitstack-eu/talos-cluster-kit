package controlplane

// A gateway's serving certificate is renewed by replacing the gateway, since Hetzner fixes a server's user data, and
// with it the certificate's key, when it creates the server.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
)

// GatewayCertRenewBefore is how long before a gateway's serving certificate lapses the plane begins replacing the
// gateway, which leaves a month of calls for the three locations to take their turn.
const GatewayCertRenewBefore = 30 * 24 * time.Hour

// KeyGatewayCertExpires on a gateway is when its serving certificate lapses, in Unix seconds, since the plane keeps no
// state between calls and reads no event back.
const KeyGatewayCertExpires = "mitstack.dev/gateway-cert-expires"

// renewalPlan is the plan a gateway replaced for a new certificate carries as KeyRemovalFor.
const renewalPlan = "certificate-renewal"

// issuedCertificate is what the plane records of a serving certificate it issued.
type issuedCertificate struct {
	event   string
	expires time.Time
}

// issued reads a certificate's serial, validity and renewal time before its gateway is created, so no failure to read
// it comes after the CA exists.
func issued(loc string, tls gateway.TLS) (issuedCertificate, error) {
	leaf, err := tls.Leaf()
	if err != nil {
		return issuedCertificate{}, err
	}
	expires := leaf.NotAfter.UTC()
	event := fmt.Sprintf("issued the certificate of the gateway in %s at %s: serial %s, valid from %s until %s, renewed by replacing the gateway from %s",
		loc, GatewayAddresses[loc], leaf.SerialNumber.Text(16), stampOf(leaf.NotBefore), stampOf(expires), stampOf(renewalPoint(expires)))
	return issuedCertificate{event: event, expires: expires}, nil
}

func (c issuedCertificate) expiresLabel() string { return strconv.FormatInt(c.expires.Unix(), 10) }

func renewalPoint(expires time.Time) time.Time { return expires.Add(-GatewayCertRenewBefore) }

// certificateExpiry is when gw's serving certificate lapses, as its label names it. A gateway built before the label is
// judged from its creation, seconds after its certificate was issued for gatewayCertValidity.
func certificateExpiry(gw Server) time.Time {
	unix, err := strconv.ParseInt(gw.Labels[KeyGatewayCertExpires], 10, 64)
	if err != nil {
		return gw.Created.Add(gatewayCertValidity)
	}
	return time.Unix(unix, 0).UTC()
}

// dueForRenewal is the first gateway, in location order, whose certificate has reached its renewal point.
func (p Plane) dueForRenewal(st State) (Server, bool) {
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if ok && !p.now().Before(renewalPoint(certificateExpiry(gw))) {
			return gw, true
		}
	}
	return Server{}, false
}

// renewStep replaces the first gateway whose certificate reached its renewal point, as an upgrade replaces one built
// from an earlier list, so the location's next gateway takes its address with a certificate the kept CA signs. It waits
// out an upgrade, whose own replacements each carry a new certificate.
func (p Plane) renewStep(ctx context.Context, st State, s site) (Step, bool, error) {
	upgrade, err := p.replacement(st, s.network)
	if err != nil || upgrade.active {
		return Step{}, false, err
	}
	gw, due := p.dueForRenewal(st)
	if !due {
		return Step{}, false, nil
	}
	lapses := "whose certificate lapses at " + stampOf(certificateExpiry(gw))
	unbuildable, err := p.noReplacementBecause(ctx)
	if err != nil {
		return Step{}, false, err
	}
	if unbuildable != "" {
		p.notify(ctx, "the renewal keeps gateway %d in %s, %s, since %s", gw.ID, gw.Location, lapses, unbuildable)
		return Step{}, false, nil
	}
	return p.replaceGateway(ctx, st, s, removal{gw: gw, plan: renewalPlan, why: lapses, keeper: "the renewal"})
}

// noReplacementBecause names what would leave a location without a gateway once its own is deleted, "" when nothing would.
func (p Plane) noReplacementBecause(ctx context.Context) (string, error) {
	if p.GatewayCA == nil {
		return "the block holds no gateway CA to sign a new one's certificate with", nil
	}
	_, why, err := p.newestImages(ctx, time.Time{})
	if err != nil || why == "" {
		return "", err
	}
	return "its replacement keeps the images the cluster still runs and " + why, nil
}
