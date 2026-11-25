package pools_test

// A server type offers a pod the least that the serving, uncordoned nodes of the pool's members of it report.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/pools"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

func poolMember(name, serverType string) controlplane.Server {
	return controlplane.Server{Name: name, ServerType: serverType, Labels: map[string]string{pools.KeyMember: name}}
}

// reported is a node report of nodes, each a node's fields as the report carries them.
func reported(t *testing.T, nodes ...map[string]any) report.Read {
	t.Helper()
	body, err := json.Marshal(map[string]any{"nodes": nodes, "pods": []any{}})
	require.NoError(t, err)
	return report.Read{Body: body, At: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
}

func offering(name, cpu, memory string) map[string]any {
	return map[string]any{"name": name, "ready": true, "unschedulable": false, "allocatable": map[string]string{"cpu": cpu, "memory": memory, "pods": "110"}}
}

func TestMeasuredBy_TakesEachTypesLeastRoomFromItsMembersServingNodesAndNoneFromANodeNotReadyCordonedPressedOrLeaving(t *testing.T) {
	leaving := poolMember("w5", "cx43")
	leaving.Labels = controlplane.BeginRemoval(leaving, time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC), "cloud-pool")
	members := []controlplane.Server{poolMember("w1", "cx23"), poolMember("w2", "cx23"), poolMember("w3", "cx33"), poolMember("w4", "cx33"),
		leaving, poolMember("w6", "cx53"), poolMember("w7", "cx53"), poolMember("w8", "cpx22")}
	notReady, cordoned, pressed := offering("w6", "15950m", "31Gi"), offering("w4", "3000m", "6000Mi"), offering("w7", "15950m", "31Gi")
	notReady["ready"], cordoned["unschedulable"] = false, true
	pressed["taints"] = []map[string]any{{"key": "node.kubernetes.io/disk-pressure", "effect": "NoSchedule"}}
	read := reported(t,
		offering("cp1", "1950m", "3615Mi"),
		offering("w1", "1950m", "3600Mi"), offering("w2", "1900m", "3500Mi"),
		offering("w3", "3950m", "7243Mi"), cordoned,
		offering("w5", "7950m", "15Gi"),
		notReady, pressed,
		map[string]any{"name": "w8", "ready": true})

	measured, err := pools.MeasuredBy(members, read)

	require.NoError(t, err)
	assert.Equal(t, pools.Measured{
		"cx23": {MilliCPU: 1900, Memory: 3500 << 20},
		"cx33": {MilliCPU: 3950, Memory: 7243 << 20},
	}, measured, "the room of cx23 by the least of two nodes, of cx33 by its uncordoned node, and of no type whose only nodes are leaving, not Ready, under disk pressure or silent")
}

func TestMeasuredBy_FailsOnAFigureThatIsNoQuantityNamingTheNode(t *testing.T) {
	_, err := pools.MeasuredBy([]controlplane.Server{poolMember("w1", "cx23")}, reported(t, offering("w1", "lots", "3500Mi")))

	assert.ErrorContains(t, err, "w1", "the error names the node whose figure cannot be read")
}
