package robotcluster

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

// observers judge every declared resource by the labels, names and topics it carries, never by a recorded identity,
// so an unsubscribe of a subscription the engine never recorded still settles from what the providers hold.
func (k *Cluster) Observers() map[string]handler.Observer {
	gw := controlplane.GatewayLocation
	out := map[string]handler.Observer{
		ResourceNetwork:                          labelledObserver{k: k, kind: "network", list: func(st controlplane.State) []int64 { return controlplane.IDs(st.Networks, networkID) }},
		ResourceFirewall:                         labelledObserver{k: k, kind: "firewall", list: func(st controlplane.State) []int64 { return controlplane.IDs(st.Firewalls, firewallID) }},
		Located(ResourceAPILoadBalancer, gw):     labelledObserver{k: k, kind: gw + " API load balancer", list: loadBalancersIn(controlplane.RoleAPI, gw), outputs: loadBalancerOutputs(controlplane.RoleAPI, gw, exposurePrivate)},
		Located(ResourceIngressLoadBalancer, gw): labelledObserver{k: k, kind: gw + " ingress load balancer", list: loadBalancersIn(controlplane.RoleIngress, gw), outputs: loadBalancerOutputs(controlplane.RoleIngress, gw, exposurePublic)},
		ResourceRepository:                       clusterrepo.RepositoryObserver{Dial: k.dialRepository, Check: deployKeyCheck},
		ResourceSeedCommit:                       clusterrepo.SeedObserver{Dial: k.dialRepository},
		k.cfg.ClusterType:                        operator.ObserverFunc(k.observeCluster),
		ResourceCloudWorker:                      cloudWorkerObserver{k: k},
		ResourceDedicatedServer:                  robotObserver{k: k, judge: judgeDedicatedServer},
		ResourceVSwitch:                          robotObserver{k: k, judge: judgeVSwitch},
		ResourceRobotFirewall:                    robotObserver{k: k, judge: judgeRobotFirewall},
	}
	for _, loc := range controlplane.Locations {
		out[Located(ResourceNode, loc)] = labelledObserver{k: k, kind: loc + " node", list: func(st controlplane.State) []int64 {
			return controlplane.IDs(controlplane.ServersIn(st.Servers[controlplane.RoleNode], loc), controlplane.ServerID)
		}}
		out[Located(ResourceGateway, loc)] = gatewayObserver{k: k, loc: loc}
	}
	return out
}

func Located(resource, loc string) string { return resource + "-" + loc }

func networkID(n controlplane.Network) int64 { return n.ID }

func firewallID(f controlplane.Firewall) int64 { return f.ID }

// labelledObserver reads one kind: present when exactly one resource carries the labels, absent when none does.
type labelledObserver struct {
	k    *Cluster
	kind string
	list func(controlplane.State) []int64
	// outputs, when set, are the present resource's outputs, of which an empty one is dropped.
	outputs func(controlplane.State) map[string]string
}

func (o labelledObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	p, refused, ok := o.k.hetznerReader(r.Desired)
	if !ok {
		return refused, nil
	}
	st, err := p.Read(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	obs := judgeLabelled(o.kind, o.list(st))
	if obs.State == contract.Present && o.outputs != nil {
		obs.Outputs = withoutEmpty(o.outputs(st))
	}
	return obs, nil
}

func judgeLabelled(kind string, ids []int64) contract.Observation {
	switch len(ids) {
	case 0:
		return contract.Observation{State: contract.Absent, StatusDetail: "no " + kind + " carries this subscription's labels"}
	case 1:
		return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("%s %d carries this subscription's labels", kind, ids[0])}
	default:
		return operator.Unknown(controlplane.DuplicatesDetail(kind, ids))
	}
}

// The outputs a port scan from outside reads: a resource's public address, and whether that address should answer.
const (
	outputPublicIP  = "publicIp"
	outputExposure  = "exposure"
	exposurePublic  = "public"
	exposurePrivate = "private"
)

func loadBalancersIn(r controlplane.Role, loc string) func(controlplane.State) []int64 {
	return func(st controlplane.State) []int64 {
		return controlplane.IDs(controlplane.LoadBalancersIn(st.LoadBalancers[r], loc), controlplane.LoadBalancerID)
	}
}

