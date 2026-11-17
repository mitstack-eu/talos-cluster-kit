package clusterrepo_test

// A Talos cluster's repository is dialled, read, created, marked, seeded and deleted.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

func refusal(t *testing.T, err error) *handler.Refused {
	t.Helper()
	var refused *handler.Refused
	require.ErrorAs(t, err, &refused, "the error is a block-kit refusal: %v", err)
	return refused
}

func TestDial_RefusesARequestWhoseGitHubConnectionCannotBeActedOnBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name    string
		desired []desiredOption
		appName string
		class   string
		detail  string
	}{
		{"a request with no GitHub token", []desiredOption{withGitHubSecret("token", "")}, testApp,
			"missing-github-access", "secrets.github.token is empty"},
		{"a request naming no GitHub owner", []desiredOption{withGitHubSecret("owner", "")}, testApp,
			"missing-github-access", "secrets.github.owner is empty"},
		{"an app name GitHub takes as no repository name", nil, "shop/x",
			"invalid-param", "the cluster repository name is not usable"},
		{"a subscription id longer than a GitHub topic", []desiredOption{withSubscription("sub_" + strings.Repeat("a", 50))}, testApp,
			"invalid-subscription", "at most 50 lowercase letters, digits and hyphens"},
		{"a GitHub endpoint the operator's allowlist does not name", []desiredOption{withGitHubEndpoint("https://github.attacker.example")}, testApp,
			"untrusted-api-base", "github api base rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)

			_, err := gh.dial(desired(tc.desired...), tc.appName)

			r := refusal(t, err)
			assert.Equal(t, tc.class, r.Class, "the refusal's class")
			assert.Contains(t, r.Detail, tc.detail, "the refusal's detail")
			assert.Empty(t, gh.bases, "no GitHub client is built for a refused request")
			assert.Empty(t, gh.madeCalls(), "no call reaches GitHub")
		})
	}
}

func TestDial_TakesGitHubsOwnAPIWhenTheRequestNamesNoEndpoint(t *testing.T) {
	gh := newFakeGitHub(t)

	a, err := gh.dial(desired(), testApp)

	require.NoError(t, err)
	assert.Equal(t, []string{"https://api.github.com"}, gh.bases, "the base the client is built on")
	assert.Equal(t, testToken, gh.clientOf, "the client carries the request's token")
	assert.Equal(t, clusterrepo.Repo{Owner: testOwner, Name: "shop-cluster"}, a.Repo)
	assert.Equal(t, testSub, a.SubscriptionID())
}

func TestFor_NamesTheAppsClusterRepositoryAndItsAddress(t *testing.T) {
	repo := clusterrepo.For("acme", "shop")

	assert.Equal(t, "acme/shop-cluster", repo.FullName())
	assert.Equal(t, "https://github.com/acme/shop-cluster", repo.URL())
}

func TestMark_IsTheBlocksTopicAndTheSubscriptionsTopicInLowercase(t *testing.T) {
	assert.Equal(t, []string{"mitstack-hcloud-talos-cluster", "mitstack-sub-ab12"}, clusterrepo.Mark(testBlock, testSub))
	assert.Equal(t, "mitstack-sub-ab12", clusterrepo.SubscriptionTopic(testSub))
}

func TestCarriesMark_RequiresBothTopicsOfTheMark(t *testing.T) {
	cases := []struct {
		name   string
		topics []string
		want   bool
	}{
		{"both topics among others carry the mark", []string{"team", "mitstack-hcloud-talos-cluster", "mitstack-sub-ab12"}, true},
		{"the block's topic alone does not", []string{"mitstack-hcloud-talos-cluster"}, false},
		{"the subscription's topic alone does not", []string{"mitstack-sub-ab12"}, false},
		{"another block's topic does not", []string{"mitstack-hcloud-hrobot-talos-cluster", "mitstack-sub-ab12"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, clusterrepo.CarriesMark(tc.topics, testBlock, testSub), "CarriesMark(%v)", tc.topics)
		})
	}
}

