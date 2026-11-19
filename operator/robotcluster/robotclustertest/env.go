// Package robotclustertest settles a robot cluster on fakes of every service it calls, for the kit and its operators.
package robotclustertest

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
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/noderollout"
	"github.com/mitstack-eu/talos-cluster-kit/operator/robotcluster"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/stretchr/testify/require"
)

const (
	// Block is the block id a cluster gets unless the test hands the harness an operator's own Config.
	Block = "test-robot-cluster"
	// App is the app the request names, whose cluster repository is Owner's App-cluster.
	App          = "shop"
	Owner        = "acme"
	hetznerToken = "hetzner-token-for-tests"
	githubToken  = "github-token-for-tests"
	egress       = "198.51.100.7"
	// rounds bounds the subscribes a cluster gets to read present, each a minute after the last.
	rounds = 240
)

// Start is the fake clock's time when a test begins.
var Start = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// Env is one subscription's cluster with every client it calls faked, its clock moved by the test.
type Env struct {
	T       *testing.T
	Cluster *robotcluster.Cluster
	Config  robotcluster.Config
	Cloud   *cloudtest.Fake
	Robot   *robottest.Fake
	GitHub  *GitHub
	Argo    *testutil.Cluster
	Now     time.Time
	Sub     string
	Params  map[string]string
	Own     map[string]string
	// NoRobotLogin sends the Hetzner connection with its Cloud token alone.
	NoRobotLogin bool
	// WorkerBuildHangs keeps the worker image's builder running, as a build that failed leaves it.
	WorkerBuildHangs bool
	Reports          []observe.NodeReport
	// WaitingPods is what each later node report says of the cluster's pods, as the report serves them.
	WaitingPods []map[string]any
	// Earlier names the nodes that run an earlier release's machine configuration until the release's rollout Job runs.
	Earlier map[string]string
	// EtcdDown names the node whose etcd member each later report reads unhealthy.
	EtcdDown string
	Created  []contract.CreatedResource

	mu       sync.Mutex
	queued   []func()
	installs int
	events   []string
}

// Option changes the Env before its cluster is built.
type Option func(*Env)

// WithParams adds params to the request.
func WithParams(params map[string]string) Option {
	return func(e *Env) { maps.Copy(e.Params, params) }
}

// WithOperator builds from an operator's own Config, its clients faked and its InstallWorker kept; it replaces earlier options.
func WithOperator(cfg robotcluster.Config) Option {
	return func(e *Env) { e.Config = e.faked(cfg) }
}

// New is subscription sub's Env, its cluster installing the dedicated server through the fakes.
func New(t *testing.T, sub string, opts ...Option) *Env {
	t.Helper()
	e := &Env{T: t, Now: Start, Sub: sub, Params: map[string]string{"appName": App}}
	e.Cloud = cloudtest.New(func() time.Time { return e.Now })
	e.Robot = robottest.New(t)
	e.GitHub = newGitHub(t)
	e.Argo = testutil.BuildCluster(t, components.HostingPolicies()[0].Bytes)
	t.Setenv(robotcluster.EgressEnv, egress)
	e.Config = e.faked(robotcluster.Config{Block: Block, Boundary: "test-boundary", InstallWorker: e.install, DeclaredSize: 3})
	for _, opt := range opts {
		opt(e)
	}
	e.Cluster = robotcluster.New(e.Config)
	return e
}

func (e *Env) faked(cfg robotcluster.Config) robotcluster.Config {
	cfg.Now = func() time.Time { return e.Now }
	cfg.CloudAPI = func(string) controlplane.API { return e.Cloud }
	cfg.RobotClient = func(string, string) *robot.Client { return e.Robot.Client() }
	cfg.GitHubClient = func(_, token string) *ghapi.Client {
		return ghapi.NewClient(e.GitHub.server.URL, token, e.GitHub.server.Client())
	}
	cfg.NodeReports = e.nodeReports
	cfg.StartJob = func(run func()) { with(&e.mu, func() { e.queued = append(e.queued, run) }) }
	cfg.ImageServerPoll = func() time.Duration { return time.Millisecond }
	cfg.EmitProgress = func(_ context.Context, detail string) error {
		with(&e.mu, func() { e.events = append(e.events, detail) })
		return nil
	}
	return cfg
}

