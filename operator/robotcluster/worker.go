package robotcluster

// A Robot worker's install outlasts a verb, so it runs in this process's background from the call that renders the
// cluster's keys, which exist nowhere else, and keeps its progress in the Cloud network's labels for every later call.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/safe"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

// The network labels that record the worker's install, and the stages they record.
const (
	KeyWorkerInstall   = "mitstack.dev/worker-install"
	KeyWorkerInstallAt = "mitstack.dev/worker-install-at"
	WorkerRunning      = "running"
	WorkerWritten      = "written"
	WorkerFailed       = "failed"
)

const (
	// RoleImageServer labels the short-lived server that builds and serves the worker's image, so Plane.Delete finds it.
	RoleImageServer controlplane.Role = "robot-image-server"
	// workerSubnet is the network's subnet on the vSwitch, whose first address is Hetzner's vSwitch gateway.
	workerSubnet         = "10.0.1.0/24"
	workerInstallTimeout = time.Hour
	progressTimeout      = time.Minute
)

var workerAddress = netip.MustParsePrefix("10.0.1.2/24")

// Worker is the Robot server the install writes, the names it and its vSwitch carry, and the firewall rules it adds.
type Worker struct {
	Server   int64
	Name     string
	VSwitch  string
	Hostname string
	// Address lies on the vSwitch subnet, whose first address is Hetzner's vSwitch gateway.
	Address netip.Prefix
	Rules   []robot.Rule
}

// WorkerInstall is what one install of the worker is handed: Robot, the Cloud its image server runs in, the cluster it
// joins, and the public IPv4 address its rescue session leaves from, the one address the firewall admits SSH from.
type WorkerInstall struct {
	Robot   *robot.Client
	Cloud   robot.Cloud
	Cluster nodeconfig.Cluster
	From    netip.Addr
	Worker  Worker
}

// WorkerFirewall is the worker's Robot firewall: replies, ping from the cluster's network, and the worker's own rules.
func WorkerFirewall(network netip.Prefix, w Worker) robot.Firewall {
	rules := append(robot.ReplyRules(), robot.Rule{Name: "icmp from network", IPVersion: "ipv4", SrcIP: network.String(), Protocol: "icmp", Action: "accept"})
	return robot.Firewall{Status: "active", FilterIPv6: true, WhitelistHOS: true, Input: append(rules, w.Rules...)}
}

type workerJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// JobRunning is whether this process runs the subscription's worker install.
func (k *Cluster) JobRunning(subID string) bool {
	k.jobs.Lock()
	defer k.jobs.Unlock()
	return k.jobs.running[subID] != nil
}

// readCluster reads the cluster and whether this process runs its install, asked on both sides of the read, since an
// install that ends during the read records its outcome only before it leaves jobs.
func (k *Cluster) readCluster(ctx context.Context, p controlplane.Plane, subID string) (controlplane.State, bool, error) {
	installing := k.JobRunning(subID)
	st, err := p.Read(ctx)
	return st, installing || k.JobRunning(subID), err
}

// StopJob cancels the subscription's install and waits for its clean-up, which returns the server's firewall and boot.
func (k *Cluster) StopJob(ctx context.Context, subID string) error {
	k.jobs.Lock()
	j := k.jobs.running[subID]
	k.jobs.Unlock()
	if j == nil {
		return nil
	}
	j.cancel()
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the Robot worker's install is still cleaning up, so nothing was deleted: %w", ctx.Err())
	}
}

func workerFor(p controlplane.Plane, server int64) Worker {
	name := RobotName(p.Subscription)
	return Worker{
		Server: server, Name: name, VSwitch: name, Hostname: p.Name("worker"), Address: workerAddress,
		Rules: []robot.Rule{{Name: "cluster network", IPVersion: "ipv4", SrcIP: controlplane.NetworkIPRange, Action: "accept"}},
	}
}

