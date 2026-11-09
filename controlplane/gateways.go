package controlplane

// A gateway is immutable: a new list, a lost gateway or a failed one is answered by a new server at the same address,
// never by a change to a running one, and the network's default route moves to another gateway while one is down.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// The causes a cluster reads unhealthy with, word for word as the catalogue type lists them under failures.
const (
	CauseSingleGateway      = "the cluster was built by an older release that cannot be upgraded in place; rebuild the cluster"
	CauseGatewayDown        = "a gateway the block powered back on after an outage does not serve, so the block replaces it from the same list once a node report reads the other gateways serving; read its console log for why it failed"
	CauseGatewayOverdue     = "a gateway runs past its deadline and the newest node report since its creation does not read it serving, so the block replaces it from the same list once a node report reads the other gateways serving; read its console log for why it failed"
	CauseGatewayFailedAgain = "a gateway the block built from the same list to replace one that failed has failed too, so the block leaves it; read its console log, and upgrade to a release with a new list to replace it"
	CauseEarlierGatewayDown = "a server the block powered back on after an outage does not serve; read its console log and replace it with an upgrade"
)

// KeyGatewayUpgrade on the network names the list an upgrade replaces the gateways with, so a re-drive carries the
// replacement on and a lost gateway's rebuild starts none.
const KeyGatewayUpgrade = "mitstack.dev/gateway-upgrade"

// KeyGatewayFailed on the network, followed by a location, names the failed gateway the plane deleted there until the
// location has its successor, which carries it as KeyGatewayReplaces, so a second failure there reads as a repeat.
const (
	KeyGatewayFailed   = "mitstack.dev/gateway-failed-"
	KeyGatewayReplaces = "mitstack.dev/gateway-replaces"
)

// GatewayUpgradeBound is the longest an upgrade takes to replace every gateway in turn when none fails, which a block's
// settle budget must cover: per location, two report intervals for the others' reading, one for a report since the
// loss, and the replacement's deadline, by which a report reads it serving.
var GatewayUpgradeBound = time.Duration(len(Locations)) * (gateway.Deadline + 3*report.Interval)

// GatewayReading is what the newest node report received since a given time read of one gateway's mirror.
type GatewayReading struct {
	// Read is false when no report since then reads the gateway at all.
	Read, Serving bool
	// At is when the reading was taken, as report.GatewayReadAt gives it, and no removal rests on a reading without one.
	At time.Time
	// Detail names the report's receivedAt and, when the gateway does not serve, why.
	Detail string
}

func (p Plane) reading(ctx context.Context, gw Server, since time.Time) (GatewayReading, error) {
	if p.ReadGateway == nil {
		return GatewayReading{Detail: "the block reads no node report"}, nil
	}
	return p.ReadGateway(ctx, gatewayAddressOf(gw), since)
}

func gatewayAddressOf(gw Server) string { return GatewayAddresses[gw.Location] }

// gatewayComponents is what every gateway mirrors and serves, which holds none of the cluster's keys, so a
// replacement renders it without them.
func (p Plane) gatewayComponents() (components.Set, []components.Image, error) {
	set := components.Default()
	if p.Hosting != nil {
		set = components.HostingServed()
	}
	tools, err := components.ParseImage(toolsImage())
	if err != nil {
		return components.Set{}, nil, fmt.Errorf("the tools image: %w", err)
	}
	return set, []components.Image{tools}, nil
}

// gatewayLists keeps each gateway list by its set and tools image, since reading the images out of every manifest takes
// the better part of a second and one call reads the list at several steps.
var gatewayLists sync.Map

func (p Plane) gatewayList() (gateway.List, error) {
	set, extra, err := p.gatewayComponents()
	if err != nil {
		return gateway.List{}, err
	}
	key := fmt.Sprint(p.Hosting != nil, extra)
	if list, ok := gatewayLists.Load(key); ok {
		return list.(gateway.List), nil
	}
	list, err := gateway.NewList(set, extra...)
	if err != nil {
		return gateway.List{}, err
	}
	gatewayLists.Store(key, list)
	return list, nil
}

