// Package redundant carries a removal's redundant set to the guard the block passes, block-kit's
// redundancy.Set.AllowRemoval, which the kit does not import, so any block framework can drive it.
package redundant

import (
	"errors"
	"strings"
	"time"
)

// Reading is what one read of a member found, and when that read was taken.
type Reading struct {
	At      time.Time
	Serving bool
}

// Member is one member of a set with its newest reading, nil when nothing has read it.
type Member struct {
	Name    string
	Reading *Reading
}

// Minimum is how many members a set keeps serving, as block-kit's redundancy package names them.
type Minimum int

const (
	// Majority keeps more than half of the set serving, the quorum etcd and OpenBao need.
	Majority Minimum = iota + 1
	// AllButOne keeps every member but the one removed serving, and at least one.
	AllButOne
	// NoRedundancy lets the one member of a set of one go.
	NoRedundancy
)

// Set is a redundant set: the Name a refusal calls its members, each member's newest reading, and its minimum.
type Set struct {
	Name    string
	Members []Member
	Minimum Minimum
}

// Guard judges removing member from set by readings since stepStart as block-kit's redundancy.Set.AllowRemoval does,
// returning nil to allow it, its refusal unwrapped to hold it, and any other error for a set it cannot judge.
type Guard func(set Set, member string, stepStart time.Time) error

// ClassBelowMinimum is the class block-kit's guard refuses a removal with when it would leave a set below its minimum.
const ClassBelowMinimum = "redundancy-below-minimum"

// Allow asks g to judge the removal, and refuses every removal when the block passes no guard.
func (g Guard) Allow(set Set, member string, stepStart time.Time) error {
	if g == nil {
		return errors.New("the block passes no redundancy guard, so no member of the " + set.Name + " is removed")
	}
	return g(set, member, stepStart)
}

// Held reports whether err is the guard's unwrapped refusal, "refused:<class>: <detail>", which a later call may lift.
func Held(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "refused:"+ClassBelowMinimum+": ")
}