// startWorkerInstall marks the install running before it starts, so a process that dies mid-install leaves the mark.
func (k *Cluster) startWorkerInstall(ctx context.Context, req ensureRequest, p controlplane.Plane, network controlplane.Network, cluster nodeconfig.Cluster) error {
	if err := setWorkerProgress(ctx, p, network.ID, WorkerRunning); err != nil {
		return err
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerInstallTimeout)
	j := &workerJob{cancel: cancel, done: make(chan struct{})}
	k.jobs.Lock()
	k.jobs.running[req.subID] = j
	k.jobs.Unlock()
	install := WorkerInstall{
		Robot: req.robot, Cluster: cluster, From: req.egress,
		Cloud:  imageCloud{plane: p, network: network.ID, robot: req.robot, vswitch: RobotName(req.subID), poll: k.cfg.ImageServerPoll()},
		Worker: workerFor(p, req.params.robotServer),
	}
	k.cfg.StartJob(func() {
		defer close(j.done)
		defer cancel()
		outcome := WorkerWritten
		if err := k.cfg.InstallWorker(jobCtx, install); err != nil {
			outcome = WorkerFailed
			log.Print(safe.LogSafe(fmt.Sprintf("%s: subscription %s: the install of dedicated server %d failed: %v", k.cfg.Block, req.subID, install.Worker.Server, err), req.secrets...))
		}
		pctx, pcancel := context.WithTimeout(context.Background(), progressTimeout)
		defer pcancel()
		if err := setWorkerProgress(pctx, p, network.ID, outcome); err != nil {
			log.Print(safe.LogSafe(fmt.Sprintf("%s: subscription %s: recording the worker install %s: %v", k.cfg.Block, req.subID, outcome, err), req.secrets...))
		}
		k.jobs.Lock()
		delete(k.jobs.running, req.subID)
		k.jobs.Unlock()
	})
	return nil
}

func setWorkerProgress(ctx context.Context, p controlplane.Plane, networkID int64, status string) error {
	networks, err := p.API.ListNetworks(ctx, p.Selector())
	if err != nil {
		return fmt.Errorf("reading the cluster network: %w", err)
	}
	i := slices.IndexFunc(networks, func(n controlplane.Network) bool { return n.ID == networkID })
	if i < 0 {
		return fmt.Errorf("the cluster network %d carries this subscription's labels no more", networkID)
	}
	labels := maps.Clone(networks[i].Labels)
	labels[KeyWorkerInstall], labels[KeyWorkerInstallAt] = status, strconv.FormatInt(p.Now().Unix(), 10)
	if err := p.API.SetNetworkLabels(ctx, networkID, labels); err != nil {
		return fmt.Errorf("recording the worker install as %s: %w", status, err)
	}
	return nil
}

func workerProgress(n controlplane.Network) string { return n.Labels[KeyWorkerInstall] }

// serverDropped names a network that records a dedicated server's install although the request names no server, and
// is empty otherwise.
func serverDropped(n controlplane.Network) string {
	if workerProgress(n) == "" {
		return ""
	}
	return fmt.Sprintf(`the cluster network records the install of a dedicated server as %s, but params["robotServerNumber"] is empty, `+
		"so the server and its vSwitch would be left outside the cluster; name the server again, or unsubscribe with it named to release it",
		workerProgress(n))
}

// workerDone is whether the Robot worker is written, or the cluster has none to write.
func (c convergence) workerDone(n controlplane.Network) bool {
	return c.req.params.cloudOnly || workerProgress(n) == WorkerWritten
}

func workerProgressAt(n controlplane.Network) string {
	at, err := strconv.ParseInt(n.Labels[KeyWorkerInstallAt], 10, 64)
	if err != nil {
		return "an unrecorded time"
	}
	return time.Unix(at, 0).UTC().Format(time.RFC3339)
}