// zoneStep creates the gateway of a location that has none, once nodes run from a node report since the loss, attaches
// each gateway at its address, and takes the record of a loss off once the location has its gateway again.
func (p Plane) zoneStep(ctx context.Context, st State, s site) (Step, bool, error) {
	if st.SingleGateway() {
		return Step{}, false, nil
	}
	sibling, _ := st.SharedGateway()
	for _, loc := range Locations {
		if _, ok := st.GatewayIn(loc); !ok {
			if p.GatewayCA == nil {
				return Step{}, false, refuse("gateway-ca-unknown",
					"the gateway in %s is gone and the block holds no gateway CA to sign a new one's certificate with; unsubscribe and subscribe again to rebuild the cluster", loc)
			}
			if len(st.Servers[RoleNode]) > 0 {
				return p.rebuildStep(ctx, st, s, loc, sibling)
			}
			return Step{"create the gateway in " + loc + " from this release's list", func(ctx context.Context) error {
				return p.createGateway(ctx, st, s, loc, sibling, nil)
			}}, true, nil
		}
	}
	for _, loc := range Locations {
		gw, _ := st.GatewayIn(loc)
		if gw.PrivateIPs[s.network.ID] == "" {
			return Step{"attach the gateway in " + loc + " to the network at its fixed address before its first boot", func(ctx context.Context) error {
				return p.API.AttachServerToNetwork(ctx, gw.ID, s.network.ID, GatewayAddresses[loc])
			}}, true, nil
		}
	}
	for _, loc := range Locations {
		if _, recorded := gatewayGoneAt(s.network, loc); recorded {
			return p.setGatewayGone(s.network, loc, nil), true, nil
		}
	}
	return Step{}, false, nil
}

// neverStartedStep powers on the first gateway Hetzner has never started.
func (p Plane) neverStartedStep(ctx context.Context, st State) (Step, bool, error) {
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if !ok || gw.Status != ServerOff {
			continue
		}
		started, err := p.API.ServerStarted(ctx, gw.ID)
		if err != nil {
			return Step{}, false, err
		}
		if !started {
			return Step{"power on the gateway in " + loc, func(ctx context.Context) error { return p.API.PowerOnServer(ctx, gw.ID) }}, true, nil
		}
	}
	return Step{}, false, nil
}

// builtGatewayStep keeps the gateways of a cluster whose nodes exist, building a missing location's gateway, moving the
// route and powering a stopped gateway back on before it replaces one, since a replacement takes a location's gateway
// away and waits while any location lacks one.
func (p Plane) builtGatewayStep(ctx context.Context, st State, s site) (Step, bool, error) {
	if st.SingleGateway() {
		return p.singleGatewayStep(ctx, st, s)
	}
	for _, next := range []func(context.Context, State, site) (Step, bool, error){
		p.keptListStep,
		p.upgradeMarkStep,
		p.zoneStep,
		func(ctx context.Context, st State, _ site) (Step, bool, error) { return p.neverStartedStep(ctx, st) },
		p.routeStep,
		p.powerBackStep,
		p.failedStep,
		func(ctx context.Context, st State, s site) (Step, bool, error) {
			if st.lacksGateway() {
				return Step{}, false, nil
			}
			return p.replaceStep(ctx, st, s)
		},
	} {
		if step, ok, err := next(ctx, st, s); err != nil || ok {
			return step, ok, err
		}
	}
	return Step{}, false, nil
}

// singleGatewayStep keeps a cluster built before a gateway ran in each location as it stands and refuses the upgrade
// that would replace its gateway, since no kept key can sign a gateway its nodes trust.
func (p Plane) singleGatewayStep(ctx context.Context, st State, s site) (Step, bool, error) {
	if p.Upgrading {
		return Step{}, false, refuse("gateway-predates-zones",
			"the cluster's one gateway predates per-zone gateways and its CA key was never kept, so the upgrade cannot replace it; unsubscribe and subscribe again to rebuild the cluster")
	}
	return p.routeStep(ctx, st, s)
}

func runsAtItsAddress(gw Server, network Network) bool {
	return gw.Status == ServerRunning && gw.PrivateIPs[network.ID] == gatewayAddressOf(gw)
}

