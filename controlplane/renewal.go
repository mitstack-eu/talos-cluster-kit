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

// certificateExpiry is when gw's serving certificate lapses, as its label names it; unlabelled, it is the latest that can
// be, the creation of a gateway built before the label plus the validity its certificate was issued for just before.
func certificateExpiry(gw Server) (expires time.Time, labelled bool) {
	unix, err := strconv.ParseInt(gw.Labels[KeyGatewayCertExpires], 10, 64)
	if err != nil {
		return gw.Created.Add(unlabelledCertValidity), false
	}
	return time.Unix(unix, 0).UTC(), true
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

// renewalOf is gw's replacement for a new certificate, which names the lapse of an unlabelled gateway as a bound.
func renewalOf(gw Server) removal {
	expires, labelled := certificateExpiry(gw)
	why := "whose certificate lapses at " + stampOf(expires)
	if !labelled {
		why = "whose certificate lapses by " + stampOf(expires) + " as judged from its creation"
	}
	return removal{gw: gw, plan: renewalPlan, why: why, keeper: "the renewal"}
}

// dueForRenewal leaves out a gateway failedStep replaces, whose replacement the renewal's plan would begin anew on
// every call.
func (p Plane) dueForRenewal(ctx context.Context, st State, network Network) ([]Server, error) {
	list, err := p.gatewayList()
	if err != nil {
		return nil, err
	}
	var due []Server
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if expires, _ := certificateExpiry(gw); !ok || p.now().Before(renewalPoint(expires)) {
			continue
		}
		failed, err := p.replacedAsFailed(ctx, gw, network, list.Digest())
		if err != nil {
			return nil, err
		}
		if !failed {
			due = append(due, gw)
		}
	}
	return due, nil
}

// renewStep hands a gateway whose certificate reached its renewal point to the replacement an upgrade runs, and waits
// out an upgrade, whose own replacements each carry a new certificate.
func (p Plane) renewStep(ctx context.Context, st State, s site) (Step, bool, error) {
	upgrade, err := p.replacement(st, s.network)
	if err != nil || upgrade.active {
		return Step{}, false, err
	}
	due, err := p.dueForRenewal(ctx, st, s.network)
	if err != nil || len(due) == 0 {
		return Step{}, false, err
	}
	held, err := p.renewalHeldBy(ctx)
	if err != nil {
		return Step{}, false, err
	}
	if held != "" {
		p.keep(ctx, renewalOf(due[0]), held)
		return Step{}, false, nil
	}
	ordered, err := p.notServingFirst(ctx, due, s.network)
	if err != nil {
		return Step{}, false, err
	}
	for _, gw := range ordered {
		step, ok, err := p.replaceGateway(ctx, st, s, renewalOf(gw))
		var refused *Refusal
		if !errors.As(err, &refused) {
			return step, ok, err
		}
		// A refusal would fail every call of a cluster that serves, and another location may still order its successor.
		p.keep(ctx, renewalOf(gw), refused.Message)
	}
	return Step{}, false, nil
}

func (p Plane) keep(ctx context.Context, r removal, why string) {
	p.notify(ctx, "%s keeps gateway %d in %s, %s, since %s", r.keeper, r.gw.ID, r.gw.Location, r.why, why)
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