// workerRefusal refuses an install that failed or that a restart of this process lost, since the cluster's keys it
// needs are gone, and before the install a dedicated server the worker cannot run on.
func (c convergence) workerRefusal(ctx context.Context) error {
	network, _ := c.state.Network()
	if c.req.params.cloudOnly {
		if detail := serverDropped(network); detail != "" {
			return handler.Refuse("dedicated-server-removed", "%s", detail)
		}
		return nil
	}
	switch workerProgress(network) {
	case WorkerFailed:
		return handler.Refuse("worker-install-failed",
			"the install of dedicated server %d as the cluster's worker failed at %s, and the operator's log says why; the cluster's keys it needs are gone, so unsubscribe and subscribe again",
			c.req.params.robotServer, workerProgressAt(network))
	case WorkerRunning:
		if !c.installing {
			return handler.Refuse("worker-install-lost",
				"the install of dedicated server %d began at %s, but the operator restarted during it, and the cluster's keys it needs went with it; unsubscribe and subscribe again",
				c.req.params.robotServer, workerProgressAt(network))
		}
		return nil
	case WorkerWritten:
		return nil
	}
	return c.robotTarget().admits(ctx)
}

const robotLimitDetail = "Robot answered 403 RATE_LIMIT_EXCEEDED, so its hourly request limit is reached; " +
	"the operator changes nothing in Robot until a later call reads it again"

// WaitOutRobotLimit answers a Robot refusal past its request limit as a wait, since it passes once the hour's allowance refills.
func WaitOutRobotLimit(err error) error {
	if errors.Is(err, robot.ErrRateLimited) {
		return handler.Wait("%s", robotLimitDetail)
	}
	return err
}

// robotTarget is the subscription's dedicated server and the names it and its vSwitch carry.
type robotTarget struct {
	robot  *robot.Client
	server int64
	name   string
	plane  controlplane.Plane
}

func (c convergence) robotTarget() robotTarget {
	return robotTarget{robot: c.req.robot, server: c.req.params.robotServer, name: RobotName(c.req.subID), plane: c.plane}
}

// admits refuses a dedicated server that is missing, cancelled, outside the cluster's locations or adopted by another
// subscription, before the control plane's install renders the keys the worker is installed with.
func (t robotTarget) admits(ctx context.Context) error {
	server, err := t.robot.GetServer(ctx, t.server)
	switch {
	case robot.ErrorCode(err) == "SERVER_NOT_FOUND":
		return handler.Refuse("server-not-found", "Robot holds no dedicated server %d in this account, so nothing was changed", t.server)
	case err != nil:
		return fmt.Errorf("reading dedicated server %d: %w", t.server, err)
	case server.Cancelled:
		return handler.Refuse("server-cancelled", "dedicated server %d is cancelled, so no worker is installed over it and nothing was changed", t.server)
	case !slices.Contains(robot.Locations, server.Location()):
		return handler.Refuse("location-unsupported",
			"dedicated server %d is in %s, and a worker runs only in fsn1, nbg1 or hel1, where its vSwitch reaches the cluster's Cloud network, so nothing was changed", t.server, server.DC)
	case server.Name != t.name && strings.HasPrefix(server.Name, robot.NamePrefix):
		return handler.Refuse("server-taken", "dedicated server %d is named %s, the name of another mitstack subscription, so nothing was changed", t.server, server.Name)
	}
	return nil
}

// robotState is what Robot holds of the worker: its server's name, the vSwitch named for the subscription with
// whether the server is on it, and the server's firewall.
type robotState struct {
	server    robot.Server
	vswitch   robot.VSwitch
	vswitches int
	onSwitch  bool
	firewall  robot.Firewall
}

