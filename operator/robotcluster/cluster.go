// Package robotcluster holds a Talos cluster on Hetzner Cloud with a Cloud pool and a Robot server, so an operator keeps its id, manifest and wiring.
package robotcluster

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/github/ghapi"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/cloud"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/events"
	"github.com/mitstack-eu/block-kit/helpers/reportproof"
	"github.com/mitstack-eu/block-kit/helpers/reqcache"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const PlatformURL = "https://api.mitstack.dev"

const defaultImageServerPoll = 15 * time.Second

// Config is an operator's block id, user data boundary and settings, and each client the cluster calls, nil taking a released operator's.
type Config struct {
	Block         string
	Boundary      string
	InstallWorker func(ctx context.Context, install WorkerInstall) error
	// DeclaredSize is the one control-plane size the manifest lists servers for, since the platform waits on each; zero takes 1 or 3.
	DeclaredSize int
	// DedicatedServerOptional lets a request naming no robotServerNumber run on Hetzner Cloud alone, its apps on the Cloud pool.
	DedicatedServerOptional bool
	// ClusterType is the name and type the manifest declares the cluster under; empty takes ResourceCluster.
	ClusterType string
	// Runners names the runner services a request's cluster runs, its error refusing the request; nil runs none.
	Runners func(d contract.Desired) (*components.RunnerInputs, error)
	// GatewayEgressPorts are the TCP ports the gateway forwards to any outside address beside 443; nil forwards 443 alone.
	GatewayEgressPorts []int

	Now             func() time.Time
	CloudAPI        func(token string) controlplane.API
	RobotClient     func(user, password string) *robot.Client
	GitHubClient    func(base, token string) *ghapi.Client
	ToolsImage      func() string
	NodeReports     func(ctx context.Context, d contract.Desired, subscriptionID string) ([]observe.NodeReport, error)
	StartJob        func(run func())
	ImageServerPoll func() time.Duration
	EmitProgress    func(ctx context.Context, detail string) error
}

// Cluster keeps no state between calls but the worker installs its process runs and the renderings it has hashed.
type Cluster struct {
	cfg        Config
	jobs       installJobs
	renderings renderingCache
}

var (
	_ handler.OwnSecretsReporter = (*Cluster)(nil)
	_ handler.ReplacementPlanner = (*Cluster)(nil)
)

// New panics on a Config without a block id, a boundary or a worker install, since no operator can run without them.
func New(cfg Config) *Cluster {
	if cfg.Block == "" || cfg.Boundary == "" || cfg.InstallWorker == nil {
		panic("robotcluster: a Config needs its Block, its Boundary and its InstallWorker")
	}
	return &Cluster{cfg: cfg.withDefaults(), jobs: installJobs{running: map[string]*workerJob{}}, renderings: renderingCache{kept: map[renderingID]*Rendering{}}}
}

// CloudAPIAt is a released operator's Cloud API at endpoint, whose calls go through block-kit's client, which holds a
// token's calls once Hetzner refuses one for its rate limit, and whose action waits ask on block-kit's poll schedule.
func CloudAPIAt(endpoint, block string) func(token string) controlplane.API {
	calls := controlplane.Calls{HTTP: cloud.HTTPClient(), PollBackoff: cloud.PollBackoff}
	return func(token string) controlplane.API { return controlplane.NewAPIAt(endpoint, token, block, calls) }
}

func (cfg Config) withDefaults() Config {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CloudAPI == nil {
		cfg.CloudAPI = CloudAPIAt(controlplane.Endpoint, cfg.Block)
	}
	if cfg.RobotClient == nil {
		cfg.RobotClient = robot.New
	}
	if cfg.GitHubClient == nil {
		cfg.GitHubClient = clusterrepo.NewClient
	}
	if cfg.ToolsImage == nil {
		cfg.ToolsImage = func() string { return tools.Image }
	}
	if cfg.NodeReports == nil {
		cfg.NodeReports = PlatformReports{}.NodeReports
	}
	if cfg.StartJob == nil {
		cfg.StartJob = func(run func()) { go run() }
	}
	if cfg.ImageServerPoll == nil {
		cfg.ImageServerPoll = func() time.Duration { return defaultImageServerPoll }
	}
	if cfg.EmitProgress == nil {
		cfg.EmitProgress = events.EmitProgress
	}
	if cfg.ClusterType == "" {
		cfg.ClusterType = ResourceCluster
	}
	return cfg
}

