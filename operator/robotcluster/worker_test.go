package robotcluster

// The Robot server a worker is installed over is refused before any write when another subscription named it, and a
// worker's firewall is read as held only while active with every rule the cluster gives it.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdmits_RefusesAServerAnotherSubscriptionNamedAndOneMissingCancelledOrOutsideTheClusterLocations(t *testing.T) {
	fake := robottest.New(t)
	fake.AddServer(robottest.Server{Number: 1, Name: RobotName("sub_ours"), DC: "FSN1-DC14"})
	fake.AddServer(robottest.Server{Number: 2, Name: "", DC: "HEL1-DC2"})
	fake.AddServer(robottest.Server{Number: 3, Name: "rack-7", DC: "NBG1-DC3"})
	fake.AddServer(robottest.Server{Number: 4, Name: RobotName("sub_theirs"), DC: "FSN1-DC14"})
	fake.AddServer(robottest.Server{Number: 5, Name: "", DC: "FSN1-DC14", Cancelled: true})
	fake.AddServer(robottest.Server{Number: 6, Name: "", DC: "ASH-DC1"})

	cases := []struct {
		server  int64
		refusal string
		words   string
	}{
		{server: 1},
		{server: 2},
		{server: 3},
		{server: 4, refusal: "server-taken", words: "dedicated server 4 is named mitstack-sub-theirs, the name of another mitstack subscription, so nothing was changed"},
		{server: 5, refusal: "server-cancelled", words: "dedicated server 5 is cancelled, so no worker is installed over it and nothing was changed"},
		{server: 6, refusal: "location-unsupported", words: "dedicated server 6 is in ASH-DC1, and a worker runs only in fsn1, nbg1 or hel1"},
		{server: 7, refusal: "server-not-found", words: "Robot holds no dedicated server 7 in this account, so nothing was changed"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.server), func(t *testing.T) {
			err := robotTarget{robot: fake.Client(), server: c.server, name: RobotName("sub_ours")}.admits(context.Background())
			if c.refusal == "" {
				assert.NoError(t, err, "a server named for this subscription, unnamed or named outside mitstack is admitted")
				return
			}
			var refused *handler.Refused
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, c.refusal, refused.Class)
			assert.Contains(t, refused.Detail, c.words)
		})
	}
	assert.Empty(t, fake.Writes(), "admitting or refusing a server writes nothing to Robot")
}

func TestWaitOutRobotLimit_AnswersRobotsRequestLimitAsAWaitAndPassesAnyOtherErrorOn(t *testing.T) {
	var waiting *handler.Waiting
	require.ErrorAs(t, WaitOutRobotLimit(fmt.Errorf("listing vSwitches: %w", robot.ErrRateLimited)), &waiting)
	assert.Contains(t, waiting.Error(), "Robot answered 403 RATE_LIMIT_EXCEEDED")

	other := errors.New("reading dedicated server 1: connection reset")
	assert.Same(t, other, WaitOutRobotLimit(other))
	assert.NoError(t, WaitOutRobotLimit(nil))
}

func TestHoldsWorkerRules_ReadsAFirewallAsHeldOnlyWhileActiveWithEveryRuleTheClusterGivesIt(t *testing.T) {
	want := robot.Firewall{Status: "active", Input: []robot.Rule{
		{Name: "cluster network", SrcIP: "10.0.0.0/16", Action: "accept"},
		{Name: "rescue ssh", SrcIP: "198.51.100.7/32", Action: "accept"},
	}}
	extra := append([]robot.Rule{{Name: "icmp", Action: "accept"}}, want.Input...)

	assert.True(t, HoldsWorkerRules(want, want), "the firewall the cluster wrote")
	assert.True(t, HoldsWorkerRules(robot.Firewall{Status: "active", Input: extra}, want), "a firewall holding a rule beside the cluster's")
	assert.False(t, HoldsWorkerRules(robot.Firewall{Status: "in process", Input: want.Input}, want), "a firewall Robot is still applying")
	assert.False(t, HoldsWorkerRules(robot.Firewall{Status: "active", Input: want.Input[:1]}, want), "a firewall missing the rescue rule")
	changed := []robot.Rule{want.Input[0], {Name: "rescue ssh", SrcIP: "203.0.113.9/32", Action: "accept"}}
	assert.False(t, HoldsWorkerRules(robot.Firewall{Status: "active", Input: changed}, want), "a rule admitting another address")
}
