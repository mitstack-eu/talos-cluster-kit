package robotcluster

// The gateway CA's key lives only in the block's own secret, so each request carries it back to sign a replacement.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/helpers/events"
	"github.com/mitstack-eu/block-kit/helpers/redundancy"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/redundant"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

// The keys of the block's own secrets that hold the gateway CA.
const (
	OwnGatewayCACert = "gatewayCaCert"
	OwnGatewayCAKey  = "gatewayCaKey"
)

func gatewayCA(d contract.Desired) *gateway.CA {
	cert, key := d.OwnSecrets[OwnGatewayCACert], d.OwnSecrets[OwnGatewayCAKey]
	if cert == "" || key == "" {
		return nil
	}
	return &gateway.CA{Cert: []byte(cert), Key: []byte(key)}
}

func ownSecrets(ca gateway.CA) map[string]string {
	return map[string]string{OwnGatewayCACert: string(ca.Cert), OwnGatewayCAKey: string(ca.Key)}
}

// WithGateways lets the plane sign with the kept CA, read node reports and replace gateways on upgrade alone.
func (k *Cluster) WithGateways(ctx context.Context, p controlplane.Plane, d contract.Desired) controlplane.Plane {
	return k.withReports(ctx, p, d, k.reportsOf(d))
}

// withReports is WithGateways reading the node reports through reports, which the caller reads too.
func (k *Cluster) withReports(ctx context.Context, p controlplane.Plane, d contract.Desired, reports *GatewayReports) controlplane.Plane {
	env, _ := events.EnvelopeFrom(ctx)
	p.GatewayCA, p.Upgrading = gatewayCA(d), env.Operation == OperationUpgrade
	p.ReadGateway, p.ReadReport = reports.ReadGateway, reports.ReadReport
	p.Guard = GuardRemoval
	p.Notify = func(ctx context.Context, detail string) { k.narrate(ctx, d, detail) }
	return p
}

// GuardRemoval judges the kit's removal through block-kit's redundancy guard, which the kit's root module cannot import.
func GuardRemoval(set redundant.Set, member string, stepStart time.Time) error {
	minimum := map[redundant.Minimum]redundancy.Minimum{
		redundant.Majority: redundancy.Majority(), redundant.AllButOne: redundancy.AllButOne(), redundant.NoRedundancy: redundancy.NoRedundancy(),
	}[set.Minimum]
	judged := redundancy.Set{Name: set.Name, Minimum: minimum}
	for _, m := range set.Members {
		read := redundancy.Member{Name: m.Name}
		if m.Reading != nil {
			read.Reading = &redundancy.Reading{At: m.Reading.At, Serving: m.Reading.Serving}
		}
		judged.Members = append(judged.Members, read)
	}
	return judged.AllowRemoval(member, stepStart)
}

func (k *Cluster) narrate(ctx context.Context, d contract.Desired, detail string) {
	if err := k.cfg.EmitProgress(ctx, detail); err != nil {
		log.Printf("%s: subscription %s: %s, which the callback did not record: %v", k.cfg.Block, d.SubscriptionID, detail, err)
		return
	}
	log.Printf("%s: subscription %s: %s", k.cfg.Block, d.SubscriptionID, detail)
}

// GatewayReports reads the subscription's node reports once per call, however many gateways the call reads.
type GatewayReports struct {
	load    func(ctx context.Context) ([]observe.NodeReport, error)
	once    sync.Once
	reports []observe.NodeReport
	err     error
}

// NewGatewayReports reads the reports through load, on the first read alone.
func NewGatewayReports(load func(ctx context.Context) ([]observe.NodeReport, error)) *GatewayReports {
	return &GatewayReports{load: load}
}

func (k *Cluster) reportsOf(d contract.Desired) *GatewayReports {
	return NewGatewayReports(func(ctx context.Context) ([]observe.NodeReport, error) {
		return k.cfg.NodeReports(ctx, d, d.SubscriptionID)
	})
}

func (g *GatewayReports) loaded(ctx context.Context) {
	g.once.Do(func() { g.reports, g.err = g.load(ctx) })
}

// ReadReport answers with the newest report received after since, read at its madeAt, else when mitstack received it.
// A failed read of the reports fails the call, since no report would read as the cluster sending none.
func (g *GatewayReports) ReadReport(ctx context.Context, since time.Time) (*report.Read, error) {
	g.loaded(ctx)
	if g.err != nil {
		return nil, fmt.Errorf("the node reports cannot be read: %w", g.err)
	}
	newest := operator.NewestReport(g.reports, since)
	if newest == nil {
		return nil, nil
	}
	read := readOf(*newest)
	return &read, nil
}

func (g *GatewayReports) readAll(ctx context.Context) ([]report.Read, error) {
	g.loaded(ctx)
	if g.err != nil {
		return nil, fmt.Errorf("the node reports cannot be read: %w", g.err)
	}
	reads := make([]report.Read, 0, len(g.reports))
	for _, r := range g.reports {
		reads = append(reads, readOf(r))
	}
	return reads, nil
}

func readOf(r observe.NodeReport) report.Read {
	read := report.Read{Body: r.Body, At: r.ReceivedAt}
	var body struct {
		MadeAt *time.Time `json:"madeAt"`
	}
	if json.Unmarshal(r.Body, &body) == nil && body.MadeAt != nil {
		read.At = body.MadeAt.UTC()
	}
	return read
}

// ReadGateway answers from the newest report received after since. A report that cannot be read, or names no gateway,
// reads nothing, so the route stays and an upgrade waits on it.
func (g *GatewayReports) ReadGateway(ctx context.Context, address string, since time.Time) (controlplane.GatewayReading, error) {
	g.loaded(ctx)
	if g.err != nil {
		return controlplane.GatewayReading{Detail: "the node reports cannot be read: " + g.err.Error()}, nil
	}
	newest := operator.NewestReport(g.reports, since)
	if newest == nil {
		return controlplane.GatewayReading{Detail: "the cluster has sent no node report since " + since.UTC().Format(time.RFC3339)}, nil
	}
	at := newest.ReceivedAt.UTC().Format(time.RFC3339)
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return controlplane.GatewayReading{}, fmt.Errorf("the gateway's address %q: %w", address, err)
	}
	readAt := report.GatewayReadAt(newest.Body, addr, newest.ReceivedAt)
	switch named, passed, reason := report.Gateway(newest.Body, addr, since); {
	case !named && reason != "":
		return controlplane.GatewayReading{Detail: fmt.Sprintf("the node report received at %s: %s", at, reason)}, nil
	case !named:
		return controlplane.GatewayReading{Detail: fmt.Sprintf("the node report received at %s names no gateway at %s", at, address)}, nil
	case passed:
		return controlplane.GatewayReading{Read: true, Serving: true, At: readAt, Detail: "the node report received at " + at + " reads its mirror answering"}, nil
	default:
		return controlplane.GatewayReading{Read: true, At: readAt, Detail: fmt.Sprintf("the node report received at %s reads its mirror not answering: %s", at, reason)}, nil
	}
}