func (k *Cluster) NewPlane(api controlplane.API, subID, appName string) controlplane.Plane {
	return controlplane.Plane{
		API:                controlplane.ReadOncePerRequest(api, requestReads),
		Block:              k.cfg.Block,
		Subscription:       subID,
		AppName:            appName,
		ServerTypes:        []string{controlplane.DefaultServerType},
		Boundary:           k.cfg.Boundary,
		PlatformURL:        PlatformURL,
		ReportProofScript:  reportproof.TokenRequestScript,
		Now:                k.cfg.Now,
		AppsOnWorkers:      true,
		WorkerAddresses:    []string{workerAddress.Addr().String()},
		GatewayEgressPorts: k.cfg.GatewayEgressPorts,
		// A successor boots the configuration its image was built with, which the rollout replaces.
		RollsOutMachineConfig: true,
	}
}

type requestReadsKey struct{}

// requestReadsMu gives the first calls of one request a single holder, since reqcache looks up and stores in two steps.
var requestReadsMu sync.Mutex

func requestReads(ctx context.Context) *controlplane.RequestReads {
	requestReadsMu.Lock()
	defer requestReadsMu.Unlock()
	reads, _ := reqcache.Immutable(ctx, requestReadsKey{}, func() (*controlplane.RequestReads, error) {
		return controlplane.NewRequestReads(), nil
	})
	return reads
}

// PlaneFor is d's plane with the runner services Config.Runners names for it, or the error Config.Runners returns.
func (k *Cluster) PlaneFor(api controlplane.API, d contract.Desired) (controlplane.Plane, error) {
	p := k.readPlane(api, d)
	runners, err := k.runnersFor(d)
	if err != nil {
		return controlplane.Plane{}, err
	}
	p.Runners = runners
	return p, nil
}

// readPlane is d's plane without its runner services or a size error, for a read that renders nothing, so a teardown
// never waits on Config.Runners and Ensure and PlanReplacements refuse a bad size themselves.
func (k *Cluster) readPlane(api controlplane.API, d contract.Desired) controlplane.Plane {
	p := k.placeApps(k.NewPlane(api, d.SubscriptionID, d.Params["appName"]), d)
	p.Size, _ = operator.ControlPlaneSize(d)
	return p
}

func (k *Cluster) runnersFor(d contract.Desired) (*components.RunnerInputs, error) {
	if k.cfg.Runners == nil {
		return nil, nil
	}
	return k.cfg.Runners(d)
}

func (k *Cluster) cloudOnly(d contract.Desired) bool {
	return k.cfg.DedicatedServerOptional && d.Params[ParamRobotServer] == ""
}

func (k *Cluster) placeApps(p controlplane.Plane, d contract.Desired) controlplane.Plane {
	if k.cloudOnly(d) {
		p.WorkerAddresses, p.IngressTargetsCloudPool = nil, true
	}
	return p
}

func (k *Cluster) refuseUndeclaredSize(size int) error {
	if k.cfg.DeclaredSize == 0 {
		return nil
	}
	return operator.RefuseUndeclaredSize(size, k.cfg.DeclaredSize)
}

// RobotName keeps the whole subscription id, since Robot holds no labels and no other subscription's name may match it.
func RobotName(subID string) string {
	return robot.NamePrefix + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, strings.ToLower(subID))
}

// installJobs lets an unsubscribe stop a running install and a later call tell one running from one a restart lost.
type installJobs struct {
	sync.Mutex
	running map[string]*workerJob
}
