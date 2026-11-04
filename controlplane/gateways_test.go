package controlplane_test

// Each location runs its own immutable gateway at a fixed address: the network's default route moves off one that stops
// serving, and an upgrade replaces one location's gateway at a time, never a node.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const reportEvery = 5 * time.Minute

// withReports reads each gateway as the node report every five minutes would: serving once it runs at its address,
// unless h.failing names its address.
func withReports(h *harness) {
	h.plane.ReadGateway = func(_ context.Context, address string, since time.Time) (controlplane.GatewayReading, error) {
		received := h.now.Truncate(reportEvery)
		if !received.After(since) {
			return controlplane.GatewayReading{Detail: "no node report was received since " + since.Format(time.RFC3339)}, nil
		}
		detail := "the node report received at " + received.Format(time.RFC3339)
		if slices.Contains(h.failing, address) {
			return controlplane.GatewayReading{Read: true, Detail: detail + " reads its mirror not answering"}, nil
		}
		for _, gw := range h.byRole(controlplane.RoleGateway) {
			command, stoppedAt, err := h.cloud.LastPowerAction(context.Background(), gw.ID)
			require.NoError(h.t, err)
			ranAtReport := gw.Status == controlplane.ServerRunning || command == controlplane.StopServerCommand && stoppedAt.After(received)
			if ranAtReport && slices.Contains(slices.Collect(maps.Values(gw.PrivateIPs)), address) && !gw.Created.After(received) {
				return controlplane.GatewayReading{Read: true, Serving: true, Detail: detail}, nil
			}
		}
		return controlplane.GatewayReading{Read: true, Detail: detail + " reads no answer from " + address}, nil
	}
}

func (h *harness) gatewayIn(loc string) controlplane.Server {
	h.t.Helper()
	gateways := controlplane.ServersIn(h.byRole(controlplane.RoleGateway), loc)
	require.Len(h.t, gateways, 1, "gateways in %s", loc)
	return gateways[0]
}

func (h *harness) relabelGateways(list string) {
	h.t.Helper()
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		labels := maps.Clone(gw.Labels)
		labels[controlplane.KeyGatewayList] = list
		if list == "" {
			delete(labels, controlplane.KeyGatewayList)
		}
		require.NoError(h.t, h.cloud.SetServerLabels(context.Background(), gw.ID, labels))
	}
}

func (h *harness) gatewayLeaf(gw controlplane.Server) *x509.Certificate {
	h.t.Helper()
	part := gatewayParts(h.t, h.cloud.UserData[gw.ID])[0]
	block, _ := pem.Decode([]byte(decodeCloudConfig(h.t, part).file(h.t, "/etc/mitstack-gateway/tls.crt").Content))
	require.NotNil(h.t, block, "the certificate of gateway %d", gw.ID)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(h.t, err)
	return leaf
}

func TestPlane_BuildsAGatewayInEachLocationAtItsOwnAddressUnderOneCA(t *testing.T) {
	h := newHarness(t)

	h.built()

	network := h.network()
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(h.ca.Cert))
	for _, loc := range controlplane.Locations {
		gw := h.gatewayIn(loc)
		address := controlplane.GatewayAddresses[loc]
		assert.Equal(t, address, gw.PrivateIPs[network.ID], "the gateway in %s's address", loc)
		assert.NotEmpty(t, gw.PublicIPv4, "the gateway in %s's public address", loc)
		assert.NotEmpty(t, gw.Labels[controlplane.KeyGatewayList], "the gateway in %s's list label", loc)
		bootstraps := loc == controlplane.GatewayLocation
		assert.Len(t, gatewayParts(t, h.cloud.UserData[gw.ID]), map[bool]int{true: 2, false: 1}[bootstraps],
			"the parts of the gateway in %s, of which only the first location's bootstraps etcd", loc)
		_, err := h.gatewayLeaf(gw).Verify(x509.VerifyOptions{Roots: roots, CurrentTime: h.now, DNSName: address})
		assert.NoError(t, err, "the gateway in %s's certificate for %s under the cluster's CA", loc, address)
	}
	assert.Len(t, h.byRole(controlplane.RoleGateway), len(controlplane.Locations), "gateways")
	require.Len(t, h.installed, 1, "installs")
	assert.Equal(t, controlplane.GatewayAddrs(), h.installed[0].Gateways, "the gateways every node pulls through")
}

func TestPlane_RecordsEachGatewayCertificatesSerialAndValidity(t *testing.T) {
	h := newHarness(t)

	h.built()

	for _, loc := range controlplane.Locations {
		leaf := h.gatewayLeaf(h.gatewayIn(loc))
		assert.Contains(t, h.events, fmt.Sprintf("issued the certificate of the gateway in %s at %s: serial %s, valid from %s until %s",
			loc, controlplane.GatewayAddresses[loc], leaf.SerialNumber.Text(16), leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339)))
	}
}

