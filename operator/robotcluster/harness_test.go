package robotcluster_test

// The fakes a whole cluster settles against: Hetzner Cloud and Robot, GitHub, Argo CD, the node reports and the install.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/github/ghapi"
	"github.com/mitstack-eu/block-kit/capabilities/github/landing"
	"github.com/mitstack-eu/block-kit/capabilities/github/landing/landingtest"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/operator"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/require"
)

const (
	envApp     = "shop"
	envOwner   = "acme"
	envHetzner = "hetzner-token-for-tests"
	envGitHub  = "github-token-for-tests"
	envEgress  = "198.51.100.7"
	// envRounds bounds the subscribes a cluster gets to read present, each a minute after the last.
	envRounds = 240
)

var envStart = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

type env struct {
	t      *testing.T
	k      *robotcluster.Cluster
	cfg    robotcluster.Config
	cloud  *cloudtest.Fake
	robot  *robottest.Fake
	github *fakeGitHub
	argo   *testutil.Cluster
	now    time.Time
	sub    string
	params map[string]string
	own    map[string]string
	// noRobotLogin sends the Hetzner connection with its Cloud token alone.
	noRobotLogin bool
	// workerBuildHangs keeps the worker image's builder running, as a build that failed leaves it.
	workerBuildHangs bool
	reports          []observe.NodeReport
	created          []contract.CreatedResource

	mu       sync.Mutex
	queued   []func()
	installs int
	events   []string
}

type envOption func(*env)

func withParams(params map[string]string) envOption {
	return func(e *env) { maps.Copy(e.params, params) }
}

func newEnv(t *testing.T, sub string, opts ...envOption) *env {
	t.Helper()
	e := &env{t: t, now: envStart, sub: sub, params: map[string]string{"appName": envApp}}
	e.cloud = cloudtest.New(func() time.Time { return e.now })
	e.robot = robottest.New(t)
	e.github = newFakeGitHub(t)
	e.argo = testutil.BuildCluster(t, components.HostingPolicies()[0].Bytes)
	t.Setenv(robotcluster.EgressEnv, envEgress)
	e.cfg = robotcluster.Config{
		Block: testBlock, Boundary: "test-boundary", InstallWorker: e.install, DeclaredSize: 3,
		Now:         func() time.Time { return e.now },
		CloudAPI:    func(string) controlplane.API { return e.cloud },
		RobotClient: func(string, string) *robot.Client { return e.robot.Client() },
		GitHubClient: func(_, token string) *ghapi.Client {
			return ghapi.NewClient(e.github.server.URL, token, e.github.server.Client())
		},
		NodeReports:     e.nodeReports,
		StartJob:        func(run func()) { with(&e.mu, func() { e.queued = append(e.queued, run) }) },
		ImageServerPoll: func() time.Duration { return time.Millisecond },
		EmitProgress: func(_ context.Context, detail string) error {
			with(&e.mu, func() { e.events = append(e.events, detail) })
			return nil
		},
	}
	for _, opt := range opts {
		opt(e)
	}
	e.k = robotcluster.New(e.cfg)
	return e
}

func with(mu *sync.Mutex, f func()) {
	mu.Lock()
	defer mu.Unlock()
	f()
}

func (e *env) desired() contract.Desired {
	hetzner := map[string]string{"api_token": envHetzner, "robot_user": robottest.User, "robot_password": robottest.Password}
	if e.noRobotLogin {
		hetzner = map[string]string{"api_token": envHetzner}
	}
	return contract.Desired{
		Identity:   contract.Identity{SubscriptionID: e.sub},
		Params:     maps.Clone(e.params),
		Secrets:    map[string]map[string]string{"hetzner": hetzner, "github": {"token": envGitHub, "owner": envOwner}},
		OwnSecrets: maps.Clone(e.own),
	}
}

func (e *env) plane() controlplane.Plane { return e.k.PlaneFor(nil, e.desired()) }

func (e *env) byRole(r controlplane.Role) []controlplane.Server {
	return e.cloud.Selected(e.t, e.plane().RoleSelector(r))
}

