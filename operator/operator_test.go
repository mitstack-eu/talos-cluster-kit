package operator_test

// The start-up wiring and the request reads both Talos cluster operators share answer as each operator answered before.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/operator"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func refusalClass(t *testing.T, err error) string {
	t.Helper()
	var refused *handler.Refused
	require.ErrorAs(t, err, &refused, "the error is a block-kit refusal: %v", err)
	return refused.Class
}

func TestReadSettings_ListensOnTheAddressItIsGivenOrPort8080(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want operator.Settings
	}{
		{
			name: "an operator given no LISTEN_ADDR listens on port 8080",
			env:  map[string]string{"MITSTACK_JWKS_URL": "https://api.mitstack.test/.well-known/jwks.json"},
			want: operator.Settings{JWKSURL: "https://api.mitstack.test/.well-known/jwks.json", Addr: ":8080"},
		},
		{
			name: "an operator given a LISTEN_ADDR listens there",
			env:  map[string]string{"MITSTACK_JWKS_URL": "https://api.mitstack.test/jwks", "LISTEN_ADDR": ":9090"},
			want: operator.Settings{JWKSURL: "https://api.mitstack.test/jwks", Addr: ":9090"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := operator.ReadSettings(env(tc.env))

			require.NoError(t, err)
			assert.Equal(t, tc.want, got, "ReadSettings(%v)", tc.env)
		})
	}
}

func TestReadSettings_RefusesToStartWithoutTheJWKSURLTheTrustBoundaryReadsItsKeyFrom(t *testing.T) {
	_, err := operator.ReadSettings(env(map[string]string{"LISTEN_ADDR": ":9090"}))

	require.Error(t, err)
	assert.EqualError(t, err, "MITSTACK_JWKS_URL is required: the trust boundary resolves mitstack's signing key from it")
}

func TestNewServer_ServesTheMuxOnTheAddressAndBoundsTheTimeToReadARequestsHeaders(t *testing.T) {
	srv := operator.NewServer(":9090", nil)

	assert.Equal(t, ":9090", srv.Addr)
	assert.Equal(t, 10*time.Second, srv.ReadHeaderTimeout, "the server bounds the time a client may take to send its headers")
}

func TestUnknown_ReadsUnknownWithTheDetail(t *testing.T) {
	obs := operator.Unknown("no Hetzner token on this call")

	assert.Equal(t, contract.Observation{State: contract.Unknown, StatusDetail: "no Hetzner token on this call"}, obs)
}

func TestRefusalDetail_DropsTheRefusedPrefixAValidateAnswerDoesNotCarry(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a refusal gives its detail alone", handler.Refuse("missing-param", "the app name is required"), "the app name is required"},
		{"a wrapped refusal gives its detail alone", fmt.Errorf("dialling: %w", handler.Refuse("missing-param", "no owner")), "no owner"},
		{"any other error gives its whole text", errors.New("github unreachable"), "github unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, operator.RefusalDetail(tc.err), "RefusalDetail(%v)", tc.err)
		})
	}
}

func TestObserverFunc_AnswersTheObservationOfTheRequestItIsHanded(t *testing.T) {
	observe := operator.ObserverFunc(func(_ context.Context, d contract.Desired) (contract.Observation, error) {
		return operator.Unknown("observed " + d.SubscriptionID), nil
	})
	var _ handler.Observer = observe

	obs, err := observe.ObserveResource(context.Background(), contract.ResourceRef{Desired: contract.Desired{Identity: contract.Identity{SubscriptionID: "sub_1"}}})

	require.NoError(t, err)
	assert.Equal(t, "observed sub_1", obs.StatusDetail)
}

func TestHetznerToken_RefusesARequestWithoutTheHetznerConnection(t *testing.T) {
	_, err := operator.HetznerToken(contract.Desired{})

	assert.Equal(t, "missing-hetzner-access", refusalClass(t, err))
	assert.ErrorContains(t, err, "secrets.hetzner.api_token is empty, so the cluster cannot be reached")
}

func TestHetznerToken_ReadsTheConnectionsToken(t *testing.T) {
	token, err := operator.HetznerToken(contract.Desired{Secrets: map[string]map[string]string{"hetzner": {"api_token": "hc-token"}}})

	require.NoError(t, err)
	assert.Equal(t, "hc-token", token)
}

func TestAppName_RefusesARequestThatNamesNoApp(t *testing.T) {
	_, err := operator.AppName(contract.Desired{})

	assert.Equal(t, "missing-param", refusalClass(t, err))
	assert.ErrorContains(t, err, `the app name is required: params["appName"] is empty`)
}

func TestAppName_ReadsTheAppTheRequestNames(t *testing.T) {
	name, err := operator.AppName(contract.Desired{Params: map[string]string{"appName": "shop"}})

	require.NoError(t, err)
	assert.Equal(t, "shop", name)
}

func TestRefuseUnreleasedTools_RefusesTheToolsImagePlaceholderAnUnreleasedKitCarries(t *testing.T) {
	cases := []struct {
		name, image, class string
	}{
		{"the placeholder between releases is refused", "ghcr.io/mitstack-eu/talos-cluster-kit-tools:unreleased@sha256:" + zeros, "tools-image-unreleased"},
		{"a released image passes", "ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.81.0@sha256:" + zeros, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := operator.RefuseUnreleasedTools(tc.image)

			if tc.class == "" {
				assert.NoError(t, err, "RefuseUnreleasedTools(%s)", tc.image)
				return
			}
			assert.Equal(t, tc.class, refusalClass(t, err))
			assert.ErrorContains(t, err, tc.image)
		})
	}
}

const zeros = "0000000000000000000000000000000000000000000000000000000000000000"