// serving reports whether a gateway runs at its address and a report since its creation reads its mirror answering,
// so one no report has read does not serve, as waitingOn judges it.
func (p Plane) serving(ctx context.Context, gw Server, network Network) (bool, error) {
	if !runsAtItsAddress(gw, network) {
		return false, nil
	}
	r, err := p.reading(ctx, gw, gw.Created)
	return r.Read && r.Serving, err
}

// readFailing reports whether a gateway is off its address or a report since its creation reads its mirror failing,
// which alone makes its route a problem, since a cluster sends its first report once it is built.
func (p Plane) readFailing(ctx context.Context, gw Server, network Network) (bool, error) {
	if !runsAtItsAddress(gw, network) {
		return true, nil
	}
	r, err := p.reading(ctx, gw, gw.Created)
	return r.Read && !r.Serving, err
}

func defaultRouteOf(n Network) (Route, bool) {
	for _, r := range n.Routes {
		if r.Destination == defaultRoute {
			return r, true
		}
	}
	return Route{}, false
}

func (st State) gatewayAt(address string) (Server, bool) {
	for loc, a := range GatewayAddresses {
		if a == address {
			return st.GatewayIn(loc)
		}
	}
	return Server{}, false
}

// routeTarget is the first gateway, in location order and other than skip, that serves.
func (p Plane) routeTarget(ctx context.Context, st State, network Network, skip string) (Server, bool, error) {
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if !ok || GatewayAddresses[loc] == skip {
			continue
		}
		up, err := p.serving(ctx, gw, network)
		if err != nil || up {
			return gw, up, err
		}
	}
	return Server{}, false, nil
}

// routeStep adds the default route when it is missing and moves it off a gateway that does not serve.
func (p Plane) routeStep(ctx context.Context, st State, s site) (Step, bool, error) {
	route, has := defaultRouteOf(s.network)
	if has {
		if gw, ok := st.gatewayAt(route.Gateway); ok {
			up, err := p.serving(ctx, gw, s.network)
			if err != nil || up {
				return Step{}, false, err
			}
		}
	}
	target, ok, err := p.routeTarget(ctx, st, s.network, route.Gateway)
	if err != nil || !ok {
		return Step{}, false, err
	}
	if !has {
		return Step{"add the network's default route via the gateway in " + target.Location, func(ctx context.Context) error {
			return p.addRoute(ctx, s.network, target, "the network had none")
		}}, true, nil
	}
	return p.moveRoute(s.network, route, target, "the gateway at "+route.Gateway+" does not serve"), true, nil
}

// moveRoute deletes the route before it adds the new one, since Hetzner refuses a second route to the same destination;
// when the add fails, the next call finds no route and adds it.
func (p Plane) moveRoute(network Network, from Route, to Server, why string) Step {
	return Step{"move the network's default route from " + from.Gateway + " to the gateway in " + to.Location, func(ctx context.Context) error {
		if err := p.API.DeleteRoute(ctx, network.ID, from); err != nil {
			return fmt.Errorf("deleting the default route via %s: %w", from.Gateway, err)
		}
		if err := p.addRoute(ctx, network, to, why); err != nil {
			return fmt.Errorf("the default route via %s is deleted and the network has none until the next call adds it: %w", from.Gateway, err)
		}
		return nil
	}}
}

func (p Plane) addRoute(ctx context.Context, network Network, to Server, why string) error {
	address := gatewayAddressOf(to)
	if err := p.API.AddRoute(ctx, network.ID, Route{Destination: defaultRoute, Gateway: address}); err != nil {
		return fmt.Errorf("adding the default route via %s: %w", address, err)
	}
	p.notify(ctx, "routed the network's default route via gateway %d in %s at %s, since %s", to.ID, to.Location, address, why)
	return nil
}

func poweredOnAt(gw Server) (time.Time, bool) {
	unix, err := strconv.ParseInt(gw.Labels[KeyPoweredOnAt], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unix, 0).UTC(), true
}

