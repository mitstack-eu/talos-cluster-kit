package clusterrepo_test

// An observe reads the cluster repository and its seed from GitHub alone.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

func TestObserve_ReadsTheClusterRepositorysStateAndSaysWhy(t *testing.T) {
	cases := []struct {
		name     string
		desired  []desiredOption
		hold     func(gh *fakeGitHub)
		state    clusterrepo.State
		repoName string
		detail   string
		absent   bool
	}{
		{
			name: "a request that names no app", desired: []desiredOption{withParam("appName", "")}, hold: func(*fakeGitHub) {},
			state: clusterrepo.Unknown, repoName: "(unnamed)", detail: `the cluster repository cannot be named: params["appName"] is empty`,
		},
		{
			name: "a request without a GitHub token", desired: []desiredOption{withGitHubSecret("token", "")}, hold: func(*fakeGitHub) {},
			state: clusterrepo.Unknown, repoName: testRepo,
			detail: "the cluster repository cannot be read: the GitHub connection is missing: secrets.github.token is empty, " +
				"so the cluster repository cannot be read, created or deleted; reconnect GitHub for this app and retry",
		},
		{
			name: "no repository", hold: func(*fakeGitHub) {},
			state: clusterrepo.Gone, repoName: testRepo, detail: "the cluster repository acme/shop-cluster does not exist", absent: true,
		},
		{
			name: "a repository without the mark", hold: func(gh *fakeGitHub) { gh.holdRepo("team") },
			state: clusterrepo.Unmarked, repoName: testRepo,
			detail: "the repository acme/shop-cluster does not carry this subscription's mark, so it is not the cluster's", absent: true,
		},
		{
			name: "a repository with the mark", hold: func(gh *fakeGitHub) { gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...) },
			state: clusterrepo.Present, repoName: testRepo, detail: "the cluster repository acme/shop-cluster exists",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			tc.hold(gh)

			obs, err := clusterrepo.Observe(ctx(), desired(tc.desired...), gh.dial)

			require.NoError(t, err)
			assert.Equal(t, tc.state, obs.State, "the repository's state")
			assert.Equal(t, tc.repoName, obs.Name, "the repository's name")
			assert.Equal(t, tc.detail, obs.Describe())
			assert.Equal(t, tc.absent, obs.Absent(), "whether the repository reads absent")
		})
	}
}

func TestObserve_ARepositoryItCanReachCarriesItsAddressAndItsAccess(t *testing.T) {
	gh := newFakeGitHub(t)

	obs, err := clusterrepo.Observe(ctx(), desired(), gh.dial)

	require.NoError(t, err)
	assert.Equal(t, "https://github.com/acme/shop-cluster", obs.URL)
	require.NotNil(t, obs.Access, "an observe that reached GitHub carries the access it read with")
	assert.Equal(t, testRepo, obs.Access.Repo.FullName())
}

func TestObserve_ARequestItCannotDialMakesNoGitHubCall(t *testing.T) {
	gh := newFakeGitHub(t)

	obs, err := clusterrepo.Observe(ctx(), desired(withGitHubSecret("owner", "")), gh.dial)

	require.NoError(t, err)
	assert.Nil(t, obs.Access)
	assert.Empty(t, gh.madeCalls())
}

func TestObserve_AFailedGitHubReadIsAnError(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.answerWith("GET /repos/"+testRepo, http.StatusInternalServerError, "")

	_, err := clusterrepo.Observe(ctx(), desired(), gh.dial)

	assert.EqualError(t, err, "reading acme/shop-cluster: github returned 500")
}

func TestRepositoryObserver_ReadsTheMarkedRepositoryPresentWithItsName(t *testing.T) {
	cases := []struct {
		name    string
		desired []desiredOption
		hold    func(gh *fakeGitHub)
		want    contract.Observation
	}{
		{"a marked repository", nil, func(gh *fakeGitHub) { gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...) },
			contract.Observation{State: contract.Present, Outputs: map[string]string{"repo": testRepo}, StatusDetail: "the cluster repository acme/shop-cluster exists"}},
		{"no repository", nil, func(*fakeGitHub) {},
			contract.Observation{State: contract.Absent, StatusDetail: "the cluster repository acme/shop-cluster does not exist"}},
		{"an unmarked repository", nil, func(gh *fakeGitHub) { gh.holdRepo("team") },
			contract.Observation{State: contract.Absent, StatusDetail: "the repository acme/shop-cluster does not carry this subscription's mark, so it is not the cluster's"}},
		{"a request that names no app", []desiredOption{withParam("appName", "")}, func(*fakeGitHub) {},
			contract.Observation{State: contract.Unknown, StatusDetail: `the cluster repository cannot be named: params["appName"] is empty`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			tc.hold(gh)

			obs, err := clusterrepo.RepositoryObserver{Dial: gh.dial}.ObserveResource(ctx(), contract.ResourceRef{Desired: desired(tc.desired...)})

			require.NoError(t, err)
			assert.Equal(t, tc.want, obs)
		})
	}
}

