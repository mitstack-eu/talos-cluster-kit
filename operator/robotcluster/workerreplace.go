package robotcluster

import (
	"slices"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

// namedWorkers are the member keys of the Cloud workers replace names, which only the upgrade's own call carries.
func namedWorkers(replace []string) (keys []string) {
	for _, name := range replace {
		if key := memberKey(name); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func heldWorkers(st controlplane.State, keys []string) (names []string) {
	for _, key := range keys {
		if slices.ContainsFunc(st.Servers[pools.RoleCloudWorker], func(w controlplane.Server) bool { return w.Labels[pools.KeyMember] == key }) {
			names = append(names, MemberName(key))
		}
	}
	return names
}