// loadBalancerOutputs gives a port scan the load balancer's assigned public address and whether it should answer.
func loadBalancerOutputs(r controlplane.Role, loc, exposure string) func(controlplane.State) map[string]string {
	return func(st controlplane.State) map[string]string {
		lb := controlplane.LoadBalancersIn(st.LoadBalancers[r], loc)[0]
		return map[string]string{outputPublicIP: lb.AssignedPublicIPv4, outputExposure: exposure}
	}
}

// gatewayObserver reads a location's gateway as labelledObserver does, and gives a check from outside the cluster its
// addresses, its image list and the public CA certificate the nodes trust it under, never the CA's key.
type gatewayObserver struct {
	k   *Cluster
	loc string
}

func (o gatewayObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	p, refused, ok := o.k.hetznerReader(r.Desired)
	if !ok {
		return refused, nil
	}
	st, err := p.Read(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	gateways := controlplane.ServersIn(st.Servers[controlplane.RoleGateway], o.loc)
	obs := judgeLabelled(o.loc+" gateway", controlplane.IDs(gateways, controlplane.ServerID))
	if obs.State != contract.Present {
		return obs, nil
	}
	gw := gateways[0]
	network, _ := st.Network()
	obs.Outputs = withoutEmpty(map[string]string{
		outputPublicIP:  gw.PublicIPv4,
		"privateIp":     gw.PrivateIPs[network.ID],
		"imageList":     gw.Labels[controlplane.KeyGatewayList],
		"caCertificate": r.Desired.OwnSecrets[OwnGatewayCACert],
	})
	return obs, nil
}

// withoutEmpty drops each output that holds no value, so a reader that needs an output finds it missing rather than blank.
func withoutEmpty(outputs map[string]string) map[string]string {
	maps.DeleteFunc(outputs, func(_, v string) bool { return v == "" })
	return outputs
}

// robotObserver reads the worker's Robot resources, which carry the subscription in their name.
type robotObserver struct {
	k     *Cluster
	judge func(t robotTarget, st robotState) contract.Observation
}

func (o robotObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	t, refused, ok := o.k.robotReader(ctx, r.Desired)
	if !ok {
		return refused, nil
	}
	st, err := t.read(ctx)
	switch {
	case robot.ErrorCode(err) == "SERVER_NOT_FOUND":
		return contract.Observation{State: contract.Absent, StatusDetail: fmt.Sprintf("Robot holds no dedicated server %d", t.server)}, nil
	case errors.Is(err, robot.ErrRateLimited):
		return operator.Unknown(robotLimitDetail), nil
	case err != nil:
		return contract.Observation{}, err
	}
	return o.judge(t, st), nil
}

func judgeDedicatedServer(t robotTarget, st robotState) contract.Observation {
	if st.server.Name != t.name {
		return contract.Observation{State: contract.Absent, StatusDetail: fmt.Sprintf("dedicated server %d is named %q, not %s", t.server, st.server.Name, t.name)}
	}
	return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("dedicated server %d in %s is named %s", t.server, st.server.DC, t.name)}
}

func judgeVSwitch(t robotTarget, st robotState) contract.Observation {
	switch {
	case st.vswitches == 0:
		return contract.Observation{State: contract.Absent, StatusDetail: "no uncancelled vSwitch is named " + t.name}
	case st.vswitches > 1:
		return operator.Unknown(fmt.Sprintf("%d vSwitches are named %s", st.vswitches, t.name))
	case !st.onSwitch:
		return operator.Unknown(fmt.Sprintf("vSwitch %d does not carry dedicated server %d", st.vswitch.ID, t.server))
	}
	return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("vSwitch %d on VLAN %d carries dedicated server %d", st.vswitch.ID, st.vswitch.VLAN, t.server)}
}

func judgeRobotFirewall(t robotTarget, st robotState) contract.Observation {
	switch {
	case st.server.Name != t.name:
		return contract.Observation{State: contract.Absent, StatusDetail: fmt.Sprintf("dedicated server %d is not named %s, so its firewall is not the cluster's", t.server, t.name)}
	case !HoldsWorkerRules(st.firewall, t.firewall()):
		return operator.Unknown(fmt.Sprintf("the firewall of dedicated server %d is %s and does not admit the cluster's network", t.server, st.firewall.Status))
	}
	return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("the firewall of dedicated server %d is active and admits the cluster's network", t.server)}
}