func TestRepositoryObserver_AMarkedRepositoryItsCheckFaultsReadsAbsentNamingEachProblem(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...)
	check := func(_ context.Context, _ contract.Desired, a *clusterrepo.Access) ([]string, error) {
		return []string{a.Repo.FullName() + " lacks a key", "another key is read-only"}, nil
	}

	obs, err := clusterrepo.RepositoryObserver{Dial: gh.dial, Check: check}.ObserveResource(ctx(), contract.ResourceRef{Desired: desired()})

	require.NoError(t, err)
	assert.Equal(t, contract.Observation{State: contract.Absent, StatusDetail: "acme/shop-cluster lacks a key; another key is read-only"}, obs)
}

func TestRepositoryObserver_ACheckThatFailsIsAnErrorAndOneOnlyAMarkedRepositoryRuns(t *testing.T) {
	gh := newFakeGitHub(t)
	checked := 0
	check := func(context.Context, contract.Desired, *clusterrepo.Access) ([]string, error) {
		checked++
		return nil, errors.New("listing the deploy keys: github returned 502")
	}
	observer := clusterrepo.RepositoryObserver{Dial: gh.dial, Check: check}

	_, goneErr := observer.ObserveResource(ctx(), contract.ResourceRef{Desired: desired()})
	gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...)
	_, err := observer.ObserveResource(ctx(), contract.ResourceRef{Desired: desired()})

	require.NoError(t, goneErr)
	assert.EqualError(t, err, "listing the deploy keys: github returned 502")
	assert.Equal(t, 1, checked, "the check runs for the marked repository alone")
}

func TestSeedObserver_ReadsTheSeedPresentWhileTheMarkedRepositoryHoldsIt(t *testing.T) {
	cases := []struct {
		name    string
		desired []desiredOption
		hold    func(gh *fakeGitHub)
		want    contract.Observation
	}{
		{"a marked repository holding the seed", nil, func(gh *fakeGitHub) {
			gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...)
			gh.holdFile(clusterrepo.SeedMarkerFile, "seed")
		}, contract.Observation{State: contract.Present, StatusDetail: "the cluster repository acme/shop-cluster holds the seed"}},
		{"a marked repository without the seed", nil, func(gh *fakeGitHub) { gh.holdRepo(clusterrepo.Mark(testBlock, testSub)...) },
			contract.Observation{State: contract.Absent, StatusDetail: "the cluster repository acme/shop-cluster holds no cluster/README.md"}},
		{"no repository", nil, func(*fakeGitHub) {},
			contract.Observation{State: contract.Absent, StatusDetail: "the cluster repository acme/shop-cluster does not exist"}},
		{"an unmarked repository holding the seed", nil, func(gh *fakeGitHub) {
			gh.holdRepo("team")
			gh.holdFile(clusterrepo.SeedMarkerFile, "seed")
		}, contract.Observation{State: contract.Absent, StatusDetail: "the repository acme/shop-cluster does not carry this subscription's mark, so it is not the cluster's"}},
		{"a request without a GitHub owner", []desiredOption{withGitHubSecret("owner", "")}, func(*fakeGitHub) {},
			contract.Observation{State: contract.Unknown, StatusDetail: "the cluster repository cannot be read: the GitHub connection names no owner: " +
				"secrets.github.owner is empty, so the cluster repository has no account to live in; reconnect GitHub for this app and retry"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			tc.hold(gh)

			obs, err := clusterrepo.SeedObserver{Dial: gh.dial}.ObserveResource(ctx(), contract.ResourceRef{Desired: desired(tc.desired...)})

			require.NoError(t, err)
			assert.Equal(t, tc.want, obs)
		})
	}
}
