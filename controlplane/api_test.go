package controlplane_test

// The adapter on the wire: a node with no user data and no public address, a private load balancer with no public
// interface, a server Hetzner cannot place refused, a snapshot started unwaited, a start read from its action, a
// server's metrics read a minute a point, and a worker targeted by its address.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const finishedAction = `{"id": 1, "command": "x", "status": "success", "progress": 100, "resources": []}`

// recordHetzner answers each path with its canned body and records each request body by method and path.
func recordHetzner(t *testing.T, answers map[string]string) (controlplane.API, map[string]map[string]any) {
	t.Helper()
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies[r.Method+" "+r.URL.Path] = body
		answer, ok := answers[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": "not_found", "message": "not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return controlplane.NewAPIAt(srv.URL, "token", "mitstack-test"), bodies
}

// nodes is the selector a load balancer finds a plane's nodes by.
var nodes = controlplane.Plane{Block: "a-block", Subscription: "sub_0123456789ab"}.RoleSelector(controlplane.RoleNode)

func TestHetznerAdapter_CreatesTheNodeWithNoUserDataAndNoPublicAddress(t *testing.T) {
	api, bodies := recordHetzner(t, map[string]string{
		"POST /servers": `{"server": {"id": 7, "name": "node", "status": "initializing", "public_net": {}, "private_net": []},
			"action": ` + finishedAction + `, "next_actions": []}`,
	})

	_, err := api.CreateServers(context.Background(), controlplane.ServerSpec{
		Name: "node", ServerType: "cx33", Location: "nbg1", ImageID: 42, NetworkIDs: []int64{3}, FirewallIDs: []int64{4},
	})

	require.NoError(t, err)
	body := bodies["POST /servers"]
	assert.NotContains(t, body, "user_data", "the node's create request carries user data")
	assert.Equal(t, map[string]any{"enable_ipv4": false, "enable_ipv6": false}, body["public_net"], "the node's public net")
	assert.Equal(t, float64(42), body["image"], "the node boots from the snapshot by id")
	assert.Equal(t, []any{float64(3)}, body["networks"])
}

func TestHetznerAdapter_CreatesThePrivateLoadBalancerWithNoPublicInterfaceAndLabelTargets(t *testing.T) {
	api, bodies := recordHetzner(t, map[string]string{
		"POST /load_balancers": `{"load_balancer": {"id": 9, "name": "api", "public_net": {"enabled": false}, "private_net": [], "services": [], "targets": []},
			"action": ` + finishedAction + `}`,
	})

	_, err := api.CreateLoadBalancer(context.Background(), controlplane.LoadBalancerSpec{
		Name: "api", Type: "lb11", Location: "fsn1", NetworkID: 3,
		Services:       []controlplane.Service{{ListenPort: 6443, DestinationPort: 6443}},
		TargetSelector: nodes,
	})

	require.NoError(t, err)
	body := bodies["POST /load_balancers"]
	assert.Equal(t, false, body["public_interface"], "the private load balancer's public interface")
	assert.Equal(t, float64(3), body["network"])
	assert.Equal(t, []any{map[string]any{
		"type": "label_selector", "label_selector": map[string]any{"selector": nodes}, "use_private_ip": true,
	}}, body["targets"], "the private load balancer's targets")
}

func TestHetznerAdapter_CreatesABareLoadBalancerWithItsPublicInterfaceOn(t *testing.T) {
	api, bodies := recordHetzner(t, map[string]string{
		"POST /load_balancers": `{"load_balancer": {"id": 9, "name": "api", "public_net": {"enabled": true}, "private_net": [], "services": [], "targets": []},
			"action": ` + finishedAction + `}`,
	})

	_, err := api.CreateLoadBalancer(context.Background(), controlplane.LoadBalancerSpec{
		Name: "api", Type: "lb11", Location: "nbg1", PublicInterface: true,
	})

	require.NoError(t, err)
	body := bodies["POST /load_balancers"]
	assert.Equal(t, true, body["public_interface"], "the bare load balancer's public interface")
	for _, field := range []string{"network", "services", "targets"} {
		assert.NotContains(t, body, field, "the bare load balancer's create request")
	}
}

func TestHetznerAdapter_EachLoadBalancerStepSendsItsActionAndWaitsForIt(t *testing.T) {
	const lb = "POST /load_balancers/9/actions/"
	api, bodies := recordHetzner(t, map[string]string{
		lb + "attach_to_network":        `{"action": ` + finishedAction + `}`,
		lb + "disable_public_interface": `{"action": ` + finishedAction + `}`,
		lb + "add_service":              `{"action": ` + finishedAction + `}`,
		lb + "add_target":               `{"action": ` + finishedAction + `}`,
	})
	ctx := context.Background()

	require.NoError(t, api.AttachLoadBalancerToNetwork(ctx, 9, 3, "10.0.0.3"))
	require.NoError(t, api.DisableLoadBalancerPublicInterface(ctx, 9))
	require.NoError(t, api.AddLoadBalancerService(ctx, 9, controlplane.Service{ListenPort: 443, DestinationPort: 30081, ProxyProtocol: true}))
	require.NoError(t, api.AddLoadBalancerTarget(ctx, 9, nodes))

	assert.Equal(t, map[string]any{"network": float64(3), "ip": "10.0.0.3"}, bodies[lb+"attach_to_network"])
	assert.Contains(t, bodies, lb+"disable_public_interface")
	service := bodies[lb+"add_service"]
	assert.Equal(t, "tcp", service["protocol"])
	assert.Equal(t, float64(443), service["listen_port"])
	assert.Equal(t, float64(30081), service["destination_port"])
	assert.Equal(t, true, service["proxyprotocol"])
	assert.Equal(t, map[string]any{"protocol": "tcp", "port": float64(30081), "interval": float64(15), "timeout": float64(10), "retries": float64(3)},
		service["health_check"], "the service's health check")
	assert.Equal(t, map[string]any{
		"type": "label_selector", "label_selector": map[string]any{"selector": nodes}, "use_private_ip": true,
	}, bodies[lb+"add_target"])
}

func TestHetznerAdapter_AnAttachWhoseActionEndsInAnErrorFails(t *testing.T) {
	api, _ := recordHetzner(t, map[string]string{
		"POST /load_balancers/9/actions/attach_to_network": `{"action": {"id": 2, "command": "attach_to_network", "status": "error", "progress": 100,
			"resources": [], "error": {"code": "ip_not_available", "message": "the IP is not available"}}}`,
	})

	err := api.AttachLoadBalancerToNetwork(context.Background(), 9, 3, "10.0.0.3")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ip_not_available")
}

func TestHetznerAdapter_RefusesAServerHetznerCannotPlaceNamingItsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error": {"code": "resource_unavailable", "message": "server type cx33 unavailable in nbg1"}}`))
	}))
	t.Cleanup(srv.Close)
	api := controlplane.NewAPIAt(srv.URL, "token", "mitstack-test")

	_, err := api.CreateServers(context.Background(), controlplane.ServerSpec{Name: "node", ServerType: "cx33", Location: "nbg1", ImageID: 42})

	var refused *controlplane.Refusal
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "server-unavailable", refused.Class)
	assert.Contains(t, refused.Message, "server type cx33 unavailable in nbg1")
}

