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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	runnerOrg        = "acme-org"
	runnerScaleSet   = "mitstack-ci"
	runnerToken      = "ghs_leasedRunnerToken0123456789"
	runnerHoldsRead  = "get pods,ephemeralrunners.actions.github.com -n mitstack-runners -o json"
	runnersListPath  = "/orgs/" + runnerOrg + "/actions/runners"
	forbiddenRefusal = "Resource not accessible by integration"
)

// gitHubRunner is one self-hosted runner as GitHub's runner list answers it.
type gitHubRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// runnerAPI fakes GitHub's runner list and removal for the org, answering a removal of an id in refusals with that
// status and refusal as GitHub's message, and recording each call with its bearer and the most removals open at once.
type runnerAPI struct {
	mu         sync.Mutex
	runners    []gitHubRunner
	listStatus int
	refusals   map[int64]int
	refusal    string
	calls      []string
	bearers    []string
	removing   atomic.Int32
	mostAtOnce int32
	url        string
}

func gitHubRunners(t *testing.T, runners ...gitHubRunner) *runnerAPI {
	t.Helper()
	api := &runnerAPI{runners: runners, listStatus: http.StatusOK, refusals: map[int64]int{}, refusal: forbiddenRefusal}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.record(r)
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

func (api *runnerAPI) record(r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.calls = append(api.calls, r.Method+" "+r.URL.RequestURI())
	api.bearers = append(api.bearers, r.Header.Get("Authorization"))
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

// remove holds each removal open long enough that a second one sent at the same time overlaps it.
func (api *runnerAPI) remove(w http.ResponseWriter, r *http.Request) {
	open := api.removing.Add(1)
	defer api.removing.Add(-1)
	api.mu.Lock()
	api.mostAtOnce = max(api.mostAtOnce, open)
	api.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
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
		"GITHUB_API_URL=" + api.url}
}

// withRunnerPods answers the runner namespace's read with a pod of each name and no EphemeralRunner.
func withRunnerPods(names ...string) map[string]string {
	return withRunnerHolds(names, nil)
}

// withRunnerHolds answers the runner namespace's read with a pod of each name in pods and an EphemeralRunner of each
// name in ephemeral, the object ARC creates before it registers the runner of that name.
func withRunnerHolds(pods, ephemeral []string) map[string]string {
	items := make([]map[string]any, 0, len(pods)+len(ephemeral))
	for _, name := range pods {
		items = append(items, map[string]any{"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": name, "namespace": components.RunnersNamespace}, "status": map[string]any{"phase": "Running"}})
	}
	for _, name := range ephemeral {
		items = append(items, map[string]any{"apiVersion": "actions.github.com/v1alpha1", "kind": "EphemeralRunner",
			"metadata": map[string]any{"name": name, "namespace": components.RunnersNamespace}, "status": map[string]any{"phase": "Pending"}})
	}
	list, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	return map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: "echo '" + string(list) + "'"})}
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
		runner    gitHubRunner
		pods      []string
		ephemeral []string
		removed   bool
		waits     bool
	}{
		"a runner whose pod and EphemeralRunner went mid-job is offline and idle once GitHub fails the job, and it is removed": {
			runner: gitHubRunner{ID: 11, Name: killedRunner, Status: "offline"}, removed: true},
		"a runner GitHub still reads busy after its pod went mid-job waits for GitHub to fail the job, with no removal asked": {
			runner: gitHubRunner{ID: 12, Name: killedRunner, Status: "offline", Busy: true}, waits: true},
		"a runner whose pod ended after its job leaves it offline and idle with no pod, and it is removed": {
			runner: gitHubRunner{ID: 13, Name: killedRunner, Status: "offline"}, pods: []string{"mitstack-ci-7xk2p-listener"}, removed: true},
		"a runner starting up is offline with its pod, and it is kept": {
			runner: gitHubRunner{ID: 14, Name: startingRunner, Status: "offline"}, pods: []string{startingRunner}},
		"a runner ARC registered before it created the pod is offline with only its EphemeralRunner, and it is kept": {
			runner: gitHubRunner{ID: 15, Name: startingRunner, Status: "offline"}, ephemeral: []string{startingRunner}},
		"a runner online with its pod is kept": {
			runner: gitHubRunner{ID: 16, Name: startingRunner, Status: "online", Busy: true}, pods: []string{startingRunner}},
		"a runner GitHub still lists online after its pod went is kept until GitHub reads it offline": {
			runner: gitHubRunner{ID: 17, Name: killedRunner, Status: "online"}},
		"an offline runner of another scale set with no pod is kept": {
			runner: gitHubRunner{ID: 18, Name: "other-ci-7xk2p-runner-q8w4z", Status: "offline"}},
		"an offline runner named after the scale set but not as ARC names a runner is kept": {
			runner: gitHubRunner{ID: 19, Name: runnerScaleSet + "-laptop", Status: "offline"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, tc.runner)

			_, posted, out, err := runReport(t, withRunnerHolds(tc.pods, tc.ephemeral), sweeping(t, api)...)

			require.NoError(t, err, out)
			want := map[string]any{"scaleSet": runnerScaleSet, "listed": float64(1), "removed": []any{}, "unremoved": []any{},
				"deferred": float64(0), "unlisted": float64(0)}
			if !strings.HasPrefix(tc.runner.Name, runnerScaleSet+"-7xk2p-runner-") {
				want["listed"] = float64(0)
			}
			removals := []string{}
			if tc.removed {
				want["removed"] = []any{tc.runner.Name}
				removals = []string{fmt.Sprintf("DELETE %s/%d", runnersListPath, tc.runner.ID)}
			}
			if tc.waits {
				want["deferred"] = float64(1)
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
	api := gitHubRunners(t, gitHubRunner{ID: 21, Name: killedRunner, Status: "offline"},
		gitHubRunner{ID: 22, Name: "mitstack-ci-7xk2p-runner-h5j6k", Status: "offline"})
	api.refusals[21] = http.StatusForbidden
	api.refusal = forbiddenRefusal + strings.Repeat(" and more", 40)

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, "a refused removal is a reading of the report, which still posts: %s", out)
	assert.ElementsMatch(t, []string{"DELETE " + runnersListPath + "/21", "DELETE " + runnersListPath + "/22"}, api.removals())
	assert.Equal(t, map[string]any{"scaleSet": runnerScaleSet, "listed": float64(2), "deferred": float64(0), "unlisted": float64(0),
		"removed": []any{"mitstack-ci-7xk2p-runner-h5j6k"},
		"unremoved": []any{map[string]any{"name": killedRunner, "id": float64(21), "status": float64(http.StatusForbidden),
			"message": api.refusal[:256]}}}, sweptMember(t, posted), "the refused runner by name, with GitHub's status and its message cut to 256 characters")
}

func TestReport_CountsARunnerGitHubAnswersNotFoundForAsRemovedSinceGitHubNoLongerListsIt(t *testing.T) {
	api := gitHubRunners(t, gitHubRunner{ID: 23, Name: killedRunner, Status: "offline"})
	api.refusals[23] = http.StatusNotFound
	api.refusal = "Not Found"

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, out)
	assert.Equal(t, []string{"DELETE " + runnersListPath + "/23"}, api.removals())
	member := sweptMember(t, posted)
	assert.Equal(t, []any{killedRunner}, member["removed"], "ARC removed it between the list and the removal")
	assert.Equal(t, []any{}, member["unremoved"])
}