func TestPlane_NoUserDataOrManifestHoldsTheGatewayCAKey(t *testing.T) {
	h := newHarness(t)

	h.built()

	block, _ := pem.Decode(h.ca.Key)
	require.NotNil(t, block)
	body := strings.Split(strings.TrimSpace(string(pem.EncodeToMemory(block))), "\n")
	needles := body[1 : len(body)-1]
	var haystacks []string
	for _, spec := range h.cloud.ServerSpecs {
		haystacks = append(haystacks, decodedUserData(t, spec.UserData)...)
	}
	for _, c := range h.installed {
		for _, m := range slices.Concat(c.Components.Served, c.Components.Inline) {
			haystacks = append(haystacks, string(m.Bytes))
		}
	}
	require.NotEmpty(t, haystacks)
	for _, hay := range haystacks {
		for _, needle := range needles {
			require.NotContains(t, hay, needle, "rendered user data or a manifest holds a line of the gateway CA's key")
		}
	}
}

// decodedUserData is the user data whole, each MIME part, and each file cloud-init writes from it, decoded.
func decodedUserData(t *testing.T, data string) []string {
	t.Helper()
	if data == "" {
		return nil
	}
	parts := []string{data}
	if strings.HasPrefix(data, "Content-Type: multipart/mixed") {
		parts = gatewayParts(t, data)
	}
	out := []string{data}
	for _, part := range parts {
		config := decodeCloudConfig(t, part)
		out = append(out, part)
		for _, f := range config.WriteFiles {
			out = append(out, config.file(t, f.Path).Content)
		}
	}
	return out
}

func TestPlane_MovesTheDefaultRouteOffAGatewayThatStoppedServing(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(h *harness, gw controlplane.Server)
	}{
		{"powered off", func(h *harness, gw controlplane.Server) { h.cloud.StopServer(gw.ID) }},
		{"read failing by a node report", func(h *harness, _ controlplane.Server) {
			h.failing = []string{controlplane.GatewayAddress}
			h.now = h.now.Add(reportEvery)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			tc.stop(h, h.gatewayIn(controlplane.GatewayLocation))

			require.NoError(t, h.ensure())

			assert.Equal(t, []controlplane.Route{{Destination: "0.0.0.0/0", Gateway: controlplane.GatewayAddresses["nbg1"]}}, h.network().Routes, "the network's routes")
			assert.Contains(t, h.events, fmt.Sprintf("routed the network's default route via gateway %d in nbg1 at %s, since the gateway at %s does not serve",
				h.gatewayIn("nbg1").ID, controlplane.GatewayAddresses["nbg1"], controlplane.GatewayAddress))
		})
	}
}

func TestPlane_AddsTheDefaultRouteOnTheNextCallWhenItsAddFailsAfterItsDelete(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.cloud.StopServer(h.gatewayIn(controlplane.GatewayLocation).ID)
	h.cloud.FailRoute["add"] = errors.New("server error (server_error)")

	err := h.ensure()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the default route via 10.0.0.2 is deleted and the network has none until the next call adds it")
	assert.Empty(t, h.network().Routes, "the network's routes after the failed add")
	assert.Contains(t, h.plane.Problems(context.Background(), h.read()), fmt.Sprintf("network %d has no default route", h.network().ID))
	require.NoError(t, h.ensure())
	assert.Equal(t, []controlplane.Route{{Destination: "0.0.0.0/0", Gateway: controlplane.GatewayAddresses["nbg1"]}}, h.network().Routes, "the network's routes on the next call")
}

func TestPlane_PowersAStoppedGatewayBackOnOnceAnOutageAndReadsItUnhealthyWhenItStaysDown(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	gw := h.gatewayIn(controlplane.GatewayLocation)
	stopped := h.now

	h.cloud.StopServer(gw.ID)
	require.NoError(t, h.converge())

	assert.Equal(t, controlplane.ServerRunning, h.gatewayIn(controlplane.GatewayLocation).Status, "the stopped gateway")
	assert.Contains(t, h.events, fmt.Sprintf("powered gateway %d in fsn1 back on, which was off since Hetzner ran stop_server on it at %s", gw.ID, stopped.Format(time.RFC3339)))
	h.now = h.now.Add(2 * reportEvery)
	h.cloud.StopServer(gw.ID)
	require.NoError(t, h.converge())
	assert.Equal(t, controlplane.ServerRunning, h.gatewayIn(controlplane.GatewayLocation).Status, "a gateway stopped again after a report read it serving")
	poweredOn := h.now

	h.cloud.StopServer(gw.ID)
	require.NoError(t, h.converge())

	assert.Equal(t, controlplane.ServerOff, h.gatewayIn(controlplane.GatewayLocation).Status, "a gateway stopped again before any report read it serving")
	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Equal(t, controlplane.CauseGatewayDown, cause)
	assert.Contains(t, detail, fmt.Sprintf("gateway %d in fsn1 was powered back on at %s and is off", gw.ID, poweredOn.Format(time.RFC3339)))
	assert.Empty(t, h.plane.Problems(context.Background(), h.read()), "problems, with the route on another gateway")
}