func TestHetznerAdapter_StartsTheSnapshotWithoutWaitingForItsAction(t *testing.T) {
	api, _ := recordHetzner(t, map[string]string{
		"POST /servers/7/actions/create_image": `{"image": {"id": 8, "status": "creating"},
			"action": {"id": 3, "command": "create_image", "status": "running", "progress": 0, "resources": []}}`,
	})

	snap, err := api.StartSnapshot(context.Background(), 7, controlplane.SnapshotSpec{Description: "image"})

	require.NoError(t, err, "a wait would poll the running action, which this Hetzner does not serve")
	assert.Equal(t, "creating", snap.Status)
}

func TestHetznerAdapter_AServerHasStartedOnceItsStartActionSucceeded(t *testing.T) {
	for command, want := range map[string]bool{"start_server": true, "create_server": false} {
		api, _ := recordHetzner(t, map[string]string{
			"GET /servers/7/actions": `{"actions": [{"id": 4, "command": "` + command + `", "status": "success", "progress": 100, "resources": []}]}`,
		})

		started, err := api.ServerStarted(context.Background(), 7)

		require.NoError(t, err)
		assert.Equal(t, want, started, "a server whose finished action is %s", command)
	}
}

func TestHetznerAdapter_ReadsTheServersCPUAndDiskMetricsAMinuteAPoint(t *testing.T) {
	var path string
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metrics": {"start": "2026-10-22T18:09:28Z", "end": "2026-10-22T18:11:28Z", "step": 60, "time_series": {
			"cpu": {"values": [[1761156600, "4.500000"], [1761156660, "118.709166"]]},
			"disk.0.bandwidth.write": {"values": [[1761156600, "0.000000"], [1761156660, "156655104.000000"]]}}}}`))
	}))
	t.Cleanup(srv.Close)
	api := controlplane.NewAPIAt(srv.URL, "token", "mitstack-test")
	start := time.Date(2026, 10, 22, 18, 9, 28, 0, time.UTC)

	m, err := api.ServerMetrics(context.Background(), 7, start, start.Add(2*time.Minute))

	require.NoError(t, err)
	assert.Equal(t, "/servers/7/metrics", path)
	assert.Equal(t, []string{"cpu", "disk"}, query["type"], "the metric types read")
	assert.Equal(t, "60", query.Get("step"), "the step in seconds")
	assert.Equal(t, "2026-10-22T18:09:28Z", query.Get("start"))
	assert.Equal(t, "2026-10-22T18:11:28Z", query.Get("end"))
	assert.Equal(t, time.Minute, m.Step)
	assert.Equal(t, []controlplane.MetricPoint{
		{At: time.Unix(1761156600, 0).UTC(), Value: 4.5},
		{At: time.Unix(1761156660, 0).UTC(), Value: 118.709166},
	}, m.Series["cpu"])
	assert.Equal(t, []controlplane.MetricPoint{
		{At: time.Unix(1761156600, 0).UTC(), Value: 0},
		{At: time.Unix(1761156660, 0).UTC(), Value: 156655104},
	}, m.Series["disk.0.bandwidth.write"])
}

func TestHetznerAdapter_ReadsWhichServerTypesTheLocationCanOrder(t *testing.T) {
	api, _ := recordHetzner(t, map[string]string{
		"GET /server_types": `{"server_types": [
			{"id": 1, "name": "cx23", "locations": [{"id": 2, "name": "nbg1", "available": false}, {"id": 3, "name": "hel1", "available": true}]},
			{"id": 4, "name": "cpx22", "locations": [{"id": 2, "name": "nbg1", "available": true}]},
			{"id": 5, "name": "cpx52", "locations": [{"id": 3, "name": "hel1", "available": true}]}]}`,
	})

	names, err := api.AvailableServerTypes(context.Background(), "nbg1")

	require.NoError(t, err)
	assert.Equal(t, []string{"cpx22"}, names, "the types nbg1 can order")
}

func TestHetznerAdapter_CouplesTheVSwitchThroughItsSubnetAndExposesTheNetworksRoutesToIt(t *testing.T) {
	api, bodies := recordHetzner(t, map[string]string{
		"GET /networks/3":                     `{"network": {"id": 3, "name": "net", "ip_range": "10.0.0.0/16", "subnets": []}}`,
		"POST /networks/3/actions/add_subnet": `{"action": ` + finishedAction + `}`,
		"PUT /networks/3":                     `{"network": {"id": 3, "name": "net", "ip_range": "10.0.0.0/16", "expose_routes_to_vswitch": true}}`,
	})

	require.NoError(t, api.AddVSwitchSubnet(context.Background(), 3, 87417, "10.0.1.0/24"))

	assert.Equal(t, map[string]any{"type": "vswitch", "network_zone": "eu-central", "ip_range": "10.0.1.0/24", "vswitch_id": float64(87417)},
		bodies["POST /networks/3/actions/add_subnet"], "the vSwitch subnet")
	assert.Equal(t, true, bodies["PUT /networks/3"]["expose_routes_to_vswitch"], "the network exposes its routes to the vSwitch")
}

// A retry after the subnet landed but the route exposure failed must finish the coupling, since Hetzner refuses a
// second subnet for one vSwitch.
func TestHetznerAdapter_AVSwitchAlreadyCoupledOnlyHasTheNetworksRoutesExposedToIt(t *testing.T) {
	api, bodies := recordHetzner(t, map[string]string{
		"GET /networks/3": `{"network": {"id": 3, "name": "net", "ip_range": "10.0.0.0/16", "expose_routes_to_vswitch": false, "subnets": [
			{"type": "vswitch", "ip_range": "10.0.1.0/24", "network_zone": "eu-central", "gateway": "10.0.0.1", "vswitch_id": 87417}]}}`,
		"PUT /networks/3": `{"network": {"id": 3, "name": "net", "ip_range": "10.0.0.0/16", "expose_routes_to_vswitch": true}}`,
	})

	require.NoError(t, api.AddVSwitchSubnet(context.Background(), 3, 87417, "10.0.1.0/24"))

	assert.NotContains(t, bodies, "POST /networks/3/actions/add_subnet", "a second subnet for the coupled vSwitch")
	assert.Equal(t, true, bodies["PUT /networks/3"]["expose_routes_to_vswitch"], "the network exposes its routes to the vSwitch")
}

func TestHetznerAdapter_ReadsANetworksVSwitchSubnetsAndReplacesItsLabels(t *testing.T) {
	api, bodies := recordHetzner(t, map[string]string{
		"GET /networks": `{"networks": [{"id": 3, "name": "net", "ip_range": "10.0.0.0/16", "expose_routes_to_vswitch": true, "subnets": [
			{"type": "cloud", "ip_range": "10.0.0.0/24", "network_zone": "eu-central", "gateway": "10.0.0.1"},
			{"type": "vswitch", "ip_range": "10.0.1.0/24", "network_zone": "eu-central", "gateway": "10.0.0.1", "vswitch_id": 87417}]}]}`,
		"PUT /networks/3": `{"network": {"id": 3, "name": "net", "ip_range": "10.0.0.0/16"}}`,
	})

	networks, err := api.ListNetworks(context.Background(), "a=b")
	require.NoError(t, err)
	require.NoError(t, api.SetNetworkLabels(context.Background(), 3, map[string]string{"a": "b", "c": "d"}))

	require.Len(t, networks, 1)
	assert.Equal(t, map[int64]string{87417: "10.0.1.0/24"}, networks[0].VSwitchSubnets, "the network's vSwitch subnets")
	assert.True(t, networks[0].ExposeRoutesToVSwitch, "the network exposes its routes to the vSwitch")
	assert.Equal(t, map[string]any{"labels": map[string]any{"a": "b", "c": "d"}}, bodies["PUT /networks/3"], "the label update")
}

func TestHetznerAdapter_TargetsAWorkerByItsAddressRemovesALabelTargetAndReadsBoth(t *testing.T) {
	const lb = "POST /load_balancers/9/actions/"
	api, bodies := recordHetzner(t, map[string]string{
		lb + "add_target":    `{"action": ` + finishedAction + `}`,
		lb + "remove_target": `{"action": ` + finishedAction + `}`,
		"GET /load_balancers": `{"load_balancers": [{"id": 9, "name": "ingress", "public_net": {"enabled": true}, "private_net": [], "services": [],
			"targets": [{"type": "label_selector", "label_selector": {"selector": "` + nodes + `"}}, {"type": "ip", "ip": {"ip": "10.0.1.2"}}]}]}`,
	})
	ctx := context.Background()

	require.NoError(t, api.AddLoadBalancerIPTarget(ctx, 9, "10.0.1.2"))
	assert.Equal(t, map[string]any{"type": "ip", "ip": map[string]any{"ip": "10.0.1.2"}}, bodies[lb+"add_target"], "the address target")
	require.NoError(t, api.RemoveLoadBalancerTarget(ctx, 9, nodes))
	assert.Equal(t, map[string]any{"type": "label_selector", "label_selector": map[string]any{"selector": nodes}}, bodies[lb+"remove_target"],
		"the label target removed")
	lbs, err := api.ListLoadBalancers(ctx, "a=b")
	require.NoError(t, err)
	require.Len(t, lbs, 1)
	assert.Equal(t, []string{nodes}, lbs[0].TargetSelectors, "the label targets read")
	assert.Equal(t, []string{"10.0.1.2"}, lbs[0].TargetIPs, "the address targets read")
}
