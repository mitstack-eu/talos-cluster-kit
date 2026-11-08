package controlplane

// A server records when the step removing it began, and for which plan, since no call outlives the report it waits for.

import (
	"maps"
	"strconv"
	"time"
)

const (
	// KeyRemovalBegan on a server is when the step that removes it began, in Unix seconds.
	KeyRemovalBegan = "mitstack.dev/removal-began"
	// KeyRemovalFor on a server names the plan its removal serves, such as the image list an upgrade replaces it for.
	KeyRemovalFor = "mitstack.dev/removal-for"
)

// RemovalBegan is when the step removing s for plan began, false while none has, or when one began for another plan.
func RemovalBegan(s Server, plan string) (time.Time, bool) {
	unix, err := strconv.ParseInt(s.Labels[KeyRemovalBegan], 10, 64)
	if err != nil || s.Labels[KeyRemovalFor] != plan {
		return time.Time{}, false
	}
	return time.Unix(unix, 0).UTC(), true
}

// BeginRemoval returns s's labels once its removal for plan began at at, rounded up to a whole second.
func BeginRemoval(s Server, at time.Time, plan string) map[string]string {
	labels := maps.Clone(s.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	unix := at.Unix()
	if at.Nanosecond() > 0 {
		unix++
	}
	labels[KeyRemovalBegan], labels[KeyRemovalFor] = strconv.FormatInt(unix, 10), plan
	return labels
}

// EndRemoval returns s's labels with no removal under way.
func EndRemoval(s Server) map[string]string {
	labels := maps.Clone(s.Labels)
	delete(labels, KeyRemovalBegan)
	delete(labels, KeyRemovalFor)
	return labels
}