func TestReport_RemovesNoRunnerAndNamesTheReadThatFailedWhenGitHubsListOrTheRunnerNamespacesReadFails(t *testing.T) {
	cases := map[string]struct {
		listStatus int
		fakes      map[string]string
		named      []string
	}{
		"GitHub refuses the leased token": {http.StatusUnauthorized, withRunnerPods(), []string{"401", "Bad credentials"}},
		"GitHub fails the list":           {http.StatusBadGateway, withRunnerPods(), []string{"502"}},
		"the read of the runner pods and EphemeralRunners fails": {http.StatusOK,
			map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: failedRead})},
			[]string{"pods", "EphemeralRunners", "exit status 3", "the server is gone"}},
		"the read of the runner pods and EphemeralRunners answers no list": {http.StatusOK,
			map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: "echo 'not json'"})},
			[]string{"pods", "EphemeralRunners"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, gitHubRunner{ID: 31, Name: killedRunner, Status: "offline"})
			api.listStatus = tc.listStatus

			_, posted, out, err := runReport(t, tc.fakes, append(sweeping(t, api), "REPORT_READ_SECONDS=2")...)

			require.NoError(t, err, "a failed sweep is a reading of the report, which still posts: %s", out)
			assert.Empty(t, api.removals(), "a sweep that cannot tell which runners ARC still holds removes none")
			member := sweptMember(t, posted)
			assert.Equal(t, []string{"scaleSet", "unread"}, slices.Sorted(maps.Keys(member)))
			for _, named := range tc.named {
				assert.Contains(t, member["unread"], named, "the reason names %q", named)
			}
			assert.NotRegexp(t, reportToken+`|`+runnerToken+`|Bearer|eyJ`, string(posted.raw), "no token reaches the platform")
		})
	}
}

