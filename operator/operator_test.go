package operator_test

// The start-up wiring and the request reads both Talos cluster operators share.

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
	jwks := "https://api.mitstack.test/jwks"

	byDefault, err := operator.ReadSettings(env(map[string]string{"MITSTACK_JWKS_URL": jwks}))
	require.NoError(t, err)
	given, err := operator.ReadSettings(env(map[string]string{"MITSTACK_JWKS_URL": jwks, "LISTEN_ADDR": ":9090"}))
	require.NoError(t, err)

	assert.Equal(t, operator.Settings{JWKSURL: jwks, Addr: ":8080"}, byDefault, "an operator given no LISTEN_ADDR")
	assert.Equal(t, operator.Settings{JWKSURL: jwks, Addr: ":9090"}, given, "an operator given a LISTEN_ADDR")
}

func TestReadSettings_RefusesToStartWithoutTheJWKSURLTheTrustBoundaryReadsItsKeyFrom(t *testing.T) {
	_, err := operator.ReadSettings(env(map[string]string{"LISTEN_ADDR": ":9090"}))

	assert.EqualError(t, err, "MITSTACK_JWKS_URL is required: the trust boundary resolves mitstack's signing key from it")
}

func TestNewServer_ServesTheMuxOnTheAddressAndBoundsTheTimeToReadARequestsHeaders(t *testing.T) {
	srv := operator.NewServer(":9090", nil)

	assert.Equal(t, ":9090", srv.Addr)
	assert.Equal(t, 10*time.Second, srv.ReadHeaderTimeout, "the server bounds the time a client may take to send its headers")
}

func TestRefusalDetail_DropsTheRefusedPrefixAValidateAnswerDoesNotCarry(t *testing.T) {
	assert.Equal(t, "the app name is required", operator.RefusalDetail(handler.Refuse("missing-param", "the app name is required")))
	assert.Equal(t, "no owner", operator.RefusalDetail(fmt.Errorf("dialling: %w", handler.Refuse("missing-param", "no owner"))))
	assert.Equal(t, "github unreachable", operator.RefusalDetail(errors.New("github unreachable")), "any other error gives its whole text")
}

func TestObserverFunc_AnswersTheUnknownObservationOfTheRequestItIsHanded(t *testing.T) {
	var observe handler.Observer = operator.ObserverFunc(func(_ context.Context, d contract.Desired) (contract.Observation, error) {
		return operator.Unknown("observed " + d.SubscriptionID), nil
	})

	obs, err := observe.ObserveResource(context.Background(), contract.ResourceRef{Desired: contract.Desired{Identity: contract.Identity{SubscriptionID: "sub_1"}}})

	require.NoError(t, err)
	assert.Equal(t, contract.Observation{State: contract.Unknown, StatusDetail: "observed sub_1"}, obs)
}

func TestHetznerToken_ReadsTheConnectionsTokenAndRefusesARequestWithoutIt(t *testing.T) {
	token, err := operator.HetznerToken(contract.Desired{Secrets: map[string]map[string]string{"hetzner": {"api_token": "hc-token"}}})
	require.NoError(t, err)
	_, missing := operator.HetznerToken(contract.Desired{})

	assert.Equal(t, "hc-token", token)
	assert.Equal(t, "missing-hetzner-access", refusalClass(t, missing))
	assert.ErrorContains(t, missing, "secrets.hetzner.api_token is empty, so the cluster cannot be reached")
}

func TestAppName_ReadsTheAppTheRequestNamesAndRefusesARequestThatNamesNone(t *testing.T) {
	name, err := operator.AppName(contract.Desired{Params: map[string]string{"appName": "shop"}})
	require.NoError(t, err)
	_, missing := operator.AppName(contract.Desired{})

	assert.Equal(t, "shop", name)
	assert.Equal(t, "missing-param", refusalClass(t, missing))
	assert.ErrorContains(t, missing, `the app name is required: params["appName"] is empty`)
}

