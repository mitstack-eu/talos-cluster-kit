package report_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	runnerOrg        = "acme-org"
	runnerGroup      = "acme-ci-runners"
	runnerGroupID    = 7
	otherGroupID     = 9
	runnerScaleSet   = "mitstack-ci"
	runnerToken      = "ghs_leasedRunnerToken0123456789"
	runnerHoldsRead  = "get pods,ephemeralrunners.actions.github.com -n " + components.RunnersNamespace + " -o json"
	runnersPath      = "/orgs/" + runnerOrg + "/actions/runners"
	runnerGroupsPath = "/orgs/" + runnerOrg + "/actions/runner-groups"
	groupRunnersPath = runnerGroupsPath + "/7/runners"
	forbiddenRefusal = "Resource not accessible by integration"
	killedRunner     = "mitstack-ci-7xk2p-runner-q8w4z"
	startingRunner   = "mitstack-ci-7xk2p-runner-m3v9d"
)

// gitHubRunner is one runner as GitHub lists it, in the scale set's runner group unless RunnerGroupID names another.
type gitHubRunner struct {
	ID            int64  `json:"id"`
	RunnerGroupID int64  `json:"runner_group_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	Busy          bool   `json:"busy"`
}

// runnerAPI fakes the org's runner API, refusing a removal of an id in refusals with that status, and records each call.
type runnerAPI struct {
	mu                       sync.Mutex
	groups                   []map[string]any
	groupsStatus, listStatus int
	runners                  []gitHubRunner
	refusals                 map[int64]int
	refusal                  string
	calls, bearers           []string
	removing                 atomic.Int32
	mostAtOnce               int32
	url                      string
}

func gitHubRunners(t *testing.T, runners ...gitHubRunner) *runnerAPI {
	t.Helper()
	for i := range runners {
		runners[i].RunnerGroupID = cmp.Or(runners[i].RunnerGroupID, runnerGroupID)
	}
	api := &runnerAPI{runners: runners, listStatus: http.StatusOK, groupsStatus: http.StatusOK, refusals: map[int64]int{},
		refusal: forbiddenRefusal, groups: []map[string]any{{"id": 1, "name": "Default"}, {"id": runnerGroupID, "name": runnerGroup},
			{"id": otherGroupID, "name": "another-cluster"}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.record(r)
		w.Header().Set("Content-Type", "application/json")
		group, inGroups := strings.CutPrefix(r.URL.Path, runnerGroupsPath+"/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == runnerGroupsPath:
			answer(w, api.groupsStatus, map[string]any{"total_count": len(api.groups), "runner_groups": api.groups})
		case r.Method == http.MethodGet && inGroups && strings.HasSuffix(group, "/runners"):
			id, _ := strconv.ParseInt(strings.TrimSuffix(group, "/runners"), 10, 64)
			api.list(w, r, id)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, runnersPath+"/"):
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

// answer writes body, or GitHub's refusal of the token when status is not 200.
func answer(w http.ResponseWriter, status int, body any) {
	if status != http.StatusOK {
		w.WriteHeader(status)
		body = map[string]string{"message": "Bad credentials"}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (api *runnerAPI) list(w http.ResponseWriter, r *http.Request, group int64) {
	inGroup := slices.DeleteFunc(slices.Clone(api.runners), func(runner gitHubRunner) bool { return runner.RunnerGroupID != group })
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	from := min(max(page-1, 0)*perPage, len(inGroup))
	answer(w, api.listStatus, map[string]any{"total_count": len(inGroup), "runners": inGroup[from:min(from+perPage, len(inGroup))]})
}

// callsTo is each call the sweep sent that starts with prefix, such as a method or a method and a path.
func (api *runnerAPI) callsTo(prefix string) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(api.calls), func(call string) bool { return !strings.HasPrefix(call, prefix) })
}

// lists is each GET the sweep sent for a page of its runner group's runners.
func (api *runnerAPI) lists() []string { return api.callsTo("GET " + groupRunnersPath) }

func (api *runnerAPI) removals() []string { return api.callsTo(http.MethodDelete) }

// remove holds each removal open long enough that a second one sent at the same time overlaps it.
func (api *runnerAPI) remove(w http.ResponseWriter, r *http.Request) {
	open := api.removing.Add(1)
	defer api.removing.Add(-1)
	api.mu.Lock()
	api.mostAtOnce = max(api.mostAtOnce, open)
	api.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, runnersPath+"/"), 10, 64)
	if status, refused := api.refusals[id]; err == nil && refused {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": api.refusal})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sweeping is the environment the report's CronJob gives the sweep, with the leased token in a file and GitHub's API faked.
func sweeping(t *testing.T, api *runnerAPI) []string {
	t.Helper()
	token := filepath.Join(t.TempDir(), "github_token")
	require.NoError(t, os.WriteFile(token, []byte(runnerToken), 0o600))
	return []string{"RUNNERS_ORGANIZATION=" + runnerOrg, "RUNNER_GROUP=" + runnerGroup, "RUNNER_SCALE_SET=" + runnerScaleSet,
		"RUNNERS_NAMESPACE=" + components.RunnersNamespace, "RUNNER_TOKEN_FILE=" + token, "GITHUB_API_URL=" + api.url}
}

// atRun answers date +%s as the start of the report run with that index, five minutes apart, and any other date call as date does.
func atRun(fakes map[string]string, run int) map[string]string {
	fakes = maps.Clone(fakes)
	fakes["date"] = fmt.Sprintf(`if [ "$*" = +%%s ]; then echo %d; else PATH=${PATH#*:} exec date "$@"; fi`, run*300)
	return fakes
}

// withRunnerPods answers the runner namespace's read with a pod of each name and no EphemeralRunner.
func withRunnerPods(names ...string) map[string]string { return withRunnerHolds(names, nil) }

// withRunnerHolds answers the runner namespace's read with a pod of each name in pods and an EphemeralRunner of each in ephemeral.
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
		"an offline runner of a same-named scale set in another runner group, as another cluster registers, is kept unread": {
			runner: gitHubRunner{ID: 20, RunnerGroupID: otherGroupID, Name: killedRunner, Status: "offline"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, tc.runner)

			_, posted, out, err := runReport(t, withRunnerHolds(tc.pods, tc.ephemeral), sweeping(t, api)...)

			require.NoError(t, err, out)
			assert.Equal(t, []string{"GET " + runnerGroupsPath + "?per_page=100", "GET " + groupRunnersPath + "?per_page=100&page=1"}, api.callsTo(http.MethodGet),
				"the sweep finds its runner group by name and reads only that group's runners")
			want := map[string]any{"scaleSet": runnerScaleSet, "listed": float64(1), "removed": []any{}, "unremoved": []any{},
				"deferred": float64(0), "unlisted": float64(0)}
			if !strings.HasPrefix(tc.runner.Name, runnerScaleSet+"-7xk2p-runner-") || tc.runner.RunnerGroupID == otherGroupID {
				want["listed"] = float64(0)
			}
			removals := []string{}
			if tc.removed {
				want["removed"] = []any{tc.runner.Name}
				removals = []string{fmt.Sprintf("DELETE %s/%d", runnersPath, tc.runner.ID)}
			}
			if tc.waits {
				want["deferred"] = float64(1)
			}
			assert.Equal(t, removals, api.removals(), "the removals the sweep asked GitHub for")
			assert.Equal(t, want, sweptMember(t, posted))
			assert.Equal(t, slices.Repeat([]string{"Bearer " + runnerToken}, len(api.calls)), api.bearers, "each GitHub call carries the leased runner token")
			assert.NotContains(t, string(posted.raw)+out, runnerToken, "the leased token reaches neither the platform nor the log")
		})
	}
}

