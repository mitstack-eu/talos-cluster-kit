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
	"testing"

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
	killedRunner     = "mitstack-ci-runner-tlmgf"
	startingRunner   = "mitstack-ci-runner-wrzwk"
)

// gitHubRunner is one runner as GitHub lists it, in the scale set's runner group unless RunnerGroupID names another.
type gitHubRunner struct {
	ID            int64  `json:"id"`
	RunnerGroupID int64  `json:"runner_group_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	Busy          bool   `json:"busy"`
}

// runnerAPI fakes the org's runner API, answering a list with listBody when set and a refusal with the headers in limit.
type runnerAPI struct {
	mu                       sync.Mutex
	groups                   []map[string]any
	groupsStatus, listStatus int
	listBody                 []byte
	runners                  []gitHubRunner
	refusals                 map[int64]int
	refusal                  string
	limit                    http.Header
	calls, bearers           []string
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
		case r.Method == http.MethodGet && inGroups && api.listBody != nil:
			_, _ = w.Write(api.listBody)
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

func (api *runnerAPI) remove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, runnersPath+"/"), 10, 64)
	if status, refused := api.refusals[id]; err == nil && refused {
		maps.Copy(w.Header(), api.limit)
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
		scaleSet  string
		pods      []string
		ephemeral []string
		removed   bool
		waits     bool
		notOurs   bool
	}{
		"a runner whose pod and EphemeralRunner went mid-job is offline and idle once GitHub fails the job, and it is removed": {
			runner: gitHubRunner{ID: 11, Name: killedRunner, Status: "offline"}, removed: true},
		"a runner GitHub still reads busy after its pod went mid-job waits for GitHub to fail the job, with no removal asked": {
			runner: gitHubRunner{ID: 12, Name: killedRunner, Status: "offline", Busy: true}, waits: true},
		"a runner whose pod ended after its job leaves it offline and idle with no pod, and it is removed": {
			runner: gitHubRunner{ID: 13, Name: killedRunner, Status: "offline"}, pods: []string{"mitstack-ci-7xk2p-listener"}, removed: true},
		"the same runner offline with its pod still running is kept": {
			runner: gitHubRunner{ID: 10, Name: killedRunner, Status: "offline"}, pods: []string{killedRunner}},
		"a runner of a scale set whose name Kubernetes cuts to 58 characters before the five it generates is removed": {
			scaleSet: "acme-continuous-integration-for-every-repository-of-acme", removed: true,
			runner: gitHubRunner{ID: 9, Name: "acme-continuous-integration-for-every-repository-of-acme-rtlmgf", Status: "offline"}},
		"a runner starting up is offline with its pod, and it is kept": {
			runner: gitHubRunner{ID: 14, Name: startingRunner, Status: "offline"}, pods: []string{startingRunner}},
		"a runner ARC registered before it created the pod is offline with only its EphemeralRunner, and it is kept": {
			runner: gitHubRunner{ID: 15, Name: startingRunner, Status: "offline"}, ephemeral: []string{startingRunner}},
		"a runner online with its pod is kept": {
			runner: gitHubRunner{ID: 16, Name: startingRunner, Status: "online", Busy: true}, pods: []string{startingRunner}},
		"a runner GitHub still lists online after its pod went is kept until GitHub reads it offline": {
			runner: gitHubRunner{ID: 17, Name: killedRunner, Status: "online"}},
		"an offline runner of another scale set whose name starts with this one's, as ARC names mitstack-ci-kata's, is kept": {
			runner: gitHubRunner{ID: 18, Name: "mitstack-ci-kata-runner-tlmgf", Status: "offline"}, notOurs: true},
		"an offline runner registered by hand under a name Kubernetes never generates is kept": {
			runner: gitHubRunner{ID: 19, Name: "mitstack-ci-runner-dev01", Status: "offline"}, notOurs: true},
		"an offline runner named as ARC before 0.15 named one, which is how ARC 0.15 names a runner of mitstack-ci-7xk2p, is kept": {
			runner: gitHubRunner{ID: 8, Name: "mitstack-ci-7xk2p-runner-q8w4z", Status: "offline"}, notOurs: true},
		"an offline runner of a same-named scale set in another runner group, as another cluster registers, is kept unread": {
			runner: gitHubRunner{ID: 20, RunnerGroupID: otherGroupID, Name: killedRunner, Status: "offline"}, notOurs: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, tc.runner)

			scaleSet := cmp.Or(tc.scaleSet, runnerScaleSet)

			_, posted, out, err := runReport(t, withRunnerHolds(tc.pods, tc.ephemeral), append(sweeping(t, api), "RUNNER_SCALE_SET="+scaleSet)...)

			require.NoError(t, err, out)
			assert.Equal(t, []string{"GET " + runnerGroupsPath + "?per_page=100", "GET " + groupRunnersPath + "?per_page=100&page=1"}, api.callsTo(http.MethodGet),
				"the sweep finds its runner group by name and reads only that group's runners")
			want := map[string]any{"scaleSet": scaleSet, "listed": float64(1), "removed": []any{}, "unremoved": []any{},
				"busy": []any{}, "deferred": float64(0), "unlisted": float64(0)}
			if tc.notOurs {
				want["listed"] = float64(0)
			}
			removals := []string{}
			if tc.removed {
				want["removed"] = []any{tc.runner.Name}
				removals = []string{fmt.Sprintf("DELETE %s/%d", runnersPath, tc.runner.ID)}
			}
			if tc.waits {
				want["busy"], want["deferred"] = []any{tc.runner.Name}, float64(1)
			}
			assert.Equal(t, removals, api.removals(), "the removals the sweep asked GitHub for")
			assert.Equal(t, want, sweptMember(t, posted))
			assert.Equal(t, slices.Repeat([]string{"Bearer " + runnerToken}, len(api.calls)), api.bearers, "each GitHub call carries the leased runner token")
			assert.NotContains(t, string(posted.raw)+out, runnerToken, "the leased token reaches neither the platform nor the log")
		})
	}
}

func TestReport_ARemovalGitHubRefusesLeavesTheRunnerListedAndUnremovedWithGitHubsStatusAndMessageUntilARunGitHubAcceptsItAndTheRestAndEach404AreRemoved(t *testing.T) {
	gone := "mitstack-ci-runner-n4t8r"
	api := gitHubRunners(t, gitHubRunner{ID: 21, Name: killedRunner, Status: "offline"},
		gitHubRunner{ID: 22, Name: "mitstack-ci-runner-lqrcl", Status: "offline"}, gitHubRunner{ID: 23, Name: gone, Status: "offline"})
	api.refusals[21], api.refusals[23] = http.StatusForbidden, http.StatusNotFound
	api.refusal = forbiddenRefusal + strings.Repeat(" and more", 40)

	_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, "a refused removal is a reading of the report, which still posts: %s", out)
	assert.ElementsMatch(t, []string{"DELETE " + runnersPath + "/21", "DELETE " + runnersPath + "/22", "DELETE " + runnersPath + "/23"}, api.removals())
	member := sweptMember(t, posted)
	assert.ElementsMatch(t, []any{"mitstack-ci-runner-lqrcl", gone}, member["removed"], "a 404 means ARC removed it between the list and the removal")
	assert.Equal(t, map[string]any{"scaleSet": runnerScaleSet, "listed": float64(3), "busy": []any{}, "deferred": float64(0), "unlisted": float64(0),
		"removed": member["removed"], "unremoved": []any{map[string]any{"name": killedRunner, "id": float64(21), "status": float64(http.StatusForbidden),
			"message": api.refusal[:256]}}}, member, "the refused runner by name, with GitHub's status and its message cut to 256 characters")

	delete(api.refusals, 21)
	_, next, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

	require.NoError(t, err, out)
	assert.Contains(t, sweptMember(t, next)["removed"], killedRunner, "the next run asks again and removes the runner once GitHub accepts")
}

func TestReport_RemovesNoRunnerAndNamesTheReadThatFailedWhenGitHubsListOrTheRunnerNamespacesReadFails(t *testing.T) {
	cases := map[string]struct {
		groupsStatus int
		fake         func(*runnerAPI)
		listStatus   int
		listBody     []byte
		env          []string
		fakes        map[string]string
		named        []string
	}{
		"GitHub refuses the leased token":            {listStatus: http.StatusUnauthorized, fakes: withRunnerPods(), named: []string{"401", "Bad credentials"}},
		"GitHub fails the list":                      {listStatus: http.StatusBadGateway, fakes: withRunnerPods(), named: []string{"502"}},
		"GitHub answers the list with an empty body": {listBody: []byte{}, fakes: withRunnerPods(), named: []string{"200", "runners array"}},
		"GitHub answers the list with runners that are not an array": {listBody: []byte(`{"total_count": 1, "runners": {}}`), fakes: withRunnerPods(),
			named: []string{"200", "runners array"}},
		"GitHub answers the list with no total_count": {listBody: []byte(`{"runners": []}`), fakes: withRunnerPods(), named: []string{"200", "total_count"}},
		"GitHub refuses the list of runner groups":    {groupsStatus: http.StatusForbidden, fakes: withRunnerPods(), named: []string{"403", "runner groups"}},
		"GitHub lists no runner group of the scale set's group name": {fake: func(api *runnerAPI) { api.groups = api.groups[:1] }, fakes: withRunnerPods(),
			named: []string{runnerGroup}},
		"GitHub answers the list of runner groups with no runner_groups array": {fake: func(api *runnerAPI) { api.groups = nil }, fakes: withRunnerPods(),
			named: []string{"200", "runner_groups array"}},
		"GitHub lists an offline runner whose busy is neither true nor false": {fakes: withRunnerPods(), named: []string{killedRunner, "busy"},
			listBody: []byte(`{"total_count": 1, "runners": [{"id": 31, "name": "` + killedRunner + `", "status": "offline"}]}`)},
		"the read of the runner pods and EphemeralRunners fails": {named: []string{"pods", "EphemeralRunners", "exit status 3", "the server is gone"},
			fakes: map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: failedRead})}},
		"the read of the runner pods and EphemeralRunners answers no list": {named: []string{"pods", "EphemeralRunners"},
			fakes: map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: "echo 'not json'"})}},
		"the read of the runner pods and EphemeralRunners exits 0 and prints nothing": {
			fakes: map[string]string{"kubectl": kubectlAnswering(map[string]string{runnerHoldsRead: "true"})}, named: []string{"pods", "EphemeralRunners"}},
		"jq cannot pick the runners to remove": {env: []string{"RUNNER_SCALE_SET=mitstack-ci("}, fakes: withRunnerPods(), named: []string{"jq could not pick"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, gitHubRunner{ID: 31, Name: killedRunner, Status: "offline"})
			api.listStatus = cmp.Or(tc.listStatus, http.StatusOK)
			api.groupsStatus = cmp.Or(tc.groupsStatus, http.StatusOK)
			api.listBody = tc.listBody
			if tc.fake != nil {
				tc.fake(api)
			}

			_, posted, out, err := runReport(t, tc.fakes, append(append(sweeping(t, api), "REPORT_READ_SECONDS=2"), tc.env...)...)

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

func TestReport_ReadsPagesOfAHundredUntilOneHoldsFewerOrThreeAreReadAndCountsTheRunnersPastThemAsUnlisted(t *testing.T) {
	cases := map[string]struct{ runners, pages, unlisted, removals int }{
		"152 runners take two pages, and the dead runner last on the second is removed":               {152, 2, 0, 1},
		"305 runners stop the sweep at three pages, so the dead runner fifth past them is never seen": {305, 3, 5, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runners := make([]gitHubRunner, tc.runners)
			for i := range runners {
				runners[i] = gitHubRunner{ID: int64(1000 + i), Name: fmt.Sprintf("other-ci-abcde-runner-%05d", i), Status: "online"}
			}
			runners[tc.runners-1] = gitHubRunner{ID: 999, Name: killedRunner, Status: "offline"}
			api := gitHubRunners(t, runners...)

			_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

			require.NoError(t, err, out)
			lists := []string{}
			for page := range tc.pages {
				lists = append(lists, fmt.Sprintf("GET %s?per_page=100&page=%d", groupRunnersPath, page+1))
			}
			assert.Equal(t, lists, api.lists(), "one GET per page of 100, and never a fourth however many runners the group holds")
			assert.Len(t, api.removals(), tc.removals)
			assert.Equal(t, float64(tc.unlisted), sweptMember(t, posted)["unlisted"], "the report says how many runners the sweep did not read")
		})
	}
}

func TestReport_StopsRemovingOnceGitHubAnswersARemovalWithARateLimitAndLeavesTheRestForALaterRun(t *testing.T) {
	cases := map[string]struct {
		status int
		limit  http.Header
	}{
		"a 429": {status: http.StatusTooManyRequests},
		"a 403 with Retry-After, as GitHub's secondary limit answers":   {http.StatusForbidden, http.Header{"Retry-After": {"60"}}},
		"a 403 with no request left, as GitHub's primary limit answers": {http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := gitHubRunners(t, gitHubRunner{ID: 41, Name: killedRunner, Status: "offline"}, gitHubRunner{ID: 42, Name: startingRunner, Status: "offline"})
			api.refusals[41], api.limit = tc.status, tc.limit

			_, posted, out, err := runReport(t, withRunnerPods(), sweeping(t, api)...)

			require.NoError(t, err, out)
			assert.Equal(t, []string{"DELETE " + runnersPath + "/41"}, api.removals(), "no removal after the limited one, so none is sent beside it either")
			member := sweptMember(t, posted)
			assert.Equal(t, []any{map[string]any{"name": killedRunner, "id": float64(41), "status": float64(tc.status), "message": forbiddenRefusal}},
				member["unremoved"])
			assert.Equal(t, float64(1), member["deferred"], "the runner never tried waits for a later run")
		})
	}
}

func TestReport_AsksAtMostTenRemovalsARunStartingWhereTheRunBeforeStoppedSoRunnersGitHubKeepsRefusingCannotStarveTheRest(t *testing.T) {
	var runners []gitHubRunner
	for i := range 12 {
		runners = append(runners, gitHubRunner{ID: int64(500 + i), Name: fmt.Sprintf("mitstack-ci-runner-zzzz%c", "bcdfghjklmnp"[i]), Status: "offline"})
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
				removal(505), removal(506), removal(507)}, removed: []any{"mitstack-ci-runner-zzzzn", "mitstack-ci-runner-zzzzp"}},
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

func TestTrustedGitHubBase_AnswersTheAllowlistsOwnEntryForGitHubsAPIAndRefusesEveryOtherHost(t *testing.T) {
	cases := map[string]struct{ endpoint, base string }{
		"GitHub's own API resolves to the allowlist's entry":        {"https://api.github.com", "https://api.github.com"},
		"an uppercase host with a path resolves to the entry alone": {"https://API.GitHub.com/orgs/acme", "https://api.github.com"},
		"a host outside the allowlist is refused":                   {endpoint: "https://api.github.com.attacker.example"},
		"the allowlisted host on another port is refused":           {endpoint: "https://api.github.com:8443"},
		"the allowlisted host over http is refused":                 {endpoint: "http://api.github.com"},
		"a loopback address is refused":                             {endpoint: "https://127.0.0.1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base, err := report.TrustedGitHubBase(tc.endpoint)

			if tc.base == "" {
				require.Error(t, err, "trustedGitHubBase(%q) = %q; want a refusal", tc.endpoint, base)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.base, base, "trustedGitHubBase(%q)", tc.endpoint)
		})
	}
}

func TestManifests_RefusesRunnerInputsNamingNoGitHubOrganisation(t *testing.T) {
	in := runnerReportInputs()
	in.Runners.Organization = "not an org"

	_, err := report.Manifests(in)

	require.ErrorContains(t, err, "Organization")
}