// robotReader answers Absent for a cluster with no dedicated server, and Unknown for a request the worker's Robot
// resources cannot be read with, or one that names no server while the cluster network records a server's install.
func (k *Cluster) robotReader(ctx context.Context, d contract.Desired) (robotTarget, contract.Observation, bool) {
	if k.cloudOnly(d) {
		return robotTarget{}, k.noServerReading(ctx, d), false
	}
	rb, err := k.robotLogin(d)
	if err != nil {
		return robotTarget{}, operator.Unknown("the dedicated server cannot be read: " + operator.RefusalDetail(err)), false
	}
	number, err := robotServerNumber(d)
	if err != nil {
		return robotTarget{}, operator.Unknown("the dedicated server cannot be named: " + operator.RefusalDetail(err)), false
	}
	if !controlplane.ValidLabelValue(d.SubscriptionID) {
		return robotTarget{}, operator.Unknown("the subscription id is not a valid label value, so the worker's Robot names cannot be formed"), false
	}
	p := k.PlaneFor(nil, d)
	return robotTarget{robot: rb, server: number, name: RobotName(d.SubscriptionID), plane: p}, contract.Observation{}, true
}

func (k *Cluster) noServerReading(ctx context.Context, d contract.Desired) contract.Observation {
	p, refused, ok := k.hetznerReader(d)
	if !ok {
		return refused
	}
	networks, err := p.API.ListNetworks(ctx, p.Selector())
	if err != nil {
		return operator.Unknown("the cluster network cannot be read, so whether it records a dedicated server is not known: " + err.Error())
	}
	for _, n := range networks {
		if detail := serverDropped(n); detail != "" {
			return operator.Unknown(detail)
		}
	}
	return contract.Observation{State: contract.Absent, StatusDetail: "the cluster has no dedicated server, as its request names none"}
}

func deployKeyCheck(ctx context.Context, d contract.Desired, a *clusterrepo.Access) ([]string, error) {
	if !HostsPlatform(d) {
		return nil, nil
	}
	keys, err := (&githubAccess{Access: a}).listDeployKeys(ctx)
	if err != nil {
		return nil, err
	}
	return deployKeyProblems(a.Repo.FullName(), keys), nil
}

// hetznerReader answers Unknown, before any call, for a request the subscription's Cloud resources cannot be read with.
func (k *Cluster) hetznerReader(d contract.Desired) (controlplane.Plane, contract.Observation, bool) {
	token := d.Secrets["hetzner"]["api_token"]
	if token == "" {
		return controlplane.Plane{}, operator.Unknown("no Hetzner token on this call (secrets.hetzner.api_token is empty), so the cluster cannot be read"), false
	}
	if !controlplane.ValidLabelValue(d.SubscriptionID) {
		return controlplane.Plane{}, operator.Unknown("the subscription id is not a valid Hetzner label value, so the cluster's resources cannot be found by it"), false
	}
	return k.PlaneFor(k.cfg.CloudAPI(token), d), contract.Observation{}, true
}

// Observe reads the whole cluster: present once it stands as the block makes it and every node runs the release's
// machine configuration, absent once no labelled resource or marked repository is left, unknown naming what is missing
// in between.
func (k *Cluster) Observe(ctx context.Context, d contract.Desired) (contract.Observation, error) {
	return k.observeCluster(ctx, d)
}