func TestReport_ARemovalGitHubRefusesLeavesTheRunnerUnremovedNamingItWithGitHubsStatusAndMessageAndTheRestAndEach404AreRemoved(t *testing.T) {
	gone := "mitstack-ci-7xk2p-runner-n4t8r"
	api := gitHubRunners(t, gitHubRunner{ID: 21, Name: killedRunner, Status: "offline"},
		gitHubRunner{ID: 22, Name: "mitstack-ci-7xk2p-runner-h5j6k", Status: "offline"}, gitHubRunner{ID: 23, Name: gone, Status: "offline"})
	api.refusals[21], api.refusals[23] = http.StatusForbidden, http.StatusNotFound
	api.refusal = forbiddenRefusal + strings.Repeat(" and more", 40)

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, "a refused removal is a reading of the report, which still posts: %s", out)
	assert.ElementsMatch(t, []string{"DELETE " + runnersPath + "/21", "DELETE " + runnersPath + "/22", "DELETE " + runnersPath + "/23"}, api.removals())
	member := sweptMember(t, posted)
	assert.ElementsMatch(t, []any{"mitstack-ci-7xk2p-runner-h5j6k", gone}, member["removed"], "a 404 means ARC removed it between the list and the removal")
	assert.Equal(t, map[string]any{"scaleSet": runnerScaleSet, "listed": float64(3), "deferred": float64(0), "unlisted": float64(0),
		"removed": member["removed"], "unremoved": []any{map[string]any{"name": killedRunner, "id": float64(21), "status": float64(http.StatusForbidden),
			"message": api.refusal[:256]}}}, member, "the refused runner by name, with GitHub's status and its message cut to 256 characters")
}