func (t robotTarget) read(ctx context.Context) (robotState, error) {
	var st robotState
	var err error
	if st.server, err = t.robot.GetServer(ctx, t.server); err != nil {
		return st, fmt.Errorf("reading dedicated server %d: %w", t.server, err)
	}
	all, err := t.robot.ListVSwitches(ctx)
	if err != nil {
		return st, fmt.Errorf("listing vSwitches: %w", err)
	}
	named := slices.DeleteFunc(all, func(v robot.VSwitch) bool { return v.Name != t.name || v.Cancelled })
	if st.vswitches = len(named); st.vswitches == 1 {
		if st.vswitch, err = t.robot.GetVSwitch(ctx, named[0].ID); err != nil {
			return st, fmt.Errorf("reading vSwitch %d: %w", named[0].ID, err)
		}
		st.onSwitch = slices.ContainsFunc(st.vswitch.Servers, func(s robot.VSwitchServer) bool { return s.Number == t.server })
	}
	if st.server.Name == t.name {
		if st.firewall, err = t.robot.GetFirewall(ctx, t.server); err != nil {
			return st, fmt.Errorf("reading the firewall of dedicated server %d: %w", t.server, err)
		}
	}
	return st, nil
}

func (t robotTarget) firewall() robot.Firewall { return WorkerFirewallFor(t.plane, t.server) }

// WorkerFirewallFor is the firewall the worker on dedicated server number server holds in the cluster of plane p.
func WorkerFirewallFor(p controlplane.Plane, server int64) robot.Firewall {
	return WorkerFirewall(netip.MustParsePrefix(controlplane.NetworkIPRange), workerFor(p, server))
}

// HoldsWorkerRules reports whether the firewall is active and admits the cluster's network.
func HoldsWorkerRules(fw robot.Firewall, want robot.Firewall) bool {
	if fw.Status != "active" {
		return false
	}
	for _, rule := range want.Input {
		if !slices.ContainsFunc(fw.Input, func(r robot.Rule) bool {
			return r.Name == rule.Name && r.SrcIP == rule.SrcIP && r.Action == rule.Action
		}) {
			return false
		}
	}
	return true
}

// repairStep puts back the one Robot resource a written worker lost that it can put back unchanged: the server's
// name, its place on the vSwitch, or its firewall. A cancelled vSwitch takes the worker's VLAN with it, so it refuses.
func (t robotTarget) repairStep(ctx context.Context) (controlplane.Step, bool, error) {
	st, err := t.read(ctx)
	if err != nil {
		return controlplane.Step{}, false, err
	}
	switch {
	case st.server.Name != t.name:
		return controlplane.Step{What: fmt.Sprintf("name dedicated server %d %s again", t.server, t.name), Run: func(ctx context.Context) error {
			_, err := t.robot.Adopt(ctx, t.server, t.name)
			return err
		}}, true, nil
	case st.vswitches == 0:
		return controlplane.Step{}, false, handler.Refuse("vswitch-lost",
			"no vSwitch named %s is left, and the worker on dedicated server %d boots on its VLAN alone, so nothing was changed; unsubscribe and subscribe again", t.name, t.server)
	case st.vswitches > 1:
		return controlplane.Step{}, false, handler.Refuse("ambiguous-vswitch", "%d vSwitches are named %s, so nothing was changed; cancel the extra ones in Robot and retry", st.vswitches, t.name)
	case !st.onSwitch:
		return controlplane.Step{What: fmt.Sprintf("put dedicated server %d back on vSwitch %d", t.server, st.vswitch.ID), Run: func(ctx context.Context) error {
			_, err := t.robot.Couple(ctx, t.name, t.server)
			return err
		}}, true, nil
	case !HoldsWorkerRules(st.firewall, t.firewall()):
		return controlplane.Step{What: fmt.Sprintf("write the worker firewall of dedicated server %d again", t.server), Run: func(ctx context.Context) error {
			return t.robot.WriteFirewall(ctx, t.server, t.firewall())
		}}, true, nil
	}
	return controlplane.Step{}, false, nil
}