// ensure sends one subscribe, keeping the own secrets and resources an answer that does not fail carries.
func (e *env) ensure() error {
	created, own, err := e.k.EnsureResourcesOwning(context.Background(), e.desired())
	if err == nil || waiting(err) {
		if e.own == nil {
			e.own = map[string]string{}
		}
		maps.Copy(e.own, own)
		e.created = created
	}
	return err
}

func waiting(err error) bool {
	var w *handler.Waiting
	return errors.As(err, &w)
}

func (e *env) observe() contract.Observation {
	e.t.Helper()
	obs, err := e.k.Observe(context.Background(), e.desired())
	require.NoError(e.t, err, "the observe")
	return obs
}

// settle sends a subscribe a minute while Hetzner, the install and the nodes progress, and stops at the first refusal
// or once the cluster reads present.
func (e *env) settle() (contract.Observation, error) {
	e.t.Helper()
	for range envRounds {
		if err := e.ensure(); err != nil && !waiting(err) {
			return e.observe(), err
		}
		e.now = e.now.Add(time.Minute)
		e.progress()
		if obs := e.observe(); obs.State == contract.Present {
			return obs, nil
		}
	}
	e.t.Fatalf("the cluster read present after none of %d subscribes: %s", envRounds, e.observe().StatusDetail)
	return contract.Observation{}, nil
}

// progress plays what the builders, the install, Argo CD and the nodes do while the platform waits.
func (e *env) progress() {
	e.cloud.Advance(10*time.Minute, 3*time.Minute)
	if e.workerBuildHangs {
		for _, builder := range e.byRole(controlplane.RoleWorkerBuilder) {
			e.cloud.Servers[builder.ID].Status = controlplane.ServerRunning
		}
	}
	if e.gatewayHolds() {
		var queued []func()
		with(&e.mu, func() { queued, e.queued = e.queued, nil })
		for _, run := range queued {
			run()
		}
	}
	e.syncArgo()
	if len(e.byRole(controlplane.RoleNode)) == len(e.plane().NodeLocations()) {
		e.reportNodes()
	}
}

func (e *env) gatewayHolds() bool {
	gw := controlplane.ServersIn(e.byRole(controlplane.RoleGateway), controlplane.GatewayLocation)
	networks, err := e.cloud.ListNetworks(context.Background(), e.plane().Selector())
	return err == nil && len(gw) == 1 && len(networks) == 1 && gw[0].PrivateIPs[networks[0].ID] == controlplane.GatewayAddress
}

// install does to Robot and Cloud what the rescue install does around the image write.
func (e *env) install(ctx context.Context, in robotcluster.WorkerInstall) error {
	with(&e.mu, func() { e.installs++ })
	w := in.Worker
	server, err := in.Robot.Adopt(ctx, w.Server, w.Name)
	if err != nil {
		return err
	}
	if _, err := in.Robot.Couple(ctx, w.VSwitch, w.Server); err != nil {
		return err
	}
	if err := in.Robot.WriteFirewall(ctx, w.Server, robotcluster.WorkerFirewall(in.Cluster.Network, w)); err != nil {
		return err
	}
	id, err := in.Cloud.CreateServer(ctx, robot.ShortLivedServer{Name: w.Name + "-image", Location: server.Location(), Image: "ubuntu-24.04"})
	if err != nil {
		return err
	}
	if _, err := in.Cloud.ServerAddress(ctx, id); err != nil {
		return err
	}
	return in.Cloud.DeleteServer(ctx, id)
}

func (e *env) installed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.installs
}

// syncArgo syncs Argo CD to the repository's files, a deleted one included.
func (e *env) syncArgo() {
	files := map[string][]byte{}
	for _, name := range e.argo.Files() {
		files[name] = nil
	}
	for name, content := range e.github.repoFiles(e.repo()) {
		files[name] = []byte(content)
	}
	e.argo.Commit(files)
	e.argo.Sync()
}