// upgrade marks the replacement on an upgrade's call, then re-drives the plane until it has no problem, calling check
// after each call.
func (h *harness) upgrade(check func()) {
	h.t.Helper()
	h.plane.Upgrading = true
	require.NoError(h.t, h.ensure())
	h.plane.Upgrading = false
	for range 400 {
		before := h.cloud.Writes()
		require.NoError(h.t, h.ensure())
		check()
		if h.cloud.Writes() > before {
			continue
		}
		if len(h.plane.Problems(context.Background(), h.read())) == 0 {
			return
		}
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}
	h.t.Fatal("the upgrade did not finish within 400 calls")
}

func TestPlane_UpgradeReplacesOneLocationsGatewayAtATimeAtItsAddressAndNoNode(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	current := h.gatewayIn(controlplane.GatewayLocation).Labels[controlplane.KeyGatewayList]
	nodes := controlplane.IDs(h.byRole(controlplane.RoleNode), controlplane.ServerID)
	old := controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID)
	h.relabelGateways("an-earlier-list")
	installEvents := len(h.events)

	h.upgrade(func() {
		network := h.network()
		var down []string
		for _, loc := range controlplane.Locations {
			gateways := controlplane.ServersIn(h.byRole(controlplane.RoleGateway), loc)
			if len(gateways) != 1 || gateways[0].Status != controlplane.ServerRunning || gateways[0].PrivateIPs[network.ID] != controlplane.GatewayAddresses[loc] {
				down = append(down, loc)
			}
		}
		require.LessOrEqual(t, len(down), 1, "locations without their running gateway at once: %v", down)
		require.Len(t, network.Routes, 1, "the network's routes")
		require.NotContains(t, down, locationOf(network.Routes[0].Gateway), "the route goes to a location without its gateway")
		require.Equal(t, nodes, controlplane.IDs(h.byRole(controlplane.RoleNode), controlplane.ServerID), "the nodes")
	})

	var deleted []string
	for _, e := range h.events[installEvents:] {
		if strings.HasPrefix(e, "deleted gateway") {
			deleted = append(deleted, e)
		}
	}
	require.Len(t, deleted, len(controlplane.Locations), "gateways deleted")
	for i, loc := range controlplane.Locations {
		assert.Contains(t, deleted[i], fmt.Sprintf("deleted gateway %d in %s, built from an earlier list", old[i], loc), "the replacement order")
		gw := h.gatewayIn(loc)
		assert.NotContains(t, old, gw.ID, "the gateway in %s was not replaced", loc)
		assert.Equal(t, current, gw.Labels[controlplane.KeyGatewayList], "the replacement in %s's list", loc)
	}
	issued := slices.DeleteFunc(slices.Clone(h.events[installEvents:]), func(e string) bool { return !strings.HasPrefix(e, "issued the certificate") })
	assert.Len(t, issued, len(controlplane.Locations), "certificate events of the replacements")
	assert.NotContains(t, h.network().Labels, controlplane.KeyGatewayUpgrade, "the network's labels once the upgrade is done")
}

func locationOf(address string) string {
	for loc, a := range controlplane.GatewayAddresses {
		if a == address {
			return loc
		}
	}
	return ""
}

func TestPlane_UpgradeWaitsOnAReplacementNoReportReadsServingAndNeverReplacesTheNext(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	nbg1, hel1 := h.gatewayIn("nbg1").ID, h.gatewayIn("hel1").ID
	h.relabelGateways("an-earlier-list")
	h.failing = []string{controlplane.GatewayAddress}

	h.plane.Upgrading = true
	require.NoError(t, h.ensure())
	h.plane.Upgrading = false
	for range 105 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}

	assert.Equal(t, nbg1, h.gatewayIn("nbg1").ID, "the gateway in nbg1")
	assert.Equal(t, hel1, h.gatewayIn("hel1").ID, "the gateway in hel1")
	replacement := h.gatewayIn(controlplane.GatewayLocation)
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "), fmt.Sprintf(
		"gateway %d in fsn1, built from this release's list, has not been read serving since its creation at %s, so the upgrade waits on it: the node report received at %s reads its mirror not answering",
		replacement.ID, replacement.Created.Format(time.RFC3339), h.now.Truncate(reportEvery).Format(time.RFC3339)))
}

