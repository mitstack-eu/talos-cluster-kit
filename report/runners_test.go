package report_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	runnerOrg       = "acme-org"
	runnerScaleSet  = "mitstack-ci"
	runnerToken     = "ghs_leasedRunnerToken0123456789"
	runnerPodsRead  = "get pods -n mitstack-runners -o json"
	runnersListPath = "/orgs/" + runnerOrg + "/actions/runners"
	busyRefusal     = `Bad request - Runner "mitstack-ci-7xk2p-runner-q8w4z" is still running a job"`
)

// gitHubRunner is one self-hosted runner as GitHub's runner list answers it.
type gitHubRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// runnerAPI fakes GitHub's runner list and removal for the org, answering a removal of an id in refusals with that
// status and refusal as GitHub's message, and recording each call with its bearer.
type runnerAPI struct {
	mu         sync.Mutex
	runners    []gitHubRunner
	listStatus int
	refusals   map[int64]int
	refusal    string
	calls      []string
	bearers    []string
	url        string
}

func gitHubRunners(t *testing.T, runners ...gitHubRunner) *runnerAPI {
	t.Helper()
	api := &runnerAPI{runners: runners, listStatus: http.StatusOK, refusals: map[int64]int{}, refusal: busyRefusal}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		api.calls = append(api.calls, r.Method+" "+r.URL.RequestURI())
		api.bearers = append(api.bearers, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == runnersListPath:
			api.list(w, r)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, runnersListPath+"/"):
			api.remove(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message": "Not Found"}`)
		}
	}))
	t.Cleanup(server.Close)
	api.url = server.URL
	return api
}

func (api *runnerAPI) list(w http.ResponseWriter, r *http.Request) {
	if api.listStatus != http.StatusOK {
		w.WriteHeader(api.listStatus)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
		return
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	from := min(max(page-1, 0)*perPage, len(api.runners))
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(api.runners), "runners": api.runners[from:min(from+perPage, len(api.runners))]})
}

func (api *runnerAPI) remove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, runnersListPath+"/"), 10, 64)
	if status, refused := api.refusals[id]; err == nil && refused {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": api.refusal})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *runnerAPI) removals() []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(api.calls), func(call string) bool { return !strings.HasPrefix(call, http.MethodDelete) })
}

// sweeping is the environment the report's CronJob gives the sweep, with the leased token in a file and GitHub's API faked.
func sweeping(t *testing.T, api *runnerAPI) []string {
	t.Helper()
	token := filepath.Join(t.TempDir(), "github_token")
	require.NoError(t, os.WriteFile(token, []byte(runnerToken), 0o600))
	return []string{"RUNNERS_ORGANIZATION=" + runnerOrg, "RUNNER_SCALE_SET=" + runnerScaleSet, "RUNNER_TOKEN_FILE=" + token,
		"GITHUB_API_URL=" + api.url, "RUNNER_POD_GRACE_SECONDS=0"}
}

// withRunnerPods answers the runner namespace's pod read with a pod of each name.
func withRunnerPods(names ...string) map[string]string {
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		items = append(items, map[string]any{"metadata": map[string]any{"name": name, "namespace": components.RunnersNamespace},
			"status": map[string]any{"phase": "Running"}})
	}
	list, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	return map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerPodsRead: "echo '" + string(list) + "'"})}
}

func sweptMember(t *testing.T, posted *postedReport) map[string]any {
	t.Helper()
	member, ok := posted.body["runners"].(map[string]any)
	require.True(t, ok, "the report carries no runners member: %v", posted.body["runners"])
	return member
}

const (
	killedRunner   = "mitstack-ci-7xk2p-runner-q8w4z"
	startingRunner = "mitstack-ci-7xk2p-runner-m3v9d"
)