// downAfterPowerOn reports a gateway powered back on that has not served since: it is off again, or a report since
// reads its mirror failing. Its reading says why.
func (p Plane) downAfterPowerOn(ctx context.Context, gw Server) (bool, GatewayReading, error) {
	at, ok := poweredOnAt(gw)
	if !ok {
		return false, GatewayReading{}, nil
	}
	r, err := p.reading(ctx, gw, at)
	if err != nil || r.Serving {
		return false, r, err
	}
	return gw.Status != ServerRunning || r.Read, r, nil
}

// powerBackStep powers on a gateway that ran and is off, once an outage: a gateway already powered back on is powered
// on again only when a report read it serving after that.
func (p Plane) powerBackStep(ctx context.Context, st State, _ site) (Step, bool, error) {
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if !ok || gw.Status != ServerOff {
			continue
		}
		down, _, err := p.downAfterPowerOn(ctx, gw)
		if err != nil {
			return Step{}, false, err
		}
		if down {
			continue
		}
		return Step{"power the gateway in " + loc + " back on", func(ctx context.Context) error { return p.powerBack(ctx, gw) }}, true, nil
	}
	return Step{}, false, nil
}

func (p Plane) powerBack(ctx context.Context, gw Server) error {
	command, at, err := p.API.LastPowerAction(ctx, gw.ID)
	if err != nil {
		return err
	}
	why := "it powered itself off, and Hetzner records no power action on it"
	switch command {
	case StopServerCommand, ShutdownServerCommand:
		why = fmt.Sprintf("Hetzner ran %s on it at %s", command, at.UTC().Format(time.RFC3339))
	case "":
	default:
		why = fmt.Sprintf("it powered itself off after Hetzner's %s at %s", command, at.UTC().Format(time.RFC3339))
	}
	labels := maps.Clone(gw.Labels)
	labels[KeyPoweredOnAt] = strconv.FormatInt(p.now().Unix(), 10)
	if err := p.API.SetServerLabels(ctx, gw.ID, labels); err != nil {
		return err
	}
	if err := p.API.PowerOnServer(ctx, gw.ID); err != nil {
		return err
	}
	p.notify(ctx, "powered gateway %d in %s back on, which was off since %s", gw.ID, gw.Location, why)
	return nil
}

type gatewayFailure struct {
	cause, what, reading string
}

// failure judges a gateway failed once it is down after the plane's one power-on, or, never powered back on, once it
// runs past its deadline and the newest report since its creation does not read it serving; else its cause is empty.
func (p Plane) failure(ctx context.Context, gw Server, network Network) (gatewayFailure, error) {
	if at, powered := poweredOnAt(gw); powered {
		down, r, err := p.downAfterPowerOn(ctx, gw)
		if err != nil || !down {
			return gatewayFailure{}, err
		}
		return gatewayFailure{CauseGatewayDown, fmt.Sprintf("was powered back on at %s and is %s", at.Format(time.RFC3339), gw.Status), r.Detail}, nil
	}
	if gw.Status != ServerRunning || !p.pastDeadline(gw) {
		return gatewayFailure{}, nil
	}
	r, err := p.reading(ctx, gw, gw.Created)
	if err != nil || r.Read && r.Serving && runsAtItsAddress(gw, network) {
		return gatewayFailure{}, err
	}
	return gatewayFailure{CauseGatewayOverdue, missedDeadline(gw), r.Detail}, nil
}

// failedStep replaces the first gateway of this release's list that failed, from the same list at its address, unless
// it replaced one that failed, and ends the replacement of one a report reads serving again.
func (p Plane) failedStep(ctx context.Context, st State, s site) (Step, bool, error) {
	if st.lacksGateway() {
		return Step{}, false, nil
	}
	list, err := p.gatewayList()
	if err != nil {
		return Step{}, false, err
	}
	for _, loc := range Locations {
		gw, _ := st.GatewayIn(loc)
		if gw.Labels[KeyGatewayList] != list.Digest() {
			continue
		}
		f, err := p.failure(ctx, gw, s.network)
		switch {
		case err != nil:
			return Step{}, false, err
		case f.cause == "":
			if step, ok, err := p.endReplacementStep(ctx, gw, s.network, list.Digest()); err != nil || ok {
				return step, ok, err
			}
		case gw.Labels[KeyGatewayReplaces] == "":
			return p.replaceGateway(ctx, st, s, removal{gw: gw, plan: list.Digest(), why: "which " + f.what, keeper: "the block",
				record: func(ctx context.Context) error { return p.recordFailed(ctx, s.network, gw) }})
		}
	}
	return Step{}, false, nil
}