func TestRefuseUnreleasedTools_RefusesOnlyTheToolsImagePlaceholderAnUnreleasedKitCarries(t *testing.T) {
	const digest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	placeholder := "ghcr.io/mitstack-eu/talos-cluster-kit-tools:unreleased" + digest

	err := operator.RefuseUnreleasedTools(placeholder)

	assert.Equal(t, "tools-image-unreleased", refusalClass(t, err))
	assert.ErrorContains(t, err, placeholder)
	assert.NoError(t, operator.RefuseUnreleasedTools("ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.81.0"+digest), "a released image")
}

func TestRequestSecrets_ListsEverySecretTheRequestCarriesSoALogLineCanMaskThem(t *testing.T) {
	d := contract.Desired{
		Secrets:      map[string]map[string]string{"hetzner": {"api_token": "hc-token"}, "github": {"token": "gh-token"}},
		SecretInputs: map[string]string{"password": "input-secret"},
		OwnSecrets:   map[string]string{"gatewayCaKey": "own-secret"},
	}

	secrets := operator.RequestSecrets(d)

	assert.ElementsMatch(t, []string{"hc-token", "gh-token", "input-secret", "own-secret"}, secrets)
	assert.Empty(t, operator.RequestSecrets(contract.Desired{}), "a request that carries no secret")
}

func sizeRequest(params map[string]string) contract.Desired { return contract.Desired{Params: params} }

func TestControlPlaneSize_ReadsOneOrThreeAndThreeForARequestThatNamesNone(t *testing.T) {
	for _, tc := range []struct {
		name, given string
		want        int
	}{
		{name: "a request naming one runs one server", given: "1", want: 1},
		{name: "a request naming three runs three servers", given: "3", want: 3},
		{name: "a request naming no size, as every cluster built before the input, runs three servers", given: "", want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, err := operator.ControlPlaneSize(sizeRequest(map[string]string{"controlPlaneSize": tc.given}))

			require.NoError(t, err)
			assert.Equal(t, tc.want, size, "ControlPlaneSize(%q)", tc.given)
		})
	}
}

func TestControlPlaneSize_RefusesAnyOtherSizeNamingOneAndThree(t *testing.T) {
	for _, given := range []string{"2", "0", "4", "-1", "three", " 3", "3.0"} {
		t.Run("a request naming "+given, func(t *testing.T) {
			_, err := operator.ControlPlaneSize(sizeRequest(map[string]string{"controlPlaneSize": given}))

			assert.Equal(t, "invalid-param", refusalClass(t, err))
			assert.ErrorContains(t, err, fmt.Sprintf(`params["controlPlaneSize"] is %q, and a control plane runs 1 or 3 servers`, given))
		})
	}
}

func TestControlPlaneSize_RefusesOneServerForAClusterRunningOpenBaoNamingTheRestoreDrill(t *testing.T) {
	_, refused := operator.ControlPlaneSize(sizeRequest(map[string]string{"controlPlaneSize": "1", "hostsPlatform": "true"}))
	three, err := operator.ControlPlaneSize(sizeRequest(map[string]string{"controlPlaneSize": "3", "hostsPlatform": "true"}))
	require.NoError(t, err, "three servers for a cluster running OpenBao")

	assert.Equal(t, "restore-drill-missing", refusalClass(t, refused))
	assert.ErrorContains(t, refused, "ADR 0125 restore drill")
	assert.ErrorContains(t, refused, "OpenBao")
	assert.Equal(t, 3, three, "the size of a three-server cluster running OpenBao")
}

func TestRefuseUndeclaredSize_RefusesASizeTheBlocksManifestDeclaresNoNodesForNamingTheDeclaredSize(t *testing.T) {
	declared := operator.RefuseUndeclaredSize(3, 3)
	undeclared := operator.RefuseUndeclaredSize(1, 3)

	assert.NoError(t, declared, "the one size the manifest declares")
	assert.Equal(t, "control-plane-size-undeclared", refusalClass(t, undeclared))
	assert.ErrorContains(t, undeclared, "this block lists a control-plane server in each of 3 locations, and mitstack waits for each one")
	assert.ErrorContains(t, undeclared, "keeps 3 servers after subscribe")
	assert.ErrorContains(t, undeclared, "leave controlPlaneSize at 3")
}
