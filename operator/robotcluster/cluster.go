// Package robotcluster holds the behaviour of a Talos cluster whose control plane runs on Hetzner Cloud servers and
// whose workers run on one Robot dedicated server and a Cloud pool, so an operator keeps only its id, manifest and
// wiring.
package robotcluster

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/github/ghapi"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/events"
	"github.com/mitstack-eu/block-kit/helpers/reportproof"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

// PlatformURL is where every node enrols and reports, as an operator's manifest names the platform.
const PlatformURL = "https://api.mitstack.dev"

const defaultImageServerPoll = 15 * time.Second

// Config is what an operator hands the cluster: its block id and user data boundary, and each client the cluster calls,
// of which one left nil is the client a released operator uses.
type Config struct {
	// Block is the block's id, which labels every Cloud resource and marks the cluster repository.
	Block string
	// Boundary separates the parts of the user data a Cloud server boots with.
	Boundary string
	// InstallWorker writes the cluster's worker image onto the Robot server from its rescue system.
	InstallWorker func(ctx context.Context, install WorkerInstall) error
	// DeclaredSize is the one control-plane size the block's manifest lists servers for, since the platform waits on
	// every server it lists; zero takes 1 or 3.
	DeclaredSize int
	// DedicatedServerOptional lets a request whose robotServerNumber is empty run on Hetzner Cloud alone, its apps on
	// the Cloud pool.
	DedicatedServerOptional bool

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

// Cluster answers a block's lifecycle verbs for every subscription of one operator, keeping no state between calls but
// the worker installs its process runs and the renderings it has hashed.
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

func (cfg Config) withDefaults() Config {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CloudAPI == nil {
		cfg.CloudAPI = func(token string) controlplane.API { return controlplane.NewAPI(token, cfg.Block) }
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
	return cfg
}

// NewPlane is the control plane of subscription subID, read and changed through api.
func (k *Cluster) NewPlane(api controlplane.API, subID, appName string) controlplane.Plane {
	return controlplane.Plane{
		API:               api,
		Block:             k.cfg.Block,
		Subscription:      subID,
		AppName:           appName,
		ServerTypes:       []string{controlplane.DefaultServerType},
		Boundary:          k.cfg.Boundary,
		PlatformURL:       PlatformURL,
		ReportProofScript: reportproof.TokenRequestScript,
		Now:               k.cfg.Now,
		AppsOnWorkers:     true,
		WorkerAddresses:   []string{workerAddress.Addr().String()},
	}
}

// PlaneFor is NewPlane for the request's subscription and app, of the size the request names; a size the request cannot
// run reads as the default, since its own verb refuses it.
func (k *Cluster) PlaneFor(api controlplane.API, d contract.Desired) controlplane.Plane {
	p := k.placeApps(k.NewPlane(api, d.SubscriptionID, d.Params["appName"]), d)
	p.Size, _ = operator.ControlPlaneSize(d)
	return p
}

// cloudOnly is whether the request runs on Hetzner Cloud alone, naming no dedicated server to an operator that makes
// it optional.
func (k *Cluster) cloudOnly(d contract.Desired) bool {
	return k.cfg.DedicatedServerOptional && d.Params[ParamRobotServer] == ""
}

// placeApps points the ingress load balancer of a cluster with no dedicated server at the Cloud pool's workers.
func (k *Cluster) placeApps(p controlplane.Plane, d contract.Desired) controlplane.Plane {
	if k.cloudOnly(d) {
		p.WorkerAddresses, p.IngressTargetsCloudPool = nil, true
	}
	return p
}

// refuseUndeclaredSize refuses a size other than the one the block's manifest lists servers for, when it lists one.
func (k *Cluster) refuseUndeclaredSize(size int) error {
	if k.cfg.DeclaredSize == 0 {
		return nil
	}
	return operator.RefuseUndeclaredSize(size, k.cfg.DeclaredSize)
}

// RobotName keeps the whole subscription id, so no other subscription's name can match it. Robot holds no labels, so
// each Robot resource carries the subscription in its name.
func RobotName(subID string) string {
	return robot.NamePrefix + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, strings.ToLower(subID))
}

// installJobs holds each install the process runs, by subscription, so an unsubscribe can stop it and a later call can
// tell a running install from one a restart lost.
type installJobs struct {
	sync.Mutex
	running map[string]*workerJob
}