// endReplacementStep ends the replacement of a gateway read serving again, so a later failure is judged afresh.
func (p Plane) endReplacementStep(ctx context.Context, gw Server, network Network, plan string) (Step, bool, error) {
	if _, began := RemovalBegan(gw, plan); !began {
		return Step{}, false, nil
	}
	up, err := p.serving(ctx, gw, network)
	if err != nil || !up {
		return Step{}, false, err
	}
	return Step{"end the replacement of the gateway in " + gw.Location + ", which a node report reads serving again", func(ctx context.Context) error {
		return p.API.SetServerLabels(ctx, gw.ID, EndRemoval(gw))
	}}, true, nil
}

func (p Plane) recordFailed(ctx context.Context, network Network, gw Server) error {
	labels := maps.Clone(network.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[KeyGatewayFailed+gw.Location] = strconv.FormatInt(gw.ID, 10)
	if err := p.API.SetNetworkLabels(ctx, network.ID, labels); err != nil {
		return fmt.Errorf("recording the failed gateway %d in %s on network %d: %w", gw.ID, gw.Location, network.ID, err)
	}
	return nil
}

// replacement is where an upgrade stands: the gateways built from an earlier list, and whether one is to replace them.
type replacement struct {
	digest  string
	stale   []Server
	current []Server
	active  bool
}

func (p Plane) replacement(st State, network Network) (replacement, error) {
	list, err := p.gatewayList()
	if err != nil {
		return replacement{}, err
	}
	digest := list.Digest()
	r := replacement{digest: digest}
	for _, loc := range Locations {
		if gw, ok := st.GatewayIn(loc); ok && gw.Labels[KeyGatewayList] == digest {
			r.current = append(r.current, gw)
		} else if ok {
			r.stale = append(r.stale, gw)
		}
	}
	// An upgrade with no gateway to replace waits on none, so a cluster whose report names no gateway still settles.
	r.active = network.Labels[KeyGatewayUpgrade] == digest || p.Upgrading && len(r.stale) > 0
	return r, nil
}

// waitingOn is the first gateway built from this release's list that does not run at its address, or that no report
// has read serving since its creation. Its reading names the deadline of one running past it.
func (p Plane) waitingOn(ctx context.Context, r replacement, network Network) (Server, GatewayReading, bool, error) {
	for _, gw := range r.current {
		if gw.Status != ServerRunning || gw.PrivateIPs[network.ID] != gatewayAddressOf(gw) {
			return gw, GatewayReading{Detail: fmt.Sprintf("it is %s and not yet running at %s", gw.Status, gatewayAddressOf(gw))}, true, nil
		}
		reading, err := p.reading(ctx, gw, gw.Created)
		if err != nil {
			return Server{}, GatewayReading{}, false, err
		}
		if !reading.Serving {
			if p.pastDeadline(gw) {
				reading.Detail += "; " + overdueDetail(gw)
			}
			return gw, reading, true, nil
		}
	}
	return Server{}, GatewayReading{}, false, nil
}

func gatewayDeadline(gw Server) time.Time { return gw.Created.Add(gateway.Deadline) }

func (p Plane) pastDeadline(gw Server) bool { return !p.now().Before(gatewayDeadline(gw)) }

func overdueDetail(gw Server) string { return gatewayName(gw) + " " + missedDeadline(gw) }

func missedDeadline(gw Server) string {
	return fmt.Sprintf("runs past its deadline at %s, %s after its creation, and the newest node report since its creation does not read it serving",
		gatewayDeadline(gw).UTC().Format(time.RFC3339), gateway.Deadline)
}

// upgradeMarkStep marks the network on the upgrade's own call, before any other step, since the re-drives that carry
// the replacement on are no upgrade.
func (p Plane) upgradeMarkStep(_ context.Context, st State, s site) (Step, bool, error) {
	r, err := p.replacement(st, s.network)
	if err != nil || !p.Upgrading || len(r.stale) == 0 || s.network.Labels[KeyGatewayUpgrade] == r.digest {
		return Step{}, false, err
	}
	return p.markStep(s.network, r.digest), true, nil
}

// nextToReplace is the gateway built from an earlier list that an upgrade replaces next: the first in location order
// that does not serve, since replacing it takes nothing from the gateways that do, else the first.
func (p Plane) nextToReplace(ctx context.Context, r replacement, network Network) (Server, error) {
	for _, gw := range r.stale {
		up, err := p.serving(ctx, gw, network)
		if err != nil || !up {
			return gw, err
		}
	}
	return r.stale[0], nil
}

// replaceStep replaces a gateway built from an earlier list once every gateway built from this one serves and the
// plane's redundancy guard finds every other gateway read serving since its replacement began, moving the route first.
func (p Plane) replaceStep(ctx context.Context, st State, s site) (Step, bool, error) {
	r, err := p.replacement(st, s.network)
	if err != nil || !r.active {
		return Step{}, false, err
	}
	marked := s.network.Labels[KeyGatewayUpgrade] == r.digest
	if _, _, waiting, err := p.waitingOn(ctx, r, s.network); err != nil || waiting {
		return Step{}, false, err
	}
	if len(r.stale) == 0 {
		if !marked {
			return Step{}, false, nil
		}
		return p.markStep(s.network, ""), true, nil
	}
	old, err := p.nextToReplace(ctx, r, s.network)
	if err != nil {
		return Step{}, false, err
	}
	return p.replaceGateway(ctx, st, s, removal{gw: old, plan: r.digest, why: "built from an earlier list", keeper: "the upgrade"})
}

// removal is a gateway replaced for plan, why it goes and who keeps it, as its events say, and what to record first.
type removal struct {
	gw        Server
	plan, why string
	keeper    string
	record    func(ctx context.Context) error
}

// replaceGateway moves the route off a gateway, then deletes it once the guard reads the others serving since it began.
func (p Plane) replaceGateway(ctx context.Context, st State, s site, r removal) (Step, bool, error) {
	old := r.gw
	if route, ok := defaultRouteOf(s.network); ok && route.Gateway == gatewayAddressOf(old) {
		target, ok, err := p.routeTarget(ctx, st, s.network, route.Gateway)
		if err != nil {
			return Step{}, false, err
		}
		// With no gateway serving to take the route, the guard below holds the replacement and names why.
		if ok {
			return p.moveRoute(s.network, route, target, "the gateway in "+old.Location+" is to be replaced"), true, nil
		}
	}
	began, ok := RemovalBegan(old, r.plan)
	if !ok {
		return Step{"begin replacing the gateway in " + old.Location + ", so only a node report read after now judges the others", func(ctx context.Context) error {
			return p.API.SetServerLabels(ctx, old.ID, BeginRemoval(old, p.now(), r.plan))
		}}, true, nil
	}
	set, details, err := p.gatewaySet(ctx, st, s.network, began)
	if err != nil {
		return Step{}, false, err
	}
	if err := p.Guard.Allow(set, gatewayName(old), began); err != nil {
		if !redundant.Held(err) {
			return Step{}, false, err
		}
		p.notify(ctx, "%s keeps gateway %d in %s until the others serve: %s", r.keeper, old.ID, old.Location, err)
		return Step{}, false, nil
	}
	return p.deletionStep(r, began, describeServing(set, details, gatewayName(old))), true, nil
}

func (p Plane) deletionStep(r removal, began time.Time, judged string) Step {
	old := r.gw
	return Step{"delete the gateway in " + old.Location + ", " + r.why, func(ctx context.Context) error {
		if r.record != nil {
			if err := r.record(ctx); err != nil {
				return err
			}
		}
		if err := p.API.DeleteServer(ctx, old.ID); err != nil {
			return fmt.Errorf("deleting the gateway %d in %s: %w", old.ID, old.Location, err)
		}
		p.notify(ctx, "deleted gateway %d in %s, %s, so one built from this release's list takes %s, judged by readings since %s: %s",
			old.ID, old.Location, r.why, gatewayAddressOf(old), began.Format(time.RFC3339), judged)
		return nil
	}}
}

func gatewayName(gw Server) string { return fmt.Sprintf("gateway %d in %s", gw.ID, gw.Location) }

// gatewaySet is every gateway with its newest reading since began, off its address as Hetzner reads it now, else as a
// report since began reads its mirror, with each reading's detail naming that report.
func (p Plane) gatewaySet(ctx context.Context, st State, network Network, began time.Time) (redundant.Set, map[string]string, error) {
	set, details := redundant.Set{Name: "gateways", Minimum: redundant.AllButOne}, map[string]string{}
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if !ok {
			continue
		}
		member := redundant.Member{Name: gatewayName(gw)}
		if !runsAtItsAddress(gw, network) {
			member.Reading = &redundant.Reading{At: p.now(), Serving: false}
		} else if r, err := p.reading(ctx, gw, began); err != nil {
			return redundant.Set{}, nil, err
		} else if r.Read {
			member.Reading, details[member.Name] = &redundant.Reading{At: r.At, Serving: r.Serving}, r.Detail
		}
		set.Members = append(set.Members, member)
	}
	return set, details, nil
}