func TestPlane_APinBumpReplacesNoGatewayWithoutAnUpgrade(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	gateways := controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID)
	h.relabelGateways("an-earlier-list")

	for range 20 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
	}

	assert.Equal(t, gateways, controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID), "the gateways")
	assert.Empty(t, h.plane.Problems(context.Background(), h.read()), "problems")
}

func TestPlane_AnUpgradeWithNoGatewayToReplaceChangesNothingThoughNoReportNamesAGateway(t *testing.T) {
	h := newHarness(t)
	h.built()
	h.plane.ReadGateway = func(context.Context, string, time.Time) (controlplane.GatewayReading, error) {
		return controlplane.GatewayReading{Detail: "the newest node report names no gateway"}, nil
	}
	h.plane.Upgrading = true
	before := h.cloud.Writes()

	require.NoError(t, h.converge())

	assert.Equal(t, before, h.cloud.Writes(), "what the upgrade changed")
	assert.Empty(t, h.plane.Problems(context.Background(), h.read()), "problems")
}

func TestPlane_RefusesToRebuildALostGatewayWithoutTheGatewayCA(t *testing.T) {
	h := newHarness(t)
	h.built()
	require.NoError(t, h.cloud.DeleteServer(context.Background(), h.gatewayIn("hel1").ID))
	h.lostCA, h.plane.GatewayCA = true, nil
	writes := h.cloud.Writes()

	assert.Equal(t, "gateway-ca-unknown", refusalClass(t, h.ensure()))
	assert.Equal(t, writes, h.cloud.Writes(), "writes on a refused call")
}

// singleGateway leaves the cluster as a release before per-zone gateways built it: one gateway, in fsn1, with no list
// label, and no CA the block keeps.
func singleGateway(h *harness) {
	h.built()
	for _, loc := range controlplane.Locations[1:] {
		require.NoError(h.t, h.cloud.DeleteServer(context.Background(), h.gatewayIn(loc).ID))
	}
	h.relabelGateways("")
	h.lostCA, h.plane.GatewayCA = true, nil
}

func TestPlane_KeepsAClusterBuiltWithOneGatewayAndReadsItUnhealthyWithItsRemedy(t *testing.T) {
	h := newHarness(t, withReports)
	singleGateway(h)
	writes := h.cloud.Writes()

	require.NoError(t, h.converge())

	assert.Equal(t, writes, h.cloud.Writes(), "writes to a cluster built with one gateway")
	assert.Empty(t, h.plane.Problems(context.Background(), h.read()), "problems")
	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Equal(t, controlplane.CauseSingleGateway, cause)
	assert.True(t, strings.HasSuffix(cause, "; rebuild the cluster"), "the cause names its remedy: %s", cause)
	assert.Contains(t, detail, "carries no "+controlplane.KeyGatewayList+" label")
}

func TestPlane_RefusesTheUpgradeOfAClusterBuiltWithOneGateway(t *testing.T) {
	h := newHarness(t, withReports)
	singleGateway(h)
	writes := h.cloud.Writes()
	h.plane.Upgrading = true

	assert.Equal(t, "gateway-predates-zones", refusalClass(t, h.ensure()))
	assert.Equal(t, writes, h.cloud.Writes(), "writes on a refused upgrade")
}

func TestBuilder_TheReportReadsEachGatewaysMirrorUnderTheGatewayCAAndATLSConnectionToLetsEncrypt(t *testing.T) {
	h := installedOnce(t)

	manifests := inlineManifests(t, builderConfiguration(t, h))

	var env map[string]string
	for _, job := range testutil.Kind(manifests["mitstack-node-report"], "CronJob") {
		if testutil.Field(t, job, "metadata", "name") == "mitstack-node-report" {
			env = testutil.ContainerEnv(t, testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec", "containers", "0"))
		}
	}
	var gateways []string
	for _, loc := range controlplane.Locations {
		gateways = append(gateways, controlplane.GatewayAddresses[loc]+"=https://"+controlplane.GatewayAddresses[loc])
	}
	assert.Equal(t, strings.Join(gateways, " "), env["GATEWAYS"], "each gateway's mirror the report reads")
	assert.Equal(t, string(h.installed[0].GatewayCA), env["GATEWAY_CA"], "the CA the report trusts for the mirrors")
	assert.NotContains(t, env["GATEWAY_CA"], "PRIVATE KEY", "the report carries the CA's certificate alone")
	assert.Equal(t, "https://acme-v02.api.letsencrypt.org/directory", env["EGRESS_URL"], "the TLS connection the report opens")
}
