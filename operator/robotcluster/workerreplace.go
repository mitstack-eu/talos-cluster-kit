package robotcluster

import (
	"context"
	"fmt"
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

// heldWorkers names each Cloud worker of keys that the pool holds, as a validate answers it under replaces.
func heldWorkers(ctx context.Context, p controlplane.Plane, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	workers, err := p.API.ListServers(ctx, p.RoleSelector(pools.RoleCloudWorker))
	if err != nil {
		return nil, fmt.Errorf("reading the Cloud workers to name those an upgrade would replace: %w", err)
	}
	var names []string
	for _, key := range keys {
		if slices.ContainsFunc(workers, func(w controlplane.Server) bool { return w.Labels[pools.KeyMember] == key }) {
			names = append(names, MemberName(key))
		}
	}
	return names, nil
}