// reportNodes sends a report naming each node and Cloud worker Ready once running, the Robot worker once installed,
// and each gateway passing once it runs at its address.
func (e *env) reportNodes() {
	e.t.Helper()
	var nodes []operator.ReportedNode
	for _, s := range e.byRole(controlplane.RoleNode) {
		nodes = append(nodes, operator.ReportedNode{Name: "node-" + s.Location, Ready: s.Status == controlplane.ServerRunning})
	}
	for _, s := range e.byRole(controlplane.RoleCloudWorker) {
		nodes = append(nodes, operator.ReportedNode{Name: s.Name, Ready: s.Status == controlplane.ServerRunning})
	}
	if e.installed() > 0 {
		nodes = append(nodes, operator.ReportedNode{Name: e.plane().Name("worker"), Ready: true})
	}
	network := e.cloud.OnlyNetwork(e.t, e.plane().Selector())
	var gateways []map[string]any
	for _, loc := range e.plane().NodeLocations() {
		addr := controlplane.GatewayAddresses[loc]
		up := slices.ContainsFunc(controlplane.ServersIn(e.byRole(controlplane.RoleGateway), loc), func(gw controlplane.Server) bool {
			return gw.Status == controlplane.ServerRunning && gw.PrivateIPs[network.ID] == addr
		})
		gateways = append(gateways, map[string]any{"address": addr, "passed": up, "reason": "no answer within 5s"})
	}
	body, err := json.Marshal(map[string]any{"subscriptionId": e.sub, "nodes": nodes, "gateways": gateways, "images": []any{}})
	require.NoError(e.t, err)
	e.reports = append(e.reports, observe.NodeReport{ReceivedAt: e.now, Body: body})
}

// nodeReports joins Argo CD's state now to each report, as the platform's node reports carry it.
func (e *env) nodeReports(context.Context, contract.Desired, string) ([]observe.NodeReport, error) {
	var argocd struct{ ArgoCD json.RawMessage }
	if err := json.Unmarshal(e.argo.Report(), &argocd); err != nil {
		return nil, err
	}
	out := make([]observe.NodeReport, 0, len(e.reports))
	for _, r := range e.reports {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(r.Body, &body); err != nil {
			return nil, err
		}
		body["argocd"] = argocd.ArgoCD
		r.Body, _ = json.Marshal(body)
		out = append(out, r)
	}
	return out, nil
}

func (e *env) repo() string { return envOwner + "/" + envApp + "-cluster" }

func (e *env) delete() error {
	_, err := e.k.Delete(context.Background(), e.desired())
	return err
}

func (e *env) eventsSoFar() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// stepLog collects from the log each step a call took, without how long it took.
type stepLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *stepLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

var tookStep = regexp.MustCompile(`the step to (.*) (took|failed after) \S+$`)

func captureSteps(t *testing.T) *stepLog {
	l := &stepLog{}
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(l)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return l
}

func (l *stepLog) steps() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var steps []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if m := tookStep.FindStringSubmatch(line); m != nil {
			steps = append(steps, m[1]+" ("+m[2]+")")
		}
	}
	return steps
}

// fakeGitHub answers the repository, topics, contents, git and deploy key calls a cluster repository's handling makes.
type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	repos   map[string][]string
	files   map[string]string
	keys    map[string][]robotcluster.DeployKey
	nextKey int64
	git     map[string]*landingtest.Fake
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	gh := &fakeGitHub{t: t, repos: map[string][]string{}, files: map[string]string{}, keys: map[string][]robotcluster.DeployKey{},
		nextKey: 100, git: map[string]*landingtest.Fake{}}
	gh.server = httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(gh.server.Close)
	return gh
}