// describeServing names each member but removed with when and from which report the guard read it serving.
func describeServing(set redundant.Set, details map[string]string, removed string) string {
	var parts []string
	for _, m := range set.Members {
		if m.Name != removed && m.Reading != nil {
			parts = append(parts, fmt.Sprintf("%q read serving at %s (%s)", m.Name, m.Reading.At.UTC().Format(time.RFC3339), details[m.Name]))
		}
	}
	return strings.Join(parts, "; ")
}

// markStep sets the network's upgrade mark to digest, or takes it off when digest is empty, once every gateway is built
// from this release's list and serves.
func (p Plane) markStep(network Network, digest string) Step {
	what := "mark the network as replacing its gateways with this release's list"
	if digest == "" {
		what = "take the upgrade's mark off the network, since every gateway is built from this release's list and serves"
	}
	return Step{what, func(ctx context.Context) error {
		labels := maps.Clone(network.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		labels[KeyGatewayUpgrade] = digest
		if digest == "" {
			delete(labels, KeyGatewayUpgrade)
		}
		return p.API.SetNetworkLabels(ctx, network.ID, labels)
	}}
}

// createGateway builds a location's gateway stopped and off the network, with a certificate the kept CA signs for its
// address, mirroring what k keeps beside the release's list, and with no first-node bootstrap, since only the install's
// first gateway bootstraps etcd.
func (p Plane) createGateway(ctx context.Context, st State, s site, loc string, sibling Server, k *keeping) error {
	ingress, _ := st.LoadBalancerIn(RoleIngress, GatewayLocation)
	ingressAddr, err := netip.ParseAddr(ingress.PublicIPv4)
	if err != nil {
		return fmt.Errorf("the ingress load balancer has no public address: %w", err)
	}
	list, err := p.gatewayList()
	if err != nil {
		return err
	}
	if k != nil {
		list = list.Keeping(k.kept)
	}
	userData, tls, err := p.renderGateway(loc, ingressAddr, p.now(), *p.GatewayCA, list, nil)
	if err != nil {
		return err
	}
	event, err := certificateEvent(loc, tls)
	if err != nil {
		return err
	}
	spec, err := p.gatewaySpec(ctx, &OrderableTypes{API: p.API}, loc, userData, s, sibling.Labels[KeyEnrolmentSHA256], sibling.Labels[KeyIssuerBucket], list.Digest())
	if err != nil {
		return err
	}
	if _, err := p.API.CreateServers(ctx, spec); err != nil {
		return errors.Join(fmt.Errorf("creating the gateway in %s: %w", loc, err), p.deleteUnattachedGateway(ctx, loc, s))
	}
	p.notify(ctx, "%s", event)
	if k != nil {
		p.notify(ctx, "%s", k.event(loc, list))
	}
	return nil
}

// deleteUnattachedGateway deletes a gateway whose create failed after Hetzner made it, so the next call creates it anew.
func (p Plane) deleteUnattachedGateway(ctx context.Context, loc string, s site) error {
	servers, err := p.API.ListServers(ctx, p.RoleSelector(RoleGateway))
	if err != nil {
		return err
	}
	for _, gw := range ServersIn(servers, loc) {
		if gw.PrivateIPs[s.network.ID] == "" {
			if err := p.API.DeleteServer(ctx, gw.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p Plane) gatewaySpec(ctx context.Context, orderable *OrderableTypes, loc, userData string, s site, enrolment, bucket, digest string) (ServerSpec, error) {
	serverType, err := orderable.First(ctx, string(RoleGateway), GatewayServerTypes, loc)
	if err != nil {
		return ServerSpec{}, err
	}
	labels := p.Labels(RoleGateway)
	labels[KeyEnrolmentSHA256], labels[KeyIssuerBucket], labels[KeyGatewayList] = enrolment, bucket, digest
	if replaced := s.network.Labels[KeyGatewayFailed+loc]; replaced != "" {
		labels[KeyGatewayReplaces] = replaced
	}
	return ServerSpec{
		Name:        p.LocatedName(RoleGateway, loc),
		ServerType:  serverType,
		Location:    loc,
		Image:       gatewayImage,
		UserData:    userData,
		Labels:      labels,
		FirewallIDs: []int64{s.firewall.ID},
		PublicIPv4:  true,
		Stopped:     true,
	}, nil
}

// certificateEvent names a gateway certificate's serial and validity, read before the gateway is created, so no
// failure to read it comes after the CA exists.
func certificateEvent(loc string, tls gateway.TLS) (string, error) {
	leaf, err := tls.Leaf()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("issued the certificate of the gateway in %s at %s: serial %s, valid from %s until %s", loc, GatewayAddresses[loc],
		leaf.SerialNumber.Text(16), leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339)), nil
}

// Unhealthy names the failure a cluster meets while it still serves, and what was seen; both are empty when it meets none.
func (p Plane) Unhealthy(ctx context.Context, st State) (cause, detail string, err error) {
	if st.SingleGateway() {
		gw, _ := st.SharedGateway()
		return CauseSingleGateway, fmt.Sprintf("gateway %d in %s carries no %s label, so it was built before a gateway ran in each location, and the block holds no CA key to sign another gateway's certificate with",
			gw.ID, gw.Location, KeyGatewayList), nil
	}
	list, err := p.gatewayList()
	if err != nil {
		return "", "", err
	}
	network, _ := st.Network()
	for _, loc := range Locations {
		gw, ok := st.GatewayIn(loc)
		if !ok {
			continue
		}
		f, err := p.failure(ctx, gw, network)
		if err != nil {
			return "", "", err
		}
		if cause, detail := f.unhealthy(gw, list.Digest()); cause != "" {
			return cause, detail, nil
		}
	}
	return "", "", nil
}

// unhealthy names a failed gateway's cause, with how its replacement stands or why the plane leaves it.
func (f gatewayFailure) unhealthy(gw Server, digest string) (cause, detail string) {
	detail = fmt.Sprintf("%s %s; %s", gatewayName(gw), f.what, f.reading)
	switch {
	case f.cause == "":
		return "", ""
	case gw.Labels[KeyGatewayList] != digest && f.cause == CauseGatewayDown:
		return CauseEarlierGatewayDown, detail
	case gw.Labels[KeyGatewayList] != digest:
		return "", ""
	case gw.Labels[KeyGatewayReplaces] != "":
		return CauseGatewayFailedAgain, fmt.Sprintf("%s; it replaced gateway %s, which failed from the same list, so the block leaves it in place",
			detail, gw.Labels[KeyGatewayReplaces])
	}
	if began, ok := RemovalBegan(gw, digest); ok {
		return f.cause, fmt.Sprintf("%s; the block began replacing it at %s and deletes it once a node report received after then reads the other gateways serving",
			detail, began.Format(time.RFC3339))
	}
	return f.cause, detail + "; the block replaces it once a node report reads the other gateways serving"
}
