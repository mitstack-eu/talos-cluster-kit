package robotcluster

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

// The entries an operator's manifest declares, by the names the cluster reads and reports them under.
const (
	ResourceNetwork             = "hetzner.network"
	ResourceFirewall            = "hetzner.firewall"
	ResourceAPILoadBalancer     = "api-load-balancer"
	ResourceIngressLoadBalancer = "ingress-load-balancer"
	ResourceRepository          = "github.repository"
	ResourceSeedCommit          = "github.gitops-commit"
	ResourceGateway             = "gateway"
	ResourceNode                = "node"
	ResourceCluster             = "hetzner.kubernetes-cluster"
	ResourceDedicatedServer     = "dedicated-server"
	ResourceVSwitch             = "vswitch"
	ResourceRobotFirewall       = "robot-firewall"
	TypeServer                  = "hetzner.server"
	TypeLoadBalancer            = "hetzner.load-balancer"
	TypeRepository              = "github.gitops-repository"
	TypeDedicatedServer         = "hetzner.dedicated-server"
	TypeVSwitch                 = "hetzner.vswitch"
	TypeRobotFirewall           = "hetzner.robot-firewall"
	ParamHostsPlatform          = "hostsPlatform"
	ParamRobotServer            = "robotServerNumber"
)

const (
	// OperationUpgrade is the operation an upgrade's requests carry, the one call on which gateways are replaced.
	OperationUpgrade = "upgrade"
	// EgressEnv names the operator's public IPv4 address, the one address the rescue session's firewall window admits.
	EgressEnv = "MITSTACK_EGRESS_IPV4"
)

type clusterParams struct {
	appName string
	// size is how many control-plane nodes the cluster runs.
	size int
	// serverTypes are the Cloud servers' types by preference, each server taking the first its location can order.
	serverTypes []string
	// cloudOnly is whether the cluster runs on Hetzner Cloud alone, with no robotServer.
	cloudOnly   bool
	robotServer int64
	// cloud is the Cloud pool of workers made from the worker image, empty when its inputs are left out.
	cloud pools.CloudPool
	// hosting is whether the cluster hosts the platform, and recipient the key its recovery files are encrypted to.
	hosting   bool
	recipient []byte
}

// HostsPlatform is whether the request asks for a cluster that hosts the platform.
func HostsPlatform(d contract.Desired) bool { return d.Params[ParamHostsPlatform] == "true" }

func (k *Cluster) readParams(d contract.Desired) (clusterParams, error) {
	appName, err := operator.AppName(d)
	p := clusterParams{appName: appName, cloudOnly: k.cloudOnly(d)}
	if err != nil {
		return p, err
	}
	if p.size, err = operator.ControlPlaneSize(d); err != nil {
		return p, err
	}
	if p.serverTypes, err = requestedServerTypes(d); err != nil {
		return p, err
	}
	if p.hosting = HostsPlatform(d); p.hosting {
		if p.recipient, err = readRecipient(d.Params[ParamRecipient]); err != nil {
			return p, err
		}
	}
	if !p.cloudOnly {
		number, err := robotServerNumber(d)
		p.robotServer = number
		if err != nil {
			return p, err
		}
	}
	p.cloud, err = k.readPool(d)
	return p, err
}

// readPool reads the Cloud pool, refusing on a cluster with no dedicated server one that can shrink to no worker,
// since its apps run on the pool alone.
func (k *Cluster) readPool(d contract.Desired) (pools.CloudPool, error) {
	pool, err := readCloudPool(d)
	if err == nil && k.cloudOnly(d) && pool.Min < 1 {
		return pool, handler.Refuse("invalid-param",
			`params["cloudPoolMin"] is %q, and a cluster with no dedicated server runs its apps on the Cloud pool alone, so the pool keeps at least 1 worker`,
			d.Params["cloudPoolMin"])
	}
	return pool, err
}

// readServerTypes reads one type or an ordered, comma-separated list, refusing any type the cluster does not run on.
func readServerTypes(raw string) ([]string, error) {
	var types []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if !slices.Contains(controlplane.ServerTypes, t) {
			return nil, handler.Refuse("invalid-param", `params["serverType"] is %q, and %q is not a type the cluster runs on: it runs on %s`,
				raw, t, strings.Join(controlplane.ServerTypes, ", "))
		}
		types = append(types, t)
	}
	return types, nil
}