func TestReport_RemovesNoRunnerAndNamesTheReadThatFailedWhenGitHubsListOrTheRunnerNamespacesReadFails(t *testing.T) {
	cases := map[string]struct {
		groupsStatus int
		noGroup      bool
		listStatus   int
		fakes        map[string]string
		named        []string
	}{
		"GitHub refuses the leased token": {listStatus: http.StatusUnauthorized, fakes: withRunnerPods(), named: []string{"401", "Bad credentials"}},
		"GitHub fails the list":           {listStatus: http.StatusBadGateway, fakes: withRunnerPods(), named: []string{"502"}},
		"GitHub refuses the list of runner groups": {groupsStatus: http.StatusForbidden, fakes: withRunnerPods(),
			named: []string{"403", "runner groups"}},
		"GitHub lists no runner group of the scale set's group name": {noGroup: true, fakes: withRunnerPods(),
			named: []string{runnerGroup}},
		"the read of the runner pods and EphemeralRunners fails": {
			fakes: map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: failedRead})},
			named: []string{"pods", "EphemeralRunners", "exit status 3", "the server is gone"}},
		"the read of the runner pods and EphemeralRunners answers no list": {
			fakes: map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: "echo 'not json'"})},
			named: []string{"pods", "EphemeralRunners"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, gitHubRunner{ID: 31, Name: killedRunner, Status: "offline"})
			api.listStatus = cmp.Or(tc.listStatus, http.StatusOK)
			api.groupsStatus = cmp.Or(tc.groupsStatus, http.StatusOK)
			if tc.noGroup {
				api.groups = api.groups[:1]
			}

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