// release takes the server off the subscription's vSwitch, cancels the vSwitch, and releases the server without
// cancelling it, which no Robot call this block makes can do.
func (t robotTarget) release(ctx context.Context) error {
	all, err := t.robot.ListVSwitches(ctx)
	if err != nil {
		return fmt.Errorf("listing vSwitches: %w", err)
	}
	for _, v := range all {
		if v.Name != t.name || v.Cancelled {
			continue
		}
		if err := t.robot.Uncouple(ctx, v.ID, t.server); err != nil {
			return fmt.Errorf("taking dedicated server %d off vSwitch %d: %w", t.server, v.ID, err)
		}
		if err := t.robot.CancelVSwitch(ctx, v.ID); err != nil {
			return fmt.Errorf("cancelling vSwitch %d: %w", v.ID, err)
		}
	}
	return t.robot.Release(ctx, t.server, t.name)
}

// imageCloud places the worker's image server in the cluster's Cloud network, beside the gateway it routes through.
type imageCloud struct {
	plane   controlplane.Plane
	network int64
	robot   *robot.Client
	vswitch string
	// poll is how long the image server's creation waits between reads of the gateway.
	poll time.Duration
}

// CreateServer couples the vSwitch's subnet to the network first, so the server reaches the worker, and waits
// for the gateway to hold its address, since a server joining before it would take that address.
func (c imageCloud) CreateServer(ctx context.Context, s robot.ShortLivedServer) (int64, error) {
	if err := c.coupleSubnet(ctx); err != nil {
		return 0, err
	}
	firewall, err := c.awaitGateway(ctx)
	if err != nil {
		return 0, err
	}
	serverType, err := (&controlplane.OrderableTypes{API: c.plane.API}).First(ctx, string(RoleImageServer), c.plane.ServerTypes, s.Location)
	if err != nil {
		return 0, err
	}
	created, err := c.plane.API.CreateServers(ctx, controlplane.ServerSpec{
		Name: s.Name, ServerType: serverType, Location: s.Location, Image: s.Image, UserData: s.UserData,
		Labels: c.plane.Labels(RoleImageServer), NetworkIDs: []int64{c.network}, FirewallIDs: []int64{firewall},
	})
	if err != nil {
		return 0, err
	}
	return created[0].ID, nil
}

func (c imageCloud) coupleSubnet(ctx context.Context) error {
	all, err := c.robot.ListVSwitches(ctx)
	if err != nil {
		return fmt.Errorf("listing vSwitches: %w", err)
	}
	i := slices.IndexFunc(all, func(v robot.VSwitch) bool { return v.Name == c.vswitch && !v.Cancelled })
	if i < 0 {
		return fmt.Errorf("no vSwitch named %s is left to couple to network %d", c.vswitch, c.network)
	}
	return c.plane.API.AddVSwitchSubnet(ctx, c.network, all[i].ID, workerSubnet)
}

func (c imageCloud) awaitGateway(ctx context.Context) (int64, error) {
	for {
		st, err := c.plane.Read(ctx)
		if err != nil {
			return 0, err
		}
		gw, gwOK := st.GatewayIn(controlplane.GatewayLocation)
		fw, fwOK := st.Firewall()
		if gwOK && fwOK && gw.PrivateIPs[c.network] == controlplane.GatewayAddress {
			return fw.ID, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("the gateway never took %s on network %d, so the image server was not created: %w", controlplane.GatewayAddress, c.network, ctx.Err())
		case <-time.After(c.poll):
		}
	}
}

func (c imageCloud) ServerAddress(ctx context.Context, id int64) (netip.Addr, error) {
	servers, err := c.plane.API.ListServers(ctx, c.plane.RoleSelector(RoleImageServer))
	if err != nil {
		return netip.Addr{}, err
	}
	i := slices.IndexFunc(servers, func(s controlplane.Server) bool { return s.ID == id })
	if i < 0 {
		return netip.Addr{}, fmt.Errorf("image server %d carries this subscription's labels no more", id)
	}
	return netip.ParseAddr(servers[i].PrivateIPs[c.network])
}

func (c imageCloud) DeleteServer(ctx context.Context, id int64) error {
	return c.plane.API.DeleteServer(ctx, id)
}
