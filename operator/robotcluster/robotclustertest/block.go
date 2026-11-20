package robotclustertest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/block-kit/verify/kittest"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
)

// Block is what kittest.AssertBlock drives over an operator's handler config, whose Resource the fakes settle before
// each subscribe answers and whose observers read with the own secrets every answer minted, as the platform's re-drives
// and record do and the kit's harness does not.
func (e *Env) Block(cfg handler.Config) kittest.Block {
	e.T.Helper()
	res, ok := cfg.Resource.(handler.OwnSecretsReporter)
	if !ok {
		e.T.Fatalf("the config's Resource %T reports no own secrets, and a robot cluster keeps its gateway CA in them", cfg.Resource)
	}
	m, err := contract.ParseManifest(cfg.Manifest)
	if err != nil {
		e.T.Fatalf("reading the config's manifest: %v", err)
	}
	cfg.Resource = settling{Resource: cfg.Resource, owning: res, e: e}
	observers := make(map[string]handler.Observer, len(cfg.Observers))
	for name, o := range cfg.Observers {
		observers[name] = carrying{Observer: o, e: e}
	}
	cfg.Observers = observers
	d := e.Desired()
	d.BlockID, d.OrgID = m.Block.Metadata.ID, "org_0123456789ab"
	return kittest.Block{Config: cfg, Provider: provider{e}, Desired: d, Upgrade: &d}
}

type settling struct {
	handler.Resource
	owning handler.OwnSecretsReporter
	e      *Env
}

func (s settling) EnsureResourcesOwning(ctx context.Context, d contract.Desired) ([]contract.CreatedResource, map[string]string, error) {
	// Each round stands in for a call the platform makes afresh, and the Cloud moves between rounds with no write.
	ctx = controlplane.WithoutRequestReads(ctx)
	var last error
	for range rounds {
		d.OwnSecrets = merged(s.e.Own, d.OwnSecrets)
		created, own, err := s.owning.EnsureResourcesOwning(ctx, d)
		s.e.Own = merged(d.OwnSecrets, own)
		if err != nil && !Waiting(err) {
			return created, s.e.Own, err
		}
		s.e.Now = s.e.Now.Add(time.Minute)
		s.e.Progress()
		obs, oerr := s.e.Cluster.Observe(ctx, s.e.Desired())
		if err == nil && oerr == nil && obs.State == contract.Present {
			return created, s.e.Own, nil
		}
		last = errors.Join(err, oerr, fmt.Errorf("the cluster reads %s: %s", obs.State, obs.StatusDetail))
	}
	return nil, s.e.Own, fmt.Errorf("the cluster read present after none of %d subscribes: %w", rounds, last)
}

type carrying struct {
	handler.Observer
	e *Env
}

func (o carrying) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	r.Desired.OwnSecrets = merged(o.e.Own, r.Desired.OwnSecrets)
	return o.Observer.ObserveResource(ctx, r)
}

func merged(a, b map[string]string) map[string]string {
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b)
	return out
}

// provider stands in for the Hetzner and GitHub accounts the kit's guards list and lose resources through.
type provider struct{ e *Env }

func (p provider) ListTagged(ctx context.Context, _ string) ([]observe.Tagged, error) {
	st, err := p.e.Cluster.PlaneFor(p.e.Cloud, p.e.Desired()).Read(ctx)
	if err != nil {
		return nil, err
	}
	var tagged []observe.Tagged
	add := func(typ string, ids []int64) {
		for _, id := range ids {
			tagged = append(tagged, observe.Tagged{Type: typ, ID: strconv.FormatInt(id, 10)})
		}
	}
	add(robotcluster.ResourceNetwork, controlplane.IDs(st.Networks, func(n controlplane.Network) int64 { return n.ID }))
	add(robotcluster.ResourceFirewall, controlplane.IDs(st.Firewalls, func(f controlplane.Firewall) int64 { return f.ID }))
	for _, lbs := range st.LoadBalancers {
		add(robotcluster.TypeLoadBalancer, controlplane.IDs(lbs, controlplane.LoadBalancerID))
	}
	for _, servers := range st.Servers {
		add(robotcluster.TypeServer, controlplane.IDs(servers, controlplane.ServerID))
	}
	// The snapshot holding the cluster's keys is the cluster's own part, which its type's absence statement names.
	add(p.e.clusterType(), controlplane.IDs(st.Snapshots, func(s controlplane.Snapshot) int64 { return s.ID }))
	if p.e.GitHub.Exists(p.e.Repo()) {
		tagged = append(tagged, observe.Tagged{Type: robotcluster.TypeRepository, ID: p.e.Repo()},
			observe.Tagged{Type: robotcluster.ResourceSeedCommit, ID: p.e.Repo()})
	}
	return tagged, nil
}

func (p provider) Lose(ctx context.Context, r contract.CreatedResource) error {
	c := p.e.Cloud
	for key, del := range map[string]func(context.Context, int64) error{
		"networkId": c.DeleteNetwork, "firewallId": c.DeleteFirewall, "loadBalancerId": c.DeleteLoadBalancer, "serverId": c.DeleteServer,
	} {
		if raw, ok := r.Identity[key]; ok {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return fmt.Errorf("resource %s names %s %q, which is no Hetzner id", r.Name, key, raw)
			}
			return del(ctx, id)
		}
	}
	switch r.Type {
	case robotcluster.TypeRepository, robotcluster.ResourceSeedCommit:
		// The contents fake serves no delete, so the seed commit is lost with its repository.
		return p.e.Config.GitHubClient("", githubToken).DeleteRepo(ctx, Owner, App+"-cluster")
	case p.e.clusterType():
		snaps, err := c.ListSnapshots(ctx, p.e.Plane().Selector())
		if err == nil && len(snaps) == 0 {
			err = fmt.Errorf("resource %s has no snapshot under %s to lose", r.Name, p.e.Plane().Selector())
		}
		for _, s := range snaps {
			err = errors.Join(err, c.DeleteSnapshot(ctx, s.ID))
		}
		return err
	}
	return fmt.Errorf("resource %s is of type %q, which the provider cannot lose", r.Name, r.Type)
}

func (e *Env) clusterType() string {
	if e.Config.ClusterType == "" {
		return robotcluster.ResourceCluster
	}
	return e.Config.ClusterType
}
