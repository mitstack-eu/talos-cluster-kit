// Package redundanttest answers as block-kit v0.38.0's redundancy.Set.AllowRemoval does, for the kit's own tests.
package redundanttest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/redundant"
)

// Guard judges a removal as block-kit's guard does.
func Guard(set redundant.Set, member string, stepStart time.Time) error {
	if err := judgeable(set, member, stepStart); err != nil {
		return fmt.Errorf("redundancy: removing %q from the %s: %w", member, set.Name, err)
	}
	minimum, basis := len(set.Members)/2+1, fmt.Sprintf("a majority of %d", len(set.Members))
	switch set.Minimum {
	case redundant.AllButOne:
		minimum, basis = max(len(set.Members)-1, 1), "all but the one removed"
	case redundant.NoRedundancy:
		minimum, basis = 0, "no redundancy"
	}
	var left int
	var parts []string
	for _, m := range set.Members {
		if m.Name == member {
			continue
		}
		parts = append(parts, describe(m, stepStart))
		if m.Reading != nil && m.Reading.Serving && !m.Reading.At.Before(stepStart) {
			left++
		}
	}
	if left >= minimum {
		return nil
	}
	if len(parts) == 0 {
		parts = []string{"no other member"}
	}
	return fmt.Errorf("refused:%s: removing %q from the %s would leave %d serving, below their minimum of %d (%s), judged by readings since %s: %s",
		redundant.ClassBelowMinimum, member, set.Name, left, minimum, basis, stamp(stepStart), strings.Join(parts, "; "))
}

func judgeable(set redundant.Set, member string, stepStart time.Time) error {
	switch {
	case set.Minimum == 0:
		return errors.New("the set declares no minimum")
	case set.Minimum == redundant.NoRedundancy && len(set.Members) > 1:
		return fmt.Errorf("a set of %d members declares no redundancy, which only a set of one may", len(set.Members))
	case stepStart.IsZero():
		return errors.New("the step names no start, so no reading can be judged fresh")
	}
	seen := make(map[string]bool, len(set.Members))
	for _, m := range set.Members {
		if seen[m.Name] {
			return fmt.Errorf("the set names %q twice", m.Name)
		}
		seen[m.Name] = true
	}
	if !seen[member] {
		return errors.New("the set holds no such member")
	}
	return nil
}

func describe(m redundant.Member, stepStart time.Time) string {
	switch {
	case m.Reading == nil:
		return fmt.Sprintf("%q has no reading", m.Name)
	case m.Reading.At.Before(stepStart):
		return fmt.Sprintf("%q read at %s, before the step began", m.Name, stamp(m.Reading.At))
	case m.Reading.Serving:
		return fmt.Sprintf("%q read serving at %s", m.Name, stamp(m.Reading.At))
	default:
		return fmt.Sprintf("%q read not serving at %s", m.Name, stamp(m.Reading.At))
	}
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