func (gh *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/repos/")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/orgs/"):
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.repos[strings.Split(r.URL.Path, "/")[2]+"/"+body.Name] = []string{}
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(path, "/git/"):
		repo, _, _ := strings.Cut(path, "/git/")
		gh.serveGit(w, r, repo)
	case strings.HasSuffix(path, "/topics"):
		gh.serveTopics(w, r, strings.TrimSuffix(path, "/topics"))
	case strings.Contains(path, "/contents/"):
		gh.serveContents(w, r, path)
	case strings.Contains(path, "/keys"):
		gh.serveKeys(w, r, path)
	case r.Method == http.MethodDelete:
		delete(gh.repos, path)
		delete(gh.keys, path)
		maps.DeleteFunc(gh.files, func(file, _ string) bool { return strings.HasPrefix(file, path+"/") })
		w.WriteHeader(http.StatusNoContent)
	default:
		if _, ok := gh.repos[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"` + path + `"}`))
	}
}

func (gh *fakeGitHub) serveTopics(w http.ResponseWriter, r *http.Request, repo string) {
	if _, ok := gh.repos[repo]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPut {
		var body struct{ Names []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.repos[repo] = body.Names
	}
	_ = json.NewEncoder(w).Encode(map[string][]string{"names": gh.repos[repo]})
}

// serveContents refuses a write over an existing file that does not name its sha, as GitHub does.
func (gh *fakeGitHub) serveContents(w http.ResponseWriter, r *http.Request, path string) {
	content, ok := gh.files[path]
	if r.Method == http.MethodPut {
		var body struct{ Content, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if ok && body.SHA != landing.BlobHash([]byte(content)) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		decoded, _ := base64.StdEncoding.DecodeString(body.Content)
		gh.files[path] = string(decoded)
		w.WriteHeader(http.StatusCreated)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte(content)), "sha": landing.BlobHash([]byte(content))})
}

func (gh *fakeGitHub) repoFiles(repo string) map[string]string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return gh.contentsOf(repo)
}

func (gh *fakeGitHub) contentsOf(repo string) map[string]string {
	out := map[string]string{}
	for path, content := range gh.files {
		if name, ok := strings.CutPrefix(path, repo+"/contents/"); ok {
			out[name] = content
		}
	}
	return out
}

// serveGit answers from the repository's git fake, its main first moved to the files the contents routes left, and
// keeps what a fast-forward of main lands as those files.
func (gh *fakeGitHub) serveGit(w http.ResponseWriter, r *http.Request, repo string) {
	f, ok := gh.git[repo]
	if !ok {
		f = landingtest.New(gh.t, repo)
		gh.git[repo] = f
	}
	files, main := gh.contentsOf(repo), f.Main()
	if f.Head() == "" || !maps.Equal(files, main) {
		f.Remove(slices.DeleteFunc(slices.Collect(maps.Keys(main)), func(path string) bool { _, kept := files[path]; return kept })...)
		f.Seed(files)
	}
	rec := httptest.NewRecorder()
	f.Config.Handler.ServeHTTP(rec, r)
	if r.Method == http.MethodPatch && rec.Code == http.StatusOK {
		maps.DeleteFunc(gh.files, func(path, _ string) bool { return strings.HasPrefix(path, repo+"/contents/") })
		for path, content := range f.Main() {
			gh.files[repo+"/contents/"+path] = content
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (gh *fakeGitHub) serveKeys(w http.ResponseWriter, r *http.Request, path string) {
	repo, id, _ := strings.Cut(strings.TrimSuffix(strings.Split(path, "?")[0], "/"), "/keys")
	if _, ok := gh.repos[repo]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var k robotcluster.DeployKey
		_ = json.NewDecoder(r.Body).Decode(&k)
		gh.nextKey++
		k.ID = gh.nextKey
		gh.keys[repo] = append(gh.keys[repo], k)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(k)
	case http.MethodDelete:
		before := len(gh.keys[repo])
		gh.keys[repo] = slices.DeleteFunc(gh.keys[repo], func(k robotcluster.DeployKey) bool { return "/"+fmt.Sprint(k.ID) == id })
		if len(gh.keys[repo]) == before {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		_ = json.NewEncoder(w).Encode(append([]robotcluster.DeployKey{}, gh.keys[repo]...))
	}
}

func (gh *fakeGitHub) exists(repo string) bool {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	_, ok := gh.repos[repo]
	return ok
}
