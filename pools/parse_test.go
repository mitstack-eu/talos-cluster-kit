package pools_test

import (
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

const robotAccount = "inst_robot_main"

// params is a valid pair of pools, changed by edit.
func params(edit map[string]string) map[string]string {
	p := map[string]string{
		"dedicatedPool":        `[{"serverNumber":"3041839","robotAccount":"` + robotAccount + `"}]`,
		"cloudPoolServerTypes": "cpx32, cx43",
		"cloudPoolLocations":   "fsn1, nbg1",
		"cloudPoolMin":         "2",
		"cloudPoolMax":         "2",
	}
	maps.Copy(p, edit)
	return p
}

func TestParse_ReadsBothPoolsAsTheCataloguePassesThem(t *testing.T) {
	got, err := pools.Parse(params(nil))

	require.NoError(t, err)
	assert.Equal(t, pools.Pools{
		Dedicated: []pools.Dedicated{{ServerNumber: 3041839, RobotAccount: robotAccount}},
		Cloud:     pools.CloudPool{ServerTypes: []string{"cpx32", "cx43"}, Locations: []string{"fsn1", "nbg1"}, Min: 2, Max: 2},
	}, got)
}

func TestParse_APoolWhoseMinimumIsBelowItsMaximumScalesWithItsIdleWindow(t *testing.T) {
	got, err := pools.Parse(params(map[string]string{"cloudPoolMin": "0", "cloudPoolMax": "3", "cloudPoolIdleMinutes": "15"}))

	require.NoError(t, err)
	assert.Equal(t, pools.CloudPool{ServerTypes: []string{"cpx32", "cx43"}, Locations: []string{"fsn1", "nbg1"}, Min: 0, Max: 3, IdleWindow: 15 * time.Minute}, got.Cloud)
	assert.True(t, got.Cloud.Scales(), "whether a pool of 0 to 3 scales")
}

func TestParse_APoolOfASetSizeDoesNotScale(t *testing.T) {
	got, err := pools.Parse(params(nil))

	require.NoError(t, err)
	assert.False(t, got.Cloud.Scales(), "whether a pool of 2 to 2 scales")
}

func TestParse_ASubscriptionWithNoPoolInputsHoldsNoPool(t *testing.T) {
	got, err := pools.Parse(map[string]string{})

	require.NoError(t, err)
	assert.Equal(t, pools.Pools{}, got)
}

func TestParse_ACloudPoolOfSizeZeroIsValid(t *testing.T) {
	got, err := pools.Parse(params(map[string]string{"cloudPoolMin": "0", "cloudPoolMax": "0"}))

	require.NoError(t, err)
	assert.Zero(t, got.Cloud.Max, "the pool's maximum")
	assert.Equal(t, []string{"fsn1", "nbg1"}, got.Cloud.Locations, "the pool keeps its locations")
}

func TestParse_RefusesEachInvalidPoolNamingTheInput(t *testing.T) {
	cases := []struct {
		name string
		edit map[string]string
		want string
	}{
		{"a location Hetzner does not offer", map[string]string{"cloudPoolLocations": "fsn1, ams3"}, `cloudPoolLocations: "ams3"`},
		{"a location listed twice", map[string]string{"cloudPoolLocations": "fsn1, fsn1"}, `cloudPoolLocations: "fsn1" is listed twice`},
		{"no location", map[string]string{"cloudPoolLocations": ""}, "cloudPoolLocations: a Cloud pool needs at least one location"},
		{"a server type the platform does not allow", map[string]string{"cloudPoolServerTypes": "cx43, ccx13"}, `cloudPoolServerTypes: "ccx13"`},
		{"no server type", map[string]string{"cloudPoolServerTypes": ""}, "cloudPoolServerTypes: a Cloud pool needs at least one server type"},
		{"a negative minimum", map[string]string{"cloudPoolMin": "-1"}, `cloudPoolMin: "-1"`},
		{"a negative maximum", map[string]string{"cloudPoolMax": "-2"}, `cloudPoolMax: "-2"`},
		{"a size that is no number", map[string]string{"cloudPoolMax": "two"}, `cloudPoolMax: "two"`},
		{"no maximum", map[string]string{"cloudPoolMax": ""}, "cloudPoolMax: a Cloud pool needs a maximum"},
		{"a maximum below the minimum", map[string]string{"cloudPoolMin": "3", "cloudPoolMax": "2"}, "cloudPoolMax 2 is below cloudPoolMin 3"},
		{"a pool that scales with no idle window", map[string]string{"cloudPoolMin": "0", "cloudPoolMax": "3"}, "cloudPoolIdleMinutes: a Cloud pool whose minimum is below its maximum needs an idle window"},
		{"an idle window of no minutes", map[string]string{"cloudPoolMin": "0", "cloudPoolMax": "3", "cloudPoolIdleMinutes": "0"}, `cloudPoolIdleMinutes: "0"`},
		{"an idle window past the reports the platform keeps", map[string]string{"cloudPoolMin": "0", "cloudPoolMax": "3", "cloudPoolIdleMinutes": "121"}, `cloudPoolIdleMinutes: "121"`},
		{"an idle window that is no number", map[string]string{"cloudPoolMin": "0", "cloudPoolMax": "3", "cloudPoolIdleMinutes": "ten"}, `cloudPoolIdleMinutes: "ten"`},
		{"a dedicated pool that is no list", map[string]string{"dedicatedPool": `{"serverNumber":"3041839"}`}, "dedicatedPool: "},
		{"a dedicated entry with no server number", map[string]string{"dedicatedPool": `[{"robotAccount":"inst_robot_main"}]`}, "dedicatedPool entry 1: serverNumber is required"},
		{"a dedicated entry with a server number of zero", map[string]string{"dedicatedPool": `[{"serverNumber":"0","robotAccount":"inst_robot_main"}]`}, `dedicatedPool entry 1: serverNumber "0"`},
		{"a dedicated entry with no Robot account", map[string]string{"dedicatedPool": `[{"serverNumber":"3041839"}]`}, "dedicatedPool entry 1: robotAccount is required"},
		{"a server listed twice", map[string]string{"dedicatedPool": `[{"serverNumber":"7","robotAccount":"a"},{"serverNumber":"7","robotAccount":"b"}]`}, "dedicatedPool entry 2: server 7 is listed twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pools.Parse(params(tc.edit))

			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestParse_NamesEveryInvalidInputInOneRefusal(t *testing.T) {
	_, err := pools.Parse(params(map[string]string{"cloudPoolLocations": "ams3", "cloudPoolMin": "-1", "dedicatedPool": `[{}]`}))

	require.Error(t, err)
	for _, input := range []string{"cloudPoolLocations", "cloudPoolMin", "dedicatedPool entry 1"} {
		assert.ErrorContains(t, err, input)
	}
}

func TestCheckSteadyHome_RefusesAPoolThatScalesFromZeroWhenNoNodeOutsideItTakesTheKitsComponents(t *testing.T) {
	fromZero := pools.CloudPool{ServerTypes: []string{"cpx32"}, Locations: []string{"fsn1"}, Min: 0, Max: 3, IdleWindow: 15 * time.Minute}
	fromOne := fromZero
	fromOne.Min = 1
	cases := []struct {
		name   string
		plane  controlplane.Plane
		pool   pools.CloudPool
		refuse bool
	}{
		{"apps on workers with no worker outside the pool and a pool from 0", controlplane.Plane{AppsOnWorkers: true}, fromZero, true},
		{"apps on workers with no worker outside the pool and a pool from 1", controlplane.Plane{AppsOnWorkers: true}, fromOne, false},
		{"apps on workers with a Robot worker and a pool from 0", controlplane.Plane{AppsOnWorkers: true, WorkerAddresses: []string{"10.0.1.2"}}, fromZero, false},
		{"apps on the control plane and a pool from 0", controlplane.Plane{}, fromZero, false},
		{"apps on workers with no worker outside the pool and no pool", controlplane.Plane{AppsOnWorkers: true}, pools.CloudPool{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pools.CheckSteadyHome(tc.plane, tc.pool)

			if !tc.refuse {
				assert.NoError(t, err)
				return
			}
			var refusal *controlplane.Refusal
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, "cloud-pool-no-steady-home", refusal.Class)
			assert.Contains(t, refusal.Message, "the kit's components need a steady member or a node outside the pool")
		})
	}
}
