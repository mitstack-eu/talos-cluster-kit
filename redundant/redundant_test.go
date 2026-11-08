package redundant_test

// A removal goes only on the guard's yes, and waits on its refusal of a set below its minimum.

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mitstack-eu/talos-cluster-kit/redundant"
)

func TestGuard_RefusesEveryRemovalWhenTheBlockPassesNone(t *testing.T) {
	var guard redundant.Guard

	err := guard.Allow(redundant.Set{Name: "gateways", Minimum: redundant.AllButOne}, "gateway 1 in fsn1", time.Now())

	assert.EqualError(t, err, "the block passes no redundancy guard, so no member of the gateways is removed")
	assert.False(t, redundant.Held(err), "a missing guard is the block's own failure, which no later call fixes")
}

func TestHeld_ReadsOnlyTheGuardsRefusalOfASetBelowItsMinimum(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		held bool
	}{
		{"below its minimum", errors.New(`refused:redundancy-below-minimum: removing "cp-3" from the etcd members would leave 1 serving`), true},
		{"another refusal", errors.New("refused:gateway-ca-unknown: the gateway in hel1 is gone"), false},
		{"a call the guard cannot judge", errors.New(`redundancy: removing "cp-9" from the etcd members: the set holds no such member`), false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.held, redundant.Held(tc.err), "Held(%v)", tc.err)
		})
	}
}