func with(mu *sync.Mutex, f func()) {
	mu.Lock()
	defer mu.Unlock()
	f()
}

// Desired is the request the platform sends for the subscription now.
func (e *Env) Desired() contract.Desired {
	hetzner := map[string]string{"api_token": hetznerToken, "robot_user": robottest.User, "robot_password": robottest.Password}
	if e.NoRobotLogin {
		hetzner = map[string]string{"api_token": hetznerToken}
	}
	return contract.Desired{
		Identity:   contract.Identity{SubscriptionID: e.Sub},
		Params:     maps.Clone(e.Params),
		Secrets:    map[string]map[string]string{"hetzner": hetzner, "github": {"token": githubToken, "owner": Owner}},
		OwnSecrets: maps.Clone(e.Own),
	}
}

// Plane is the control plane of the subscription, read through no API.
func (e *Env) Plane() controlplane.Plane { return e.Cluster.PlaneFor(nil, e.Desired()) }

// ByRole is the servers of the subscription in role r that the Cloud fake holds.
func (e *Env) ByRole(r controlplane.Role) []controlplane.Server {
	return e.Cloud.Selected(e.T, e.Plane().RoleSelector(r))
}

// Ensure sends one subscribe, keeping the own secrets and resources an answer that does not fail carries.
func (e *Env) Ensure() error {
	created, own, err := e.Cluster.EnsureResourcesOwning(context.Background(), e.Desired())
	if err == nil || Waiting(err) {
		if e.Own == nil {
			e.Own = map[string]string{}
		}
		maps.Copy(e.Own, own)
		e.Created = created
	}
	return err
}

// Waiting is whether err asks the platform to call again.
func Waiting(err error) bool {
	var w *handler.Waiting
	return errors.As(err, &w)
}

// Observe reads the cluster, failing the test on an error.
func (e *Env) Observe() contract.Observation {
	e.T.Helper()
	obs, err := e.Cluster.Observe(context.Background(), e.Desired())
	require.NoError(e.T, err, "the observe")
	return obs
}

// Settle sends a subscribe a minute as the fakes progress, until a refusal or the cluster reads present.
func (e *Env) Settle() (contract.Observation, error) {
	e.T.Helper()
	for range rounds {
		if err := e.Ensure(); err != nil && !Waiting(err) {
			return e.Observe(), err
		}
		e.Now = e.Now.Add(time.Minute)
		e.Progress()
		if obs := e.Observe(); obs.State == contract.Present {
			return obs, nil
		}
	}
	e.T.Fatalf("the cluster read present after none of %d subscribes: %s", rounds, e.Observe().StatusDetail)
	return contract.Observation{}, nil
}