func (k *Cluster) observeCluster(ctx context.Context, d contract.Desired) (contract.Observation, error) {
	p, refused, ok := k.hetznerReader(d)
	if !ok {
		return refused, nil
	}
	reports := k.reportsOf(d)
	p = withTelemetry(k.withReports(ctx, p, d, reports), d)
	if HostsPlatform(d) {
		// A hosting cluster's gateways mirror the hosting set's list, which tells a stale gateway; the observe installs nothing.
		p.Hosting = &controlplane.Hosting{}
	}
	st, installing, err := k.readCluster(ctx, p, d.SubscriptionID)
	if err != nil {
		return contract.Observation{}, err
	}
	repo, err := k.observeRepo(ctx, d)
	if err != nil {
		return contract.Observation{}, err
	}
	if repo.gh != nil {
		p.Removals = repo.gh.removals()
	}
	types, typesErr := requestedServerTypes(d)
	p.ServerTypes = types
	if st.Empty() && repo.Absent() {
		return contract.Observation{
			State:        contract.Absent,
			StatusDetail: "no server, load balancer, firewall, network or snapshot carries this subscription's labels, and " + repo.Describe(),
		}, nil
	}
	if typesErr != nil {
		return operator.Unknown("the request's server types cannot be read: " + operator.RefusalDetail(typesErr)), nil
	}
	problems := p.Problems(ctx, st)
	if pools.ChangeUnderWay(st) {
		if pool, err := readCloudPool(d); err != nil {
			problems = append(problems, "the Cloud worker change the network records cannot be judged, since "+operator.RefusalDetail(err))
		} else {
			problems = append(problems, pools.ReplacementProblems(ctx, p, pool)...)
		}
	}
	if repo.State != clusterrepo.Present {
		problems = append(problems, repo.Describe())
	}
	worker := p.Name("worker")
	if k.cloudOnly(d) {
		worker = ""
		if network, ok := st.Network(); ok {
			if dropped := serverDropped(network); dropped != "" {
				problems = append(problems, dropped)
			}
		}
	} else if network, ok := st.Network(); ok && workerProgress(network) != WorkerWritten {
		problems = append(problems, workerDetail(network))
	}
	if len(problems) == 0 {
		if problem := k.reportProblem(ctx, d, st, worker); problem != "" {
			problems = append(problems, problem)
		}
	}
	var recovery map[string]string
	if len(problems) == 0 && HostsPlatform(d) {
		var gap string
		if recovery, gap, err = repo.gh.hostingOutputs(ctx); err != nil {
			return contract.Observation{}, err
		}
		if gap != "" {
			problems = append(problems, gap)
		}
	}
	var hosting string
	if len(problems) == 0 {
		var problem string
		if hosting, problem, err = readRelease(ctx, d, p, st, repo, worker); err != nil {
			return contract.Observation{}, err
		}
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	left, err := k.stepLeft(ctx, d, st, installing, repo)
	if errors.Is(err, robot.ErrRateLimited) {
		problems = append(problems, robotLimitDetail)
	} else if err != nil {
		return contract.Observation{}, err
	}
	if len(problems) > 0 || left != "" {
		outputs, gap := enrolment(st, repo)
		if gap != "" {
			problems = append(problems, gap)
		}
		if len(problems) == 0 {
			problems = append(problems, left)
		}
		obs := operator.Unknown(strings.Join(problems, "; "))
		obs.Outputs, obs.Redrive = outputs, left != ""
		return obs, nil
	}
	outputs, detail := p.Outputs(st)
	outputs["clusterRepoUrl"] = repo.URL
	maps.Copy(outputs, recovery)
	if worker != "" {
		detail += ", the Robot worker is written"
	}
	detail += ", and the newest node report reads every node Ready"
	if hosting != "" {
		detail += ", and " + hosting
	}
	obs := contract.Observation{State: contract.Present, Outputs: outputs, StatusDetail: detail}
	cause, why, err := p.Unhealthy(ctx, st)
	if err == nil && cause == "" {
		cause, why, err = poolHold(ctx, d, st, reports)
	}
	if err != nil {
		return contract.Observation{}, err
	}
	if cause != "" {
		obs.Unhealthy = &contract.Unhealthy{Cause: cause, Detail: why}
	}
	return obs, nil
}

func workerDetail(network controlplane.Network) string {
	switch workerProgress(network) {
	case WorkerRunning:
		return "the Robot worker's install has run since " + workerProgressAt(network)
	case WorkerFailed:
		return "the Robot worker's install failed at " + workerProgressAt(network)
	}
	return "the Robot worker's install has not begun"
}

// readRelease reads the release's component set back once main holds it as stated, then which nodes run the release's
// machine configuration, and names the wait while main differs and no subscribe can commit the set until every
// gateway serves the release.
func readRelease(ctx context.Context, d contract.Desired, p controlplane.Plane, st controlplane.State, repo repoObservation, worker string) (applied, problem string, err error) {
	if p.GatewayCA == nil || repo.gh == nil {
		// A plane without the kept gateway CA renders no node report, and so has carried no release.
		return "", "", nil
	}
	release, err := ReleaseFor(p, st, HostsPlatform(d), repo.gh.Repo)
	if err != nil {
		return "", "", err
	}
	due, err := repo.gh.releaseChanges(ctx, release)
	if err != nil {
		return "", "", err
	}
	if len(due.changes) == 0 {
		if applied, problem, err = repo.gh.readBack(ctx, d, release); err != nil || problem != "" {
			return "", problem, err
		}
		r, err := readRollout(ctx, p, st, repo.gh, release.ToolsImage, worker)
		var refused *handler.Refused
		if err = refusal(err); errors.As(err, &refused) {
			return "", "the release's machine configuration rollout is refused with " + refused.Class + ": " + refused.Detail, nil
		}
		if err != nil || r.problem != "" {
			return "", r.problem, err
		}
		return "every node runs the release's machine configuration, and " + applied, "", nil
	}
	waits, err := carriageWaits(ctx, p, st)
	if err != nil || !waits {
		return "", "", err
	}
	return "", repo.gh.Repo.FullName() + "'s main does not hold the release's component set as stated, and the carriage waits until every gateway serves this release", nil
}

// stepLeft names the step a subscribe sent now would take, or the refusal it would answer, read from what Hetzner, Robot
// and GitHub hold, and is empty when none is left, so the engine re-drives subscribe once the step in flight has
// finished and never while it runs.
func (k *Cluster) stepLeft(ctx context.Context, d contract.Desired, st controlplane.State, installing bool, repo repoObservation) (string, error) {
	req, err := k.readEnsureRequest(d)
	if err != nil || repo.State == clusterrepo.Unknown {
		// A request the subscribe refuses before reading the cluster asks for none, since its subscribe was refused already.
		return "", nil
	}
	// The observe's own access holds the listing of main the read-back took.
	req.gh = repo.gh
	c := convergence{plane: req.plane(ctx, k.cfg.CloudAPI(req.token)), state: st, installing: installing, req: req, repo: clusterrepo.Read{Exists: repo.State != clusterrepo.Gone, Marked: repo.State == clusterrepo.Present}}
	network, ok := st.Network()
	var refused *handler.Refused
	if err := c.refusal(ctx); errors.Is(err, robot.ErrRateLimited) {
		return "", err
	} else if errors.As(err, &refused) {
		return refusalLeft(refused), nil
	} else if !ok || !c.repo.Exists {
		return "the next subscribe creates the cluster's missing network or repository", nil
	} else if err != nil {
		return "", err
	}
	due, left, err := c.carriageLeft(ctx)
	if err != nil || left != "" {
		return left, err
	}
	s, ok, err := c.nextStep(ctx, network)
	switch {
	case errors.As(err, &refused):
		return refusalLeft(refused), nil
	case err != nil:
		return "", err
	case !ok:
		return c.rolloutLeft(ctx, due, network)
	}
	return "the next subscribe takes the step to " + s.What, nil
}

func (c convergence) rolloutLeft(ctx context.Context, due carriage, network controlplane.Network) (string, error) {
	r, err := c.rolloutDue(ctx, due, network)
	var refused *handler.Refused
	if err = refusal(err); errors.As(err, &refused) {
		return refusalLeft(refused), nil
	} else if err != nil {
		return "", err
	}
	for _, content := range r.changes {
		if content != nil {
			return "the next subscribe commits the release's machine configuration rollout to " + c.req.gh.Repo.FullName(), nil
		}
	}
	if len(r.changes) > 0 {
		return "the next subscribe takes out of " + c.req.gh.Repo.FullName() + " a machine configuration rollout no node waits on", nil
	}
	return "", nil
}

func refusalLeft(r *handler.Refused) string {
	return "the next subscribe refuses with " + r.Class + ": " + r.Detail
}

// carriageLeft names the commit the next subscribe makes, as ensure carries the release before the plane's next step.
func (c convergence) carriageLeft(ctx context.Context) (carriage, string, error) {
	due, err := c.releaseDue(ctx)
	if err != nil || len(due.changes) == 0 {
		return due, "", err
	}
	return due, "the next subscribe commits the release's component set to " + c.req.gh.Repo.FullName() + ", which main does not hold as stated", nil
}

// enrolment gives a cluster not yet present the values a node's enrolment is judged against, once the gateway carries
// its digest and the marked repository exists, so the platform holds them before any node can report.
func enrolment(st controlplane.State, repo repoObservation) (map[string]string, string) {
	digest, gap := st.EnrolmentDigest()
	if digest == "" || repo.State != clusterrepo.Present {
		return nil, gap
	}
	return map[string]string{"enrolmentTokenSha256": digest, "clusterRepoUrl": repo.URL}, ""
}

type repoObservation struct {
	clusterrepo.Observation
	gh *githubAccess
}

// observeRepo leaves the repository unknown, with no GitHub call, when the request cannot name it or reach GitHub.
func (k *Cluster) observeRepo(ctx context.Context, d contract.Desired) (repoObservation, error) {
	obs, err := clusterrepo.Observe(ctx, d, k.dialRepository)
	repo := repoObservation{Observation: obs}
	if obs.Access != nil {
		repo.gh = &githubAccess{Access: obs.Access, k: k}
	}
	return repo, err
}
