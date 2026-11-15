package components

// The mesh probe's target in mesh/probe.yaml, which the node report calls from inside the mesh and its outside probe
// from kube-system.
const (
	MeshProbeNamespace = "mitstack-mesh-probe"
	// MeshProbeHost resolves to the target pod's own address, since its Service is headless.
	MeshProbeHost = "mitstack-mesh-probe." + MeshProbeNamespace + ".svc.cluster.local"
	MeshProbePort = 8080
	// MeshProbeUnadmittedPort is a port of the target where nothing listens and no policy admits a caller, so Cilium
	// drops a call to it before the mesh sees it.
	MeshProbeUnadmittedPort = 8081
	// MeshProbeIdentity is the SPIFFE identity of the certificate ztunnel holds for the target's service account.
	MeshProbeIdentity = "spiffe://cluster.local/ns/" + MeshProbeNamespace + "/sa/mitstack-mesh-probe"
	MeshProbeAnswer   = "mitstack-mesh-probe answered\n"
)