// Progress plays what the builders, the install, Argo CD and the nodes do while the platform waits.
func (e *Env) Progress() {
	e.Cloud.Advance(10*time.Minute, 3*time.Minute)
	if e.WorkerBuildHangs {
		for _, builder := range e.ByRole(controlplane.RoleWorkerBuilder) {
			e.Cloud.Servers[builder.ID].Status = controlplane.ServerRunning
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
	if len(e.ByRole(controlplane.RoleNode)) == len(e.Plane().NodeLocations()) {
		e.ReportNodes()
	}
}

func (e *Env) gatewayHolds() bool {
	gw := controlplane.ServersIn(e.ByRole(controlplane.RoleGateway), controlplane.GatewayLocation)
	networks, err := e.Cloud.ListNetworks(context.Background(), e.Plane().Selector())
	return err == nil && len(gw) == 1 && len(networks) == 1 && gw[0].PrivateIPs[networks[0].ID] == controlplane.GatewayAddress
}

func (e *Env) install(ctx context.Context, in robotcluster.WorkerInstall) error {
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

// Installed counts the installs of the dedicated server.
func (e *Env) Installed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.installs
}

func (e *Env) syncArgo() {
	files := map[string][]byte{}
	for _, name := range e.Argo.Files() {
		files[name] = nil
	}
	for name, content := range e.GitHub.RepoFiles(e.Repo()) {
		files[name] = []byte(content)
	}
	e.Argo.Commit(files)
	e.Argo.Sync()
}

// ReportNodes sends a report naming each node and Cloud worker Ready once running, the Robot worker once installed,
// each with the release its machine configuration names, each node's etcd member, and each gateway passing once it
// runs at its address.
func (e *Env) ReportNodes() {
	e.T.Helper()
	e.SendReport(e.Report())
}

// Report is the body ReportNodes sends now, for a test to change before SendReport sends it.
func (e *Env) Report() map[string]any {
	e.T.Helper()
	var nodes, members []map[string]any
	add := func(name string, ready bool) {
		nodes = append(nodes, map[string]any{"name": name, "ready": ready, "machineConfig": e.machineConfigOf(name)})
	}
	for _, s := range e.ByRole(controlplane.RoleNode) {
		add(s.Name, s.Status == controlplane.ServerRunning)
		members = append(members, map[string]any{"hostname": s.Name, "healthy": s.Status == controlplane.ServerRunning && s.Name != e.EtcdDown})
	}
	for _, s := range e.ByRole(controlplane.RoleCloudWorker) {
		add(s.Name, s.Status == controlplane.ServerRunning)
	}
	if e.Installed() > 0 {
		add(e.Plane().Name("worker"), true)
	}
	network := e.Cloud.OnlyNetwork(e.T, e.Plane().Selector())
	var gateways []map[string]any
	for _, loc := range e.Plane().NodeLocations() {
		addr := controlplane.GatewayAddresses[loc]
		up := slices.ContainsFunc(controlplane.ServersIn(e.ByRole(controlplane.RoleGateway), loc), func(gw controlplane.Server) bool {
			return gw.Status == controlplane.ServerRunning && gw.PrivateIPs[network.ID] == addr
		})
		gateways = append(gateways, map[string]any{"address": addr, "passed": up, "reason": "no answer within 5s"})
	}
	report := map[string]any{"subscriptionId": e.Sub, "nodes": nodes, "etcd": map[string]any{"members": members}, "gateways": gateways, "images": []any{}}
	if e.WaitingPods != nil {
		report["pods"] = e.WaitingPods
	}
	return report
}

// SendReport adds report to the node reports the cluster reads, received now.
func (e *Env) SendReport(report map[string]any) {
	e.T.Helper()
	body, err := json.Marshal(report)
	require.NoError(e.T, err)
	e.Reports = append(e.Reports, observe.NodeReport{ReceivedAt: e.Now, Body: body})
}

// Release is what a node's machine configuration names once it runs this kit release's.
func (e *Env) Release() string {
	e.T.Helper()
	release, err := noderollout.Release(tools.Image)
	require.NoError(e.T, err)
	return release
}

// machineConfigOf is the release node name runs: the earlier one it was set to until Argo CD runs the rollout Job.
func (e *Env) machineConfigOf(name string) string {
	job := testutil.ObjectRef{Group: "batch", Kind: "Job", Namespace: nodeconfig.TalosAPINamespace, Name: noderollout.JobName(e.Release())}
	if e.Argo.Object(job) != nil {
		delete(e.Earlier, name)
	}
	if earlier, ok := e.Earlier[name]; ok {
		return earlier
	}
	return e.Release()
}

func (e *Env) nodeReports(context.Context, contract.Desired, string) ([]observe.NodeReport, error) {
	var argocd struct{ ArgoCD json.RawMessage }
	if err := json.Unmarshal(e.Argo.Report(), &argocd); err != nil {
		return nil, err
	}
	out := make([]observe.NodeReport, 0, len(e.Reports))
	for _, r := range e.Reports {
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

// Repo is the owner and name of the cluster repository.
func (e *Env) Repo() string { return Owner + "/" + App + "-cluster" }

// Delete sends one unsubscribe.
func (e *Env) Delete() error {
	_, err := e.Cluster.Delete(context.Background(), e.Desired())
	return err
}

// EventsSoFar is every progress detail the cluster emitted.
func (e *Env) EventsSoFar() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// StepLog collects from the log each step a call took, without how long it took.
type StepLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *StepLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

var tookStep = regexp.MustCompile(`the step to (.*) (took|failed after) \S+$`)

// CaptureSteps sends the log to a StepLog until the test ends.
func CaptureSteps(t *testing.T) *StepLog {
	l := &StepLog{}
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(l)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return l
}

// Steps is each step taken, with whether it took or failed.
func (l *StepLog) Steps() []string {
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

// GitHub answers the repository, topics, contents, git and deploy key calls a cluster repository's handling makes.
type GitHub struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	repos   map[string][]string
	files   map[string]string
	keys    map[string][]robotcluster.DeployKey
	nextKey int64
	git     map[string]*landingtest.Fake
}

func newGitHub(t *testing.T) *GitHub {
	gh := &GitHub{t: t, repos: map[string][]string{}, files: map[string]string{}, keys: map[string][]robotcluster.DeployKey{},
		nextKey: 100, git: map[string]*landingtest.Fake{}}
	gh.server = httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(gh.server.Close)
	return gh
}

func (gh *GitHub) serve(w http.ResponseWriter, r *http.Request) {
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

func (gh *GitHub) serveTopics(w http.ResponseWriter, r *http.Request, repo string) {
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
func (gh *GitHub) serveContents(w http.ResponseWriter, r *http.Request, path string) {
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

// Plant writes content at path on repository repo's main, as a commit the cluster did not make.
func (gh *GitHub) Plant(repo, path, content string) {
	with(&gh.mu, func() { gh.files[repo+"/contents/"+path] = content })
}

// RepoFiles is each file of repository repo by its path.
func (gh *GitHub) RepoFiles(repo string) map[string]string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return gh.contentsOf(repo)
}

func (gh *GitHub) contentsOf(repo string) map[string]string {
	out := map[string]string{}
	for path, content := range gh.files {
		if name, ok := strings.CutPrefix(path, repo+"/contents/"); ok {
			out[name] = content
		}
	}
	return out
}

// serveGit keeps the git fake's main and the contents routes' files the same tree, both ways.
func (gh *GitHub) serveGit(w http.ResponseWriter, r *http.Request, repo string) {
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

func (gh *GitHub) serveKeys(w http.ResponseWriter, r *http.Request, path string) {
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

// Exists is whether repository repo exists.
func (gh *GitHub) Exists(repo string) bool {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	_, ok := gh.repos[repo]
	return ok
}

// LabellerBurstNodes is the burst nodes the labeller in the cluster repository names.
func (e *Env) LabellerBurstNodes() string {
	for path, content := range e.GitHub.RepoFiles(e.Repo()) {
		if !strings.Contains(path, "/mitstack-node-topology/") {
			continue
		}
		_, after, found := strings.Cut(content, "- name: TOPOLOGY_BURST_NODES\n")
		if !found {
			return ""
		}
		value, _, _ := strings.Cut(strings.TrimSpace(after), "\n")
		return value
	}
	e.T.Fatalf("the cluster repository holds no labeller")
	return ""
}

// EnsureOrWait subscribes once and fails the test on any error but a wait for the cluster.
func (e *Env) EnsureOrWait(call string) {
	e.T.Helper()
	if err := e.Ensure(); err != nil {
		require.True(e.T, Waiting(err), "%s: %v", call, err)
	}
}
