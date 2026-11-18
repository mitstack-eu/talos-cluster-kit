package operator

import (
	"slices"
	"strconv"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
)

const (
	// ParamControlPlaneSize is the hetzner.kubernetes-cluster input naming how many control-plane servers a cluster runs.
	ParamControlPlaneSize = "controlPlaneSize"
	// DefaultControlPlaneSize is the size of a request that names none, which every cluster built before the input has.
	DefaultControlPlaneSize = 3
	paramHostsPlatform      = "hostsPlatform"
)

// ControlPlaneSizes are the sizes a control plane runs: one etcd member, or three that keep a quorum while one is down.
var ControlPlaneSizes = []int{1, 3}

// ControlPlaneSize reads the size the request names, refusing any size but 1 or 3 and one server for a cluster that
// hosts the platform, since that cluster runs OpenBao, whose data no tested restore yet covers on a single server.
func ControlPlaneSize(d contract.Desired) (int, error) {
	raw := d.Params[ParamControlPlaneSize]
	if raw == "" {
		return DefaultControlPlaneSize, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || !slices.Contains(ControlPlaneSizes, size) {
		return 0, handler.Refuse("invalid-param", `params["%s"] is %q, and a control plane runs 1 or 3 servers`, ParamControlPlaneSize, raw)
	}
	if size == 1 && d.Params[paramHostsPlatform] == "true" {
		return 0, handler.Refuse("restore-drill-missing",
			`params["%s"] is 1 and params["%s"] is true, and a cluster that hosts the platform runs OpenBao, whose data a single server holds alone; it runs on one server only once an ADR 0125 restore drill has passed for it, and none has, so subscribe with 3`,
			ParamControlPlaneSize, paramHostsPlatform)
	}
	return size, nil
}

// RefuseUndeclaredSize refuses a size other than the one a block's manifest declares a node and a gateway for, since
// mitstack waits until every declared resource reads present, so a cluster of another size would never finish.
func RefuseUndeclaredSize(size, declared int) error {
	if size == declared {
		return nil
	}
	return handler.Refuse("control-plane-size-undeclared",
		"this block lists a control-plane server in each of %d locations, and mitstack waits for each one, so a cluster of %d servers would never finish; every cluster this block builds keeps %d servers after subscribe, so leave controlPlaneSize at %d",
		declared, size, declared, declared)
}