func TestReport_RemovesARunnerOfItsScaleSetThatGitHubListsOfflineWithNoPodBehindItAndKeepsEveryOtherRunner(t *testing.T) {
	cases := map[string]struct {
		runner  gitHubRunner
		pods    []string
		removed bool
	}{
		"a runner pod killed mid-job leaves its runner offline and busy with no pod, and the observe removes it": {
			gitHubRunner{ID: 11, Name: killedRunner, Status: "offline", Busy: true}, nil, true},
		"a runner whose pod ended after its job leaves it offline and idle with no pod, and it is removed": {
			gitHubRunner{ID: 12, Name: killedRunner, Status: "offline"}, []string{"mitstack-ci-7xk2p-listener"}, true},
		"a runner starting up is offline with its pod, and it is kept": {
			gitHubRunner{ID: 13, Name: startingRunner, Status: "offline"}, []string{startingRunner}, false},
		"a runner online with its pod is kept": {
			gitHubRunner{ID: 14, Name: startingRunner, Status: "online", Busy: true}, []string{startingRunner}, false},
		"a runner GitHub still lists online after its pod went is kept until GitHub reads it offline": {
			gitHubRunner{ID: 15, Name: killedRunner, Status: "online"}, nil, false},
		"an offline runner of another scale set with no pod is kept": {
			gitHubRunner{ID: 16, Name: "other-ci-7xk2p-runner-q8w4z", Status: "offline"}, nil, false},
		"an offline runner named after the scale set but not as ARC names a runner is kept": {
			gitHubRunner{ID: 17, Name: runnerScaleSet + "-laptop", Status: "offline"}, nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, tc.runner)

			_, posted, out, err := runReport(t, withRunnerPods(tc.pods...), sweeping(t, api)...)

			require.NoError(t, err, out)
			want := map[string]any{"scaleSet": runnerScaleSet, "listed": float64(1), "removed": []any{}, "unremoved": []any{}, "deferred": float64(0)}
			if !strings.HasPrefix(tc.runner.Name, runnerScaleSet+"-7xk2p-runner-") {
				want["listed"] = float64(0)
			}
			removals := []string{}
			if tc.removed {
				want["removed"] = []any{tc.runner.Name}
				removals = []string{fmt.Sprintf("DELETE %s/%d", runnersListPath, tc.runner.ID)}
			}
			assert.Equal(t, removals, api.removals(), "the removals the sweep asked GitHub for")
			assert.Equal(t, want, sweptMember(t, posted))
			for _, bearer := range api.bearers {
				assert.Equal(t, "Bearer "+runnerToken, bearer, "each GitHub call carries the leased runner token")
			}
			assert.NotContains(t, string(posted.raw)+out, runnerToken, "the leased token reaches neither the platform nor the log")
		})
	}
}

func TestReport_ARemovalGitHubRefusesLeavesTheRunnerUnremovedNamingItWithGitHubsStatusAndMessageAndTheRestAreRemoved(t *testing.T) {
	api := gitHubRunners(t, gitHubRunner{ID: 21, Name: killedRunner, Status: "offline", Busy: true},
		gitHubRunner{ID: 22, Name: "mitstack-ci-7xk2p-runner-h5j6k", Status: "offline"})
	api.refusals[21] = http.StatusUnprocessableEntity
	api.refusal = busyRefusal + strings.Repeat(" and more", 40)

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, "a refused removal is a reading of the report, which still posts: %s", out)
	assert.ElementsMatch(t, []string{"DELETE " + runnersListPath + "/21", "DELETE " + runnersListPath + "/22"}, api.removals())
	assert.Equal(t, map[string]any{"scaleSet": runnerScaleSet, "listed": float64(2), "deferred": float64(0),
		"removed": []any{"mitstack-ci-7xk2p-runner-h5j6k"},
		"unremoved": []any{map[string]any{"name": killedRunner, "id": float64(21), "status": float64(http.StatusUnprocessableEntity),
			"message": api.refusal[:256]}}}, sweptMember(t, posted), "the refused runner by name, with GitHub's status and its message cut to 256 characters")
}

func TestReport_RemovesNoRunnerAndNamesTheReadThatFailedWhenGitHubsListOrTheRunnerPodsReadFails(t *testing.T) {
	cases := map[string]struct {
		listStatus int
		fakes      map[string]string
		named      []string
	}{
		"GitHub refuses the leased token": {http.StatusUnauthorized, withRunnerPods(), []string{"401", "Bad credentials"}},
		"GitHub fails the list":           {http.StatusBadGateway, withRunnerPods(), []string{"502"}},
		"the runner pods read fails": {http.StatusOK, map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerPodsRead: failedRead})},
			[]string{"pods", "exit status 3", "the server is gone"}},
		"the runner pods read answers no pod list": {http.StatusOK, map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerPodsRead: "echo 'not json'"})},
			[]string{"pods"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, gitHubRunner{ID: 31, Name: killedRunner, Status: "offline"})
			api.listStatus = tc.listStatus

			_, posted, out, err := runReport(t, tc.fakes, append(sweeping(t, api), "REPORT_READ_SECONDS=2")...)

			require.NoError(t, err, "a failed sweep is a reading of the report, which still posts: %s", out)
			assert.Empty(t, api.removals(), "a sweep that cannot tell which runners have a pod removes none")
			member := sweptMember(t, posted)
			assert.Equal(t, []string{"scaleSet", "unread"}, slices.Sorted(maps.Keys(member)))
			for _, named := range tc.named {
				assert.Contains(t, member["unread"], named, "the reason names %q", named)
			}
			assert.NotRegexp(t, reportToken+`|`+runnerToken+`|Bearer|eyJ`, string(posted.raw), "no token reaches the platform")
		})
	}
}

func TestReport_SkipsTheSweepWithoutAnyGitHubCallUntilTheFirstRunnerTokenIsLeased(t *testing.T) {
	api := gitHubRunners(t, gitHubRunner{ID: 41, Name: killedRunner, Status: "offline"})
	env := sweeping(t, api)
	env = append(env, "RUNNER_TOKEN_FILE="+filepath.Join(t.TempDir(), "github_token"))

	_, posted, out, err := runReport(t, withRunnerPods(), env...)

	require.NoError(t, err, out)
	assert.Empty(t, api.calls, "the sweep calls GitHub only with a leased token")
	member := sweptMember(t, posted)
	assert.Equal(t, []string{"scaleSet", "skipped"}, slices.Sorted(maps.Keys(member)))
	assert.Contains(t, member["skipped"], "leased", "the reason says no token is leased yet")
}

