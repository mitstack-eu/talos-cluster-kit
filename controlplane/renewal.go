package controlplane

// A gateway's serving certificate is renewed by replacing the gateway, since Hetzner fixes a server's user data, and
// with it the certificate's key, when it creates the server.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
)

// GatewayCertRenewBefore is how long before a gateway's serving certificate lapses the plane begins replacing the
// gateway, which leaves a month of calls for the locations to take their turn.
const GatewayCertRenewBefore = 30 * 24 * time.Hour

// KeyGatewayCertExpires on a gateway is when its serving certificate lapses, in Unix seconds, since the plane keeps no
// state between calls and reads no event back.
const KeyGatewayCertExpires = "mitstack.dev/gateway-cert-expires"

// renewalPlan is the plan a gateway replaced for a new certificate carries as KeyRemovalFor.
const renewalPlan = "certificate-renewal"

// unlabelledCertValidity is the validity every release issued a certificate for before gateways carried
// KeyGatewayCertExpires, which a change of gatewayCertValidity must not move.
const unlabelledCertValidity = 365 * 24 * time.Hour

// issuedCertificate is what the plane records of a serving certificate it issued.
type issuedCertificate struct {
	event   string
	expires time.Time
}

// issued reads the certificate while its gateway is rendered, so a failure to read it comes before any server is created.
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

// certificateExpiry is when gw's serving certificate lapses, as its label names it, and a gateway built before the
// label is judged from its creation, seconds after its certificate was issued.
func certificateExpiry(gw Server) time.Time {
	unix, err := strconv.ParseInt(gw.Labels[KeyGatewayCertExpires], 10, 64)
	if err != nil {
		return gw.Created.Add(unlabelledCertValidity)
	}
	return time.Unix(unix, 0).UTC()
}

// expiryLabelProblem names a label that holds no time, which certificateExpiry reads as a gateway built before the label.
func expiryLabelProblem(gw Server) string {
	label, labelled := gw.Labels[KeyGatewayCertExpires]
	if _, err := strconv.ParseInt(label, 10, 64); !labelled || err == nil {
		return ""
	}
	return fmt.Sprintf("gateway %d's %s label is %q, which names no time in Unix seconds, so its certificate is judged from its creation",
		gw.ID, KeyGatewayCertExpires, label)
}

func (p Plane) dueForRenewal(st State) (Server, bool) {
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if ok && !p.now().Before(renewalPoint(certificateExpiry(gw))) {
			return gw, true
		}
	}
	return Server{}, false
}

// renewStep hands a gateway whose certificate reached its renewal point to the replacement an upgrade runs, and waits
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
	renewal := removal{gw: gw, plan: renewalPlan, why: "whose certificate lapses at " + stampOf(certificateExpiry(gw)), keeper: "the renewal"}
	held, err := p.renewalHeldBy(ctx)
	if err != nil {
		return Step{}, false, err
	}
	if held != "" {
		return p.keepGateway(ctx, renewal, held)
	}
	step, ok, err := p.replaceGateway(ctx, st, s, renewal)
	// A refusal would fail every call of a cluster that serves, so the renewal waits for a later call instead.
	var refused *Refusal
	if errors.As(err, &refused) {
		return p.keepGateway(ctx, renewal, refused.Message)
	}
	return step, ok, err
}

// keepGateway records why the renewal keeps a gateway, and takes no step.
func (p Plane) keepGateway(ctx context.Context, r removal, why string) (Step, bool, error) {
	p.notify(ctx, "%s keeps gateway %d in %s, %s, since %s", r.keeper, r.gw.ID, r.gw.Location, r.why, why)
	return Step{}, false, nil
}

// renewalHeldBy names what a renewal alone waits on, "" when nothing: an upgrade and a failed gateway's replacement
// refuse or fail on each of these, which a cluster that serves must not.
func (p Plane) renewalHeldBy(ctx context.Context) (string, error) {
	switch {
	case p.GatewayCA == nil:
		return "the block holds no gateway CA to sign a new one's certificate with", nil
	case p.Guard == nil:
		return "the block passes no redundancy guard, so no gateway is removed", nil
	}
	return p.keptImagesUnread(ctx)
}