func TestRead_ReadsWhetherTheRepositoryExistsAndCarriesTheSubscriptionsMark(t *testing.T) {
	cases := []struct {
		name string
		hold func(gh *fakeGitHub)
		want clusterrepo.Read
	}{
		{"no repository", func(*fakeGitHub) {}, clusterrepo.Read{}},
		{"a repository without the mark", func(gh *fakeGitHub) { gh.holdRepo("mitstack-hcloud-talos-cluster") }, clusterrepo.Read{Exists: true}},
		{"a repository with the mark", func(gh *fakeGitHub) {
			gh.holdRepo("team", "mitstack-hcloud-talos-cluster", "mitstack-sub-ab12")
		}, clusterrepo.Read{Exists: true, Marked: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			tc.hold(gh)

			got, err := gh.access(t).Read(ctx())

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRead_AGitHubAnswerOtherThanFoundOrNotFoundIsAnError(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.answerWith("GET /repos/"+testRepo, http.StatusBadGateway, "")

	_, err := gh.access(t).Read(ctx())

	assert.EqualError(t, err, "reading acme/shop-cluster: github returned 502")
}

func TestEnsure_CreatesAPrivateRepositoryMarksItBesideGitHubsOwnTopicsAndSeedsBothDirectories(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.createdTopics = []string{"team-infra"}

	err := gh.access(t).Ensure(ctx(), clusterrepo.Read{})

	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "shop-cluster", "private": true, "auto_init": true}, gh.bodies["POST /orgs/acme/repos"])
	assert.Equal(t, []string{"team-infra", "mitstack-hcloud-talos-cluster", "mitstack-sub-ab12"}, gh.topics())
	for path, content := range map[string]string{
		clusterrepo.SeedMarkerFile: "# cluster\n\nArgo CD applies every manifest in this directory to the cluster.\n",
		"repository-credentials/README.md": "# repository-credentials\n\n" +
			"Each app slice commits its repository Secret and Role here, and the cluster's token renewal writes the Secret's password.\n",
	} {
		file, ok := gh.file(path)
		require.True(t, ok, "the seed writes %s", path)
		assert.Equal(t, content, file.content, "the content of %s", path)
		assert.Equal(t, "Seed "+path+"\n\n"+observe.GitHubSubscriptionTrailer(testSub), file.message, "the commit message of %s", path)
	}
}

func TestEnsure_SeedsOnlyTheFilesAMarkedRepositoryLacks(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...)
	gh.holdFile(clusterrepo.SeedMarkerFile, "the owner's own README")

	err := gh.access(t).Ensure(ctx(), clusterrepo.Read{Exists: true, Marked: true})

	require.NoError(t, err)
	assert.NotContains(t, gh.madeCalls(), "POST /orgs/acme/repos", "a repository that exists is not created")
	assert.NotContains(t, gh.madeCalls(), "PUT /repos/acme/shop-cluster/topics", "a marked repository is not marked again")
	kept, _ := gh.file(clusterrepo.SeedMarkerFile)
	assert.Equal(t, "the owner's own README", kept.content, "a file already there is kept")
	_, seeded := gh.file("repository-credentials/README.md")
	assert.True(t, seeded, "the missing file is seeded")
}

func TestEnsure_ACreateGitHubRefusesNamesTheRepositoryAndKeepsTheTokenOutOfTheError(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.answerWith("POST /orgs/acme/repos", http.StatusUnprocessableEntity, `{"message":"name already exists","token":"`+testToken+`"}`)

	err := gh.access(t).Ensure(ctx(), clusterrepo.Read{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating the cluster repository acme/shop-cluster: github returned 422")
	assert.NotContains(t, err.Error(), testToken, "the error carries no token")
}

func TestRefuseUnmarked_RefusesOnlyARepositoryThatExistsWithoutTheMark(t *testing.T) {
	gh := newFakeGitHub(t)
	a := gh.access(t)

	err := a.RefuseUnmarked(clusterrepo.Read{Exists: true})

	r := refusal(t, err)
	assert.Equal(t, "repository-not-marked", r.Class)
	assert.Equal(t, "the repository acme/shop-cluster exists but does not carry this subscription's mark, the topics "+
		"mitstack-hcloud-talos-cluster and mitstack-sub-ab12, so nothing was changed. Add both topics if it is this cluster's "+
		"repository, or move it out of the way, and retry", r.Detail)
	assert.NoError(t, a.RefuseUnmarked(clusterrepo.Read{Exists: true, Marked: true}), "a marked repository")
	assert.NoError(t, a.RefuseUnmarked(clusterrepo.Read{}), "no repository")
}

func TestHoldsSeed_ReadsWhetherTheSeedMarkerFileIsThere(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...)
	a := gh.access(t)

	before, err := a.HoldsSeed(ctx())
	require.NoError(t, err)
	gh.holdFile(clusterrepo.SeedMarkerFile, "seed")
	after, err := a.HoldsSeed(ctx())

	require.NoError(t, err)
	assert.False(t, before, "a repository without the marker file holds no seed")
	assert.True(t, after, "a repository with the marker file holds the seed")
}

func TestDelete_DeletesTheClusterRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...)

	err := gh.access(t).Delete(ctx())

	require.NoError(t, err)
	assert.Contains(t, gh.madeCalls(), "DELETE /repos/acme/shop-cluster")
	assert.Nil(t, gh.topics(), "the repository is gone")
}

func TestCall_SendsTheTokenAndGitHubsAPIVersionAndAJSONBody(t *testing.T) {
	gh := newFakeGitHub(t)

	status, _, err := gh.access(t).Call(ctx(), http.MethodPost, "/anything", map[string]any{"k": "v"})

	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status)
	h := gh.headers["POST /anything"]
	assert.Equal(t, "Bearer "+testToken, h.Get("Authorization"))
	assert.Equal(t, "application/vnd.github+json", h.Get("Accept"))
	assert.Equal(t, "2022-11-28", h.Get("X-GitHub-Api-Version"))
	assert.Equal(t, "application/json", h.Get("Content-Type"))
	assert.Equal(t, map[string]any{"k": "v"}, gh.bodies["POST /anything"])
}

func TestCall_ReadsAtMostItsLimitOfAnAnswer(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.answerWith("GET /big", http.StatusOK, strings.Repeat("x", 70<<10))
	a := gh.access(t)

	_, whole, err := a.Call(ctx(), http.MethodGet, "/big", nil)
	require.NoError(t, err)
	_, limited, err := a.CallLimited(ctx(), http.MethodGet, "/big", nil, 100)
	require.NoError(t, err)

	assert.Len(t, whole, 64<<10, "Call reads at most 64 KiB")
	assert.Len(t, limited, 100, "CallLimited reads at most its limit")
}

func TestGetIfFound_ReadsAFoundBodyAndANotFoundAsAbsent(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.holdRepo("team")
	a := gh.access(t)

	raw, found, err := a.GetIfFound(ctx(), "/repos/"+testRepo, testRepo)
	require.NoError(t, err)
	_, missing, err := a.GetIfFound(ctx(), "/repos/acme/elsewhere", "acme/elsewhere")

	require.NoError(t, err)
	assert.True(t, found)
	assert.JSONEq(t, `{"full_name":"acme/shop-cluster"}`, string(raw))
	assert.False(t, missing)
}