func TestReport_ReadsEveryPageOfTheOrgsRunnersAndRemovesAtMostTenRunnersARunLeavingTheRestForTheNext(t *testing.T) {
	var runners []gitHubRunner
	for i := range 140 {
		runners = append(runners, gitHubRunner{ID: int64(100 + i), Name: fmt.Sprintf("other-ci-abcde-runner-%05d", i), Status: "online"})
	}
	for i := range 12 {
		runners = append(runners, gitHubRunner{ID: int64(500 + i), Name: fmt.Sprintf("mitstack-ci-abcde-runner-%05d", i), Status: "offline"})
	}
	api := gitHubRunners(t, runners...)

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, out)
	lists := slices.DeleteFunc(slices.Clone(api.calls), func(call string) bool { return !strings.HasPrefix(call, http.MethodGet) })
	assert.Equal(t, []string{"GET " + runnersListPath + "?per_page=100&page=1", "GET " + runnersListPath + "?per_page=100&page=2"}, lists,
		"one GET per page of 100, until a page holds fewer")
	assert.Len(t, api.removals(), 10, "at most ten removals a run")
	member := sweptMember(t, posted)
	assert.Equal(t, float64(12), member["listed"])
	assert.Len(t, member["removed"], 10)
	assert.Equal(t, float64(2), member["deferred"], "the two left wait for the next run")
}

func TestReport_CarriesNoRunnersMemberAndCallsNoGitHubWithoutTheSweepConfigured(t *testing.T) {
	_, posted, out, err := runReport(t, withRunnerPods(killedRunner))

	require.NoError(t, err, out)
	assert.NotContains(t, posted.body, "runners", "a cluster that runs no scale set has nothing to sweep")
}

func runnerReportInputs() report.Inputs {
	in := inputs(reportURL)
	in.Runners = &components.RunnerInputs{Organization: runnerOrg, Group: "mitstack-runners", ScaleSet: runnerScaleSet,
		RunnerImage: "ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300",
		DindImage:   "docker.io/library/docker:29.10.0-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c",
		MaxRunners:  4, LeaseResource: "github-runner-token"}
	return in
}

func TestManifests_GivesTheReportTheRunnerSweepsSettingsAndItsTokenAsAnOptionalMountOnlyWithRunnerInputs(t *testing.T) {
	with, err := report.Manifests(runnerReportInputs())
	require.NoError(t, err)

	job, _, container := cronJob(t, with, "mitstack-node-report")
	_, _, without := cronJob(t, manifests(t, reportURL), "mitstack-node-report")

	env := testutil.ContainerEnv(t, container)
	assert.Equal(t, runnerOrg, env["RUNNERS_ORGANIZATION"])
	assert.Equal(t, runnerScaleSet, env["RUNNER_SCALE_SET"])
	assert.Equal(t, "https://api.github.com", env["GITHUB_API_URL"])
	assert.Equal(t, "/var/run/mitstack-runner/github_token", env["RUNNER_TOKEN_FILE"])
	assert.Contains(t, testutil.Field(t, container, "volumeMounts"), map[string]any{"name": "runner-token", "mountPath": "/var/run/mitstack-runner", "readOnly": true})
	pod := testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec")
	assert.Contains(t, testutil.Field(t, pod, "volumes"), map[string]any{"name": "runner-token", "secret": map[string]any{
		"secretName": components.RunnerSweepTokenSecret, "optional": true, "defaultMode": 0o440}},
		"the Secret the lease job fills, optional so the report runs before the first lease")
	for _, name := range []string{"RUNNERS_ORGANIZATION", "RUNNER_SCALE_SET", "GITHUB_API_URL", "RUNNER_TOKEN_FILE"} {
		assert.NotContains(t, testutil.ContainerEnv(t, without), name, "a report without runner inputs sweeps nothing")
	}
	assert.NotContains(t, fmt.Sprint(testutil.Field(t, without, "volumeMounts")), "runner-token")
}

func TestManifests_GrantsTheReportTheSameRightsWithRunnerInputsAsWithout(t *testing.T) {
	with, err := report.Manifests(runnerReportInputs())
	require.NoError(t, err)

	rights := func(rendered []byte) []map[string]any {
		docs := testutil.Documents(t, rendered)
		return slices.Concat(testutil.Kind(docs, "ClusterRole"), testutil.Kind(docs, "Role"), testutil.Kind(docs, "ClusterRoleBinding"), testutil.Kind(docs, "RoleBinding"))
	}

	assert.Equal(t, rights(manifests(t, reportURL)), rights(with), "the sweep's token arrives as a mounted Secret, so the report gains no right")
}

func TestManifests_RefusesRunnerInputsNamingNoGitHubOrganisation(t *testing.T) {
	in := runnerReportInputs()
	in.Runners.Organization = "not an org"

	_, err := report.Manifests(in)

	require.ErrorContains(t, err, "Organization")
}