func TestReport_ReadsASweepThatStopsBeforeItFinishesAsUnreadAndSkipsItUntilTheFirstRunnerTokenIsLeasedWithNoGitHubCall(t *testing.T) {
	cases := map[string]struct{ env, key, names string }{
		"a sweep that stops before it finishes reads unread rather than clean": {"GITHUB_API_URL=", "unread", "ended before it finished"},
		"a sweep with no leased token yet is skipped":                          {"RUNNER_TOKEN_FILE=" + filepath.Join(t.TempDir(), "github_token"), "skipped", "leased"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, gitHubRunner{ID: 32, Name: killedRunner, Status: "offline"})

			_, posted, out, err := runReport(t, withRunnerPods(), append(sweeping(t, api), tc.env)...)

			require.NoError(t, err, "a sweep that removes nothing is a reading of the report, which still posts: %s", out)
			assert.Empty(t, api.calls, "the sweep calls GitHub only with a leased token and every setting")
			member := sweptMember(t, posted)
			assert.Equal(t, []string{"scaleSet", tc.key}, slices.Sorted(maps.Keys(member)))
			assert.Contains(t, member[tc.key], tc.names)
		})
	}
}

func TestReport_ReadsEveryPageOfItsRunnerGroupsRunnersAndRemovesAtMostTenRunnersARunOneAtATimeLeavingTheRestForTheNext(t *testing.T) {
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
	assert.Equal(t, []string{"GET " + groupRunnersPath + "?per_page=100&page=1", "GET " + groupRunnersPath + "?per_page=100&page=2"}, api.lists(),
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
	assert.Len(t, api.lists(), 3, "the sweep's GitHub calls stay few however many runners the group holds")
	assert.Empty(t, api.removals(), "a runner past the third page is never seen")
	member := sweptMember(t, posted)
	assert.Equal(t, float64(5), member["unlisted"], "the report says how many runners the sweep did not read")
}

func TestReport_StartsEachRunsRemovalsWhereTheRunBeforeStoppedSoRunnersGitHubKeepsRefusingCannotStarveTheRest(t *testing.T) {
	var runners []gitHubRunner
	for i := range 12 {
		runners = append(runners, gitHubRunner{ID: int64(500 + i), Name: fmt.Sprintf("mitstack-ci-abcde-runner-%05d", i), Status: "offline"})
	}
	removal := func(id int) string { return fmt.Sprintf("DELETE %s/%d", runnersPath, id) }
	cases := map[string]struct {
		run      int
		removals []string
		removed  []any
	}{
		"the first run tries the first ten, which GitHub all refuses": {run: 0,
			removals: []string{removal(500), removal(501), removal(502), removal(503), removal(504), removal(505), removal(506),
				removal(507), removal(508), removal(509)}, removed: []any{}},
		"the next run starts at the eleventh and wraps round, so the two left are removed": {run: 1,
			removals: []string{removal(510), removal(511), removal(500), removal(501), removal(502), removal(503), removal(504),
				removal(505), removal(506), removal(507)}, removed: []any{"mitstack-ci-abcde-runner-00010", "mitstack-ci-abcde-runner-00011"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, slices.Clone(runners)...)
			for id := range 10 {
				api.refusals[int64(500+id)] = http.StatusForbidden
			}

			_, posted, out, err := runReport(t, atRun(withRunnerPods(), tc.run), sweeping(t, api)...)

			require.NoError(t, err, out)
			assert.Equal(t, tc.removals, api.removals(), "the removals in the order the sweep asked them")
			member := sweptMember(t, posted)
			assert.Equal(t, tc.removed, member["removed"])
			assert.Equal(t, float64(2), member["deferred"], "two idle runners wait for a later run")
		})
	}
}

func TestReport_CarriesNoRunnersMemberAndCallsNoGitHubWithoutTheSweepConfigured(t *testing.T) {
	_, posted, out, err := runReport(t, withRunnerPods(killedRunner))

	require.NoError(t, err, out)
	assert.NotContains(t, posted.body, "runners", "a cluster that runs no scale set has nothing to sweep")
}

