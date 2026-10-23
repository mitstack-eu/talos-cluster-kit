package components_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestMesh_KeepsTheOperatorAloneOnAnInstancesManagerPortAndEveryoneOffItsOtherPorts(t *testing.T) {
	policy := namedDocument(t, manifest(t, "mitstack-mesh-cloudnative-pg"), "AuthorizationPolicy", "mitstack-database-instance-ports")
	operator, slice := "cluster.local/ns/cnpg-system/sa/cloudnative-pg", "cluster.local/ns/app-a1/sa/default"

	assert.Equal(t, "istio-system", testutil.Field(t, policy, "metadata", "namespace"), "a policy outside Istio's root namespace binds only its own")
	assert.False(t, testutil.Denies(t, policy, operator, 8000), "the operator's call to the instance manager")
	assert.False(t, testutil.Denies(t, policy, slice, 5432), "a slice's call to Postgres")
	assert.True(t, testutil.Denies(t, policy, slice, 8000), "a slice's call to the instance manager")
	assert.True(t, testutil.Denies(t, policy, operator, 9187), "a call to a port the slice policy does not name")
}