func TestReport_ReadsASweepThatStopsBeforeItFinishesAsUnreadRatherThanClean(t *testing.T) {
	api := gitHubRunners(t, gitHubRunner{ID: 32, Name: killedRunner, Status: "offline"})

	_, posted, out, err := runReport(t, withRunnerPods(), append(sweeping(t, api), "GITHUB_API_URL=")...)

	require.NoError(t, err, "a sweep that stops is a reading of the report, which still posts: %s", out)
	assert.Empty(t, api.calls)
	member := sweptMember(t, posted)
	assert.Equal(t, []string{"scaleSet", "unread"}, slices.Sorted(maps.Keys(member)))
	assert.Contains(t, member["unread"], "ended before it finished")
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

func TestReport_ReadsEveryPageOfTheOrgsRunnersAndRemovesAtMostTenRunnersARunOneAtATimeLeavingTheRestForTheNext(t *testing.T) {
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
	assert.Equal(t, int32(1), api.mostAtOnce, "one removal at a time, as GitHub asks of a client")
	member := sweptMember(t, posted)
	assert.Equal(t, float64(12), member["listed"])
	assert.Len(t, member["removed"], 10)
	assert.Equal(t, float64(2), member["deferred"], "the two left wait for the next run")
	assert.Equal(t, float64(0), member["unlisted"])
}

func TestReport_ReadsAtMostThreePagesAndCountsTheRunnersPastThemAsUnlisted(t *testing.T) {
	var runners []gitHubRunner
	for i := range 305 {
		runners = append(runners, gitHubRunner{ID: int64(1000 + i), Name: fmt.Sprintf("other-ci-abcde-runner-%05d", i), Status: "online"})
	}
	runners[304] = gitHubRunner{ID: 1304, Name: killedRunner, Status: "offline"}
	api := gitHubRunners(t, runners...)

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, out)
	lists := slices.DeleteFunc(slices.Clone(api.calls), func(call string) bool { return !strings.HasPrefix(call, http.MethodGet) })
	assert.Len(t, lists, 3, "the sweep's GitHub calls stay few however many runners the org registers")
	assert.Empty(t, api.removals(), "a runner past the third page is never seen")
	member := sweptMember(t, posted)
	assert.Equal(t, float64(5), member["unlisted"], "the report says how many runners the sweep did not read")
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

func TestManifests_GrantsTheReportOnlyAListOfARCsEphemeralRunnersBeyondItsRightsWithoutRunnerInputs(t *testing.T) {
	rendered, err := report.Manifests(runnerReportInputs())
	require.NoError(t, err)
	with, without := testutil.Documents(t, rendered), testutil.Documents(t, manifests(t, reportURL))
	bindings := func(docs []map[string]any) []map[string]any {
		return slices.Concat(testutil.Kind(docs, "Role"), testutil.Kind(docs, "ClusterRoleBinding"), testutil.Kind(docs, "RoleBinding"))
	}
	rules := func(docs []map[string]any) []any {
		return testutil.Field(t, testutil.Named(t, testutil.Kind(docs, "ClusterRole"), "ClusterRole", "mitstack-node-report"), "rules").([]any)
	}

	assert.Equal(t, bindings(without), bindings(with), "the sweep's token arrives as a mounted Secret, so the report gains no Role")
	assert.Len(t, testutil.Kind(with, "ClusterRole"), len(testutil.Kind(without, "ClusterRole")))
	assert.Equal(t, append(rules(without),
		map[string]any{"apiGroups": []any{"actions.github.com"}, "resources": []any{"ephemeralrunners"}, "verbs": []any{"list"}}), rules(with),
		"the one read the sweep adds, of the objects ARC holds for each runner, which carry no Secret data")
}

func TestManifests_RefusesRunnerInputsNamingNoGitHubOrganisation(t *testing.T) {
	in := runnerReportInputs()
	in.Runners.Organization = "not an org"

	_, err := report.Manifests(in)

	require.ErrorContains(t, err, "Organization")
}
