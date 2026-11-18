package robotcluster

import (
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

const cockpitRequirement = "scaleway.cockpit"

// withTelemetry has the plane ship the cluster's telemetry to the Cockpit d binds, which the lease job leases its push token of.
func withTelemetry(p controlplane.Plane, d contract.Desired) controlplane.Plane {
	outputs, bound := d.Bindings[cockpitRequirement]
	if !bound {
		return p
	}
	p.Telemetry = components.TelemetryExport{
		Resource:        cockpitRequirement,
		MetricsEndpoint: outputs["otlpMetricsEndpoint"],
		LogsEndpoint:    outputs["otlpLogsEndpoint"],
		TokenHeader:     outputs["otlpTokenHeader"],
	}
	return p
}