func robotServerNumber(d contract.Desired) (int64, error) {
	raw := d.Params[ParamRobotServer]
	if raw == "" {
		return 0, handler.Refuse("missing-param",
			`the dedicated server's number is required: params["%s"] is empty, and the cluster's worker runs on that Robot server`, ParamRobotServer)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, handler.Refuse("invalid-param", `params["%s"] is %q, and a Robot server number is a positive whole number`, ParamRobotServer, raw)
	}
	return n, nil
}

func (k *Cluster) robotLogin(d contract.Desired) (*robot.Client, error) {
	user, password := d.Secrets["hetzner"]["robot_user"], d.Secrets["hetzner"]["robot_password"]
	if user == "" || password == "" {
		return nil, handler.Refuse("missing-robot-access",
			"the Hetzner connection holds no Robot login: secrets.hetzner.robot_user or robot_password is empty, so the dedicated server cannot be adopted; add the Robot webservice login to the connection and retry")
	}
	return k.cfg.RobotClient(user, password), nil
}

func subscriptionID(d contract.Desired) (string, error) {
	if !controlplane.ValidLabelValue(d.SubscriptionID) {
		return "", handler.Refuse("invalid-subscription",
			"the subscription id is not a valid Hetzner label value, so the cluster's resources cannot be labelled or found by it")
	}
	return d.SubscriptionID, nil
}

func egressAddress() (netip.Addr, error) {
	addr, err := netip.ParseAddr(os.Getenv(EgressEnv))
	if err != nil || !addr.Is4() {
		return netip.Addr{}, handler.Refuse("egress-address-unset",
			"the operator's %s holds no IPv4 address, and the dedicated server's firewall admits the rescue session from that address alone, so nothing was changed; the operator's host must set it", EgressEnv)
	}
	return addr, nil
}

type ensureRequest struct {
	k            *Cluster
	token, subID string
	params       clusterParams
	gh           *githubAccess
	robot        *robot.Client
	egress       netip.Addr
	secrets      []string
	d            contract.Desired
	// minted receives the gateway CA the call's install mints, which the answer keeps as the block's own secret.
	minted *gateway.CA
	// reports are the call's node reports, read once for the plane's removals and the Cloud pool's scaling.
	reports *GatewayReports
}

func (k *Cluster) readEnsureRequest(d contract.Desired) (ensureRequest, error) {
	req := ensureRequest{k: k, secrets: operator.RequestSecrets(d), d: d, minted: &gateway.CA{}, reports: k.reportsOf(d)}
	var err error
	if req.token, err = operator.HetznerToken(d); err != nil {
		return req, err
	}
	if req.subID, err = subscriptionID(d); err != nil {
		return req, err
	}
	if req.params, err = k.readParams(d); err != nil {
		return req, err
	}
	if err = k.refuseUndeclaredSize(req.params.size); err != nil {
		return req, err
	}
	if err = operator.RefuseUnreleasedTools(k.cfg.ToolsImage()); err != nil {
		return req, err
	}
	if !req.params.cloudOnly {
		if req.robot, err = k.robotLogin(d); err != nil {
			return req, err
		}
		if req.egress, err = egressAddress(); err != nil {
			return req, err
		}
	}
	req.gh, err = k.dialGitHub(d, req.params.appName)
	return req, err
}

// plane installs the Robot worker, when the cluster has one, and keeps the gateway CA, in the one call the install
// renders them.
func (req ensureRequest) plane(ctx context.Context, api controlplane.API) controlplane.Plane {
	p := req.k.placeApps(req.k.NewPlane(api, req.subID, req.params.appName), req.d)
	p = withTelemetry(req.k.withReports(ctx, p, req.d, req.reports), req.d)
	p.Size, p.ServerTypes, p.RepoURL, p.Removals = req.params.size, req.params.serverTypes, req.gh.Repo.URL(), req.gh.removals()
	if req.params.hosting {
		p.Hosting = req.hosting()
	}
	p.Installed = func(ctx context.Context, network controlplane.Network, cluster nodeconfig.Cluster, ca gateway.CA) error {
		if !req.params.cloudOnly {
			if err := req.k.startWorkerInstall(ctx, req, p, network, cluster); err != nil {
				return err
			}
		}
		*req.minted = ca
		return nil
	}
	return p
}

func (req ensureRequest) ownSecrets() map[string]string {
	if req.minted == nil || len(req.minted.Key) == 0 {
		return nil
	}
	return ownSecrets(*req.minted)
}

type convergence struct {
	plane controlplane.Plane
	state controlplane.State
	// installing is whether this process ran the subscription's install while state was read.
	installing bool
	req        ensureRequest
	repo       clusterrepo.Read
}