func runnerReportInputs() report.Inputs {
	in := inputs(reportURL)
	in.Runners = &components.RunnerInputs{Organization: runnerOrg, Group: runnerGroup, ScaleSet: runnerScaleSet,
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
	assert.Equal(t, runnerGroup, env["RUNNER_GROUP"])
	assert.Equal(t, runnerScaleSet, env["RUNNER_SCALE_SET"])
	assert.Equal(t, components.RunnersNamespace, env["RUNNERS_NAMESPACE"], "the namespace ARC runs in, from its one owner")
	assert.Equal(t, "https://api.github.com", env["GITHUB_API_URL"])
	assert.Equal(t, "/var/run/mitstack-runner/github_token", env["RUNNER_TOKEN_FILE"])
	assert.Contains(t, testutil.Field(t, container, "volumeMounts"), map[string]any{"name": "runner-token", "mountPath": "/var/run/mitstack-runner", "readOnly": true})
	pod := testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec")
	assert.Contains(t, testutil.Field(t, pod, "volumes"), map[string]any{"name": "runner-token", "secret": map[string]any{
		"secretName": components.RunnerSweepTokenSecret, "optional": true, "defaultMode": 0o440}},
		"the Secret the lease job fills, optional so the report runs before the first lease")
	for _, name := range []string{"RUNNERS_ORGANIZATION", "RUNNER_GROUP", "RUNNER_SCALE_SET", "RUNNERS_NAMESPACE", "GITHUB_API_URL", "RUNNER_TOKEN_FILE"} {
		assert.NotContains(t, testutil.ContainerEnv(t, without), name, "a report without runner inputs sweeps nothing")
	}
	assert.NotContains(t, fmt.Sprint(testutil.Field(t, without, "volumeMounts")), "runner-token")
}

func TestManifests_GrantsTheReportOnlyAListOfARCsEphemeralRunnersInTheRunnerNamespaceBeyondItsRightsWithoutRunnerInputs(t *testing.T) {
	rendered, err := report.Manifests(runnerReportInputs())
	require.NoError(t, err)
	with, without := testutil.Documents(t, rendered), testutil.Documents(t, manifests(t, reportURL))
	rbac := func(docs []map[string]any) []map[string]any {
		return slices.Concat(testutil.Kind(docs, "ClusterRole"), testutil.Kind(docs, "ClusterRoleBinding"), testutil.Kind(docs, "Role"),
			testutil.Kind(docs, "RoleBinding"))
	}
	added := slices.DeleteFunc(rbac(with), func(doc map[string]any) bool {
		return slices.ContainsFunc(rbac(without), func(other map[string]any) bool { return reflect.DeepEqual(doc, other) })
	})

	assert.Len(t, rbac(with), len(rbac(without))+2, "the sweep adds one Role and its binding, and changes no other right")
	assert.Len(t, added, 2, "the ClusterRole and every other right render as they do without runner inputs")
	role := testutil.Named(t, testutil.Kind(added, "Role"), "Role", "mitstack-node-report-runners")
	assert.Equal(t, components.RunnersNamespace, testutil.Field(t, role, "metadata", "namespace"), "the one namespace the sweep reads")
	assert.Equal(t, []any{map[string]any{"apiGroups": []any{"actions.github.com"}, "resources": []any{"ephemeralrunners"}, "verbs": []any{"list"}}},
		testutil.Field(t, role, "rules"), "the objects ARC holds for each runner, which carry no Secret data")
	binding := testutil.Named(t, testutil.Kind(added, "RoleBinding"), "RoleBinding", "mitstack-node-report-runners")
	assert.Equal(t, components.RunnersNamespace, testutil.Field(t, binding, "metadata", "namespace"))
	assert.Equal(t, map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "mitstack-node-report-runners"},
		testutil.Field(t, binding, "roleRef"))
	assert.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "mitstack-node-report", "namespace": nodeconfig.TalosAPINamespace}},
		testutil.Field(t, binding, "subjects"), "only the report's own service account")
}

func TestManifests_RefusesRunnerInputsNamingNoGitHubOrganisation(t *testing.T) {
	in := runnerReportInputs()
	in.Runners.Organization = "not an org"

	_, err := report.Manifests(in)

	require.ErrorContains(t, err, "Organization")
}
