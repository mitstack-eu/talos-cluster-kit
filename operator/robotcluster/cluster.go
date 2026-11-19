// Package robotcluster holds a Talos cluster on Hetzner Cloud with a Cloud pool and a Robot server, so an operator keeps its id, manifest and wiring.
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
	if cfg.ClusterType == "" {
		cfg.ClusterType = ResourceCluster
	}
	return cfg
}

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
		// A successor boots the configuration its image was built with, which the rollout replaces.
		RollsOutMachineConfig: true,
	}
}

// PlaneFor drops a size error, since Ensure and PlanReplacements refuse that size themselves.
func (k *Cluster) PlaneFor(api controlplane.API, d contract.Desired) controlplane.Plane {
	p := k.placeApps(k.NewPlane(api, d.SubscriptionID, d.Params["appName"]), d)
	p.Size, _ = operator.ControlPlaneSize(d)
	return p
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