// Ensure takes one step that waits on Hetzner, so each call ends well inside the platform's two-minute verb timeout, and
// refuses before any change a cluster it cannot act on unambiguously, a repository without the subscription's mark, or
// a dedicated server it may not adopt. The network and the repository need no wait, so they come first. The handler
// calls EnsureResourcesOwning instead, which keeps the gateway CA an install mints.
func (k *Cluster) Ensure(ctx context.Context, d contract.Desired) error {
	_, err := k.ensure(ctx, d)
	return WaitOutRobotLimit(err)
}

// EnsureOwning is Ensure answering with the gateway CA an install minted, which the block keeps as its own secret, and
// without waiting out Robot's request limit.
func (k *Cluster) EnsureOwning(ctx context.Context, d contract.Desired) (map[string]string, error) {
	return k.ensure(ctx, d)
}

func (k *Cluster) ensure(ctx context.Context, d contract.Desired) (map[string]string, error) {
	req, err := k.readEnsureRequest(d)
	if err != nil {
		return nil, err
	}
	c, err := readConvergence(ctx, req.plane(ctx, k.cfg.CloudAPI(req.token)), req)
	if err != nil {
		return nil, err
	}
	if err := refusePredatingZones(c.state); err != nil {
		return nil, err
	}
	c.plane.ReplaceNodes = namedNodes(d.Replace)
	c.plane.Records = []controlplane.Record{pools.ReplaceWorkers(req.params.cloud, namedWorkers(d.Replace))}
	network, err := c.plane.EnsureNetwork(ctx, c.state)
	if err != nil {
		return nil, err
	}
	if err := c.req.gh.Ensure(ctx, c.repo); err != nil {
		return nil, err
	}
	due, err := c.carryRelease(ctx)
	if err != nil {
		return nil, err
	}
	s, ok, err := c.nextStep(ctx, network)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, c.rollNodes(ctx, due, network)
	}
	err = refusal(s.Take(ctx, k.cfg.Block, req.subID))
	return req.ownSecrets(), err
}

func readConvergence(ctx context.Context, plane controlplane.Plane, req ensureRequest) (convergence, error) {
	st, installing, err := req.k.readCluster(ctx, plane, req.subID)
	if err != nil {
		return convergence{}, err
	}
	repo, err := req.gh.Read(ctx)
	if err != nil {
		return convergence{}, err
	}
	c := convergence{plane: plane, state: st, installing: installing, req: req, repo: repo}
	return c, c.refusal(ctx)
}

func (c convergence) refusal(ctx context.Context) error {
	if err := c.plane.Refusal(c.state); err != nil {
		return refusal(err)
	}
	if err := c.req.gh.RefuseUnmarked(c.repo); err != nil {
		return err
	}
	if err := c.workerRefusal(ctx); err != nil {
		return err
	}
	return c.poolRefusal(ctx)
}

// nextStep is the control plane's next step, and once the worker is written or the cluster has none, the step that puts
// back a lost Robot resource on a cluster with a dedicated server, then a Cloud worker's next replacement stage, then the
// one that creates the Cloud pool's missing workers.
func (c convergence) nextStep(ctx context.Context, network controlplane.Network) (controlplane.Step, bool, error) {
	s, ok, err := c.plane.NextStep(ctx, c.state, network)
	if err != nil || ok || !c.workerDone(network) {
		return s, ok, refusal(err)
	}
	if !c.req.params.cloudOnly {
		if repair, repairs, err := c.robotTarget().repairStep(ctx); err != nil || repairs {
			return repair, repairs, err
		}
	}
	if pools.ChangeUnderWay(c.state) {
		if replace, replaces, err := pools.ReplaceStep(ctx, c.plane, c.req.params.cloud); err != nil || replaces {
			return replace, replaces, refusal(err)
		}
	}
	return c.poolStep(ctx)
}

// refusal answers the plane's refusal as the block framework's, keeping its class, unless another failure joins it.
func refusal(err error) error {
	var r *controlplane.Refusal
	if !errors.As(err, &r) || holdsMore(err, r) {
		return err
	}
	return handler.Refuse(r.Class, "%s", r.Message)
}

// holdsMore reports whether err holds a failure beside r, such as a failed withdrawal, which the next call retries.
func holdsMore(err error, r *controlplane.Refusal) bool {
	switch e := err.(type) {
	case *controlplane.Refusal:
		return e != r
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(e.Unwrap(), func(inner error) bool { return holdsMore(inner, r) })
	case interface{ Unwrap() error }:
		return holdsMore(e.Unwrap(), r)
	}
	return true
}
