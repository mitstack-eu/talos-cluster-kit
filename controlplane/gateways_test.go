package controlplane_test

// Each location runs its own immutable gateway at a fixed address: the network's default route moves off one that stops
// serving, and an upgrade replaces one location's gateway at a time, never a node.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/report"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const reportEvery = 5 * time.Minute

// reportReceived is when the newest node report arrived: every five minutes, until h.reportsStopAt.
func (h *harness) reportReceived() time.Time {
	if !h.reportsStopAt.IsZero() && h.now.After(h.reportsStopAt) {
		return h.reportsStopAt.Truncate(reportEvery)
	}
	return h.now.Truncate(reportEvery)
}

// withReports reads each gateway as the node report every five minutes would: serving once it runs at its address,
// unless h.failing names its address, or h.unread names it and the report names no gateway there. Each report lists
// h.images.
func withReports(h *harness) {
	h.plane.ReadReport = func(_ context.Context, since time.Time) (*report.Read, error) {
		received := h.reportReceived()
		if !received.After(since) {
			return nil, nil
		}
		body := map[string]any{"images": append([]report.ImageRef{}, h.images...)}
		if h.noImageList {
			body = map[string]any{"pods": []any{}}
		}
		data, err := json.Marshal(body)
		return &report.Read{Body: data, At: received}, err
	}
	h.plane.ReadGateway = func(_ context.Context, address string, since time.Time) (controlplane.GatewayReading, error) {
		received := h.reportReceived()
		if !received.After(since) {
			return controlplane.GatewayReading{Detail: "no node report was received since " + since.Format(time.RFC3339)}, nil
		}
		detail := "the node report received at " + received.Format(time.RFC3339)
		if slices.Contains(h.unread, address) {
			return controlplane.GatewayReading{Detail: detail + " names no gateway at " + address}, nil
		}
		if slices.Contains(h.failing, address) {
			return controlplane.GatewayReading{Read: true, At: received, Detail: detail + " reads its mirror not answering"}, nil
		}
		for _, gw := range h.byRole(controlplane.RoleGateway) {
			command, stoppedAt, err := h.cloud.LastPowerAction(context.Background(), gw.ID)
			require.NoError(h.t, err)
			ranAtReport := gw.Status == controlplane.ServerRunning || command == controlplane.StopServerCommand && stoppedAt.After(received)
			if ranAtReport && slices.Contains(slices.Collect(maps.Values(gw.PrivateIPs)), address) && !gw.Created.After(received) {
				return controlplane.GatewayReading{Read: true, Serving: true, At: received, Detail: detail}, nil
			}
		}
		return controlplane.GatewayReading{Read: true, At: received, Detail: detail + " reads no answer from " + address}, nil
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

func TestPlane_UpgradeWaitsOnAReplacementNoReportNamesAndNamesTheReportThatMissedIt(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.relabelGateways("an-earlier-list")
	h.unread = []string{controlplane.GatewayAddress}

	h.plane.Upgrading = true
	require.NoError(t, h.ensure())
	h.plane.Upgrading = false
	for range 105 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}

	replacement := h.gatewayIn(controlplane.GatewayLocation)
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "), fmt.Sprintf(
		"gateway %d in fsn1, built from this release's list, has not been read serving since its creation at %s, so the upgrade waits on it: the node report received at %s names no gateway at %s",
		replacement.ID, replacement.Created.Format(time.RFC3339), h.now.Truncate(reportEvery).Format(time.RFC3339), controlplane.GatewayAddress))
}

// deletedGateways is each event since from that records a gateway's deletion.
func (h *harness) deletedGateways(from int) []string {
	return slices.DeleteFunc(slices.Clone(h.events[from:]), func(e string) bool { return !strings.HasPrefix(e, "deleted gateway") })
}

func TestPlane_UpgradeDeletesNoGatewayWhileASecondEarlierListGatewayDoesNotServeAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		down    func(h *harness)
		reading string
	}{
		{"read not serving", func(h *harness) { h.failing = append(h.failing, controlplane.GatewayAddresses["hel1"]) }, "read not serving at"},
		{"with no reading", func(h *harness) { h.unread = []string{controlplane.GatewayAddresses["hel1"]} }, "has no reading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			gateways := controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID)
			h.relabelGateways("an-earlier-list")
			h.failing = []string{controlplane.GatewayAddresses["nbg1"]}
			tc.down(h)
			installEvents := len(h.events)

			h.plane.Upgrading = true
			require.NoError(t, h.ensure())
			h.plane.Upgrading = false
			for range 30 {
				require.NoError(t, h.converge())
				h.now = h.now.Add(time.Minute)
			}

			assert.Equal(t, gateways, controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID), "the gateways")
			assert.Empty(t, h.deletedGateways(installEvents), "gateways deleted")
			assert.Contains(t, strings.Join(h.events[installEvents:], "\n"), fmt.Sprintf("%q %s", fmt.Sprintf("gateway %d in hel1", h.gatewayIn("hel1").ID), tc.reading),
				"the refusal names the second gateway that does not serve")
			assert.Contains(t, strings.Join(h.events[installEvents:], "\n"), "refused:redundancy-below-minimum")
		})
	}
}

func TestPlane_UpgradeReplacesTheEarlierListGatewayThatDoesNotServeBeforeAnyThatServes(t *testing.T) {
	for _, tc := range []struct {
		down   string
		unread bool
	}{{"hel1", false}, {"fsn1", false}, {"fsn1", true}} {
		down := tc.down
		t.Run(fmt.Sprintf("the gateway in %s never served, unread %t", down, tc.unread), func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			old := map[string]int64{}
			for _, loc := range controlplane.Locations {
				old[loc] = h.gatewayIn(loc).ID
			}
			h.relabelGateways("an-earlier-list")
			h.failing = []string{controlplane.GatewayAddresses[down]}
			if tc.unread {
				h.failing, h.unread = nil, h.failing
			}
			installEvents := len(h.events)

			h.upgrade(func() {
				if len(h.deletedGateways(installEvents)) > 0 {
					h.failing, h.unread = nil, nil
				}
			})

			deleted := h.deletedGateways(installEvents)
			require.Len(t, deleted, len(controlplane.Locations), "gateways deleted")
			assert.True(t, strings.HasPrefix(deleted[0], fmt.Sprintf("deleted gateway %d in %s, built from an earlier list", old[down], down)),
				"the first deletion: %s", deleted[0])
			for _, loc := range controlplane.Locations {
				if loc != down {
					assert.Contains(t, deleted[0], fmt.Sprintf("%q read serving at", fmt.Sprintf("gateway %d in %s", old[loc], loc)),
						"the first deletion names the gateway in %s it read serving", loc)
				}
			}
		})
	}
}

func TestPlane_UpgradeDeletesAGatewayOnlyOnReadingsReceivedAfterItsReplacementBegan(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.relabelGateways("an-earlier-list")
	installEvents := len(h.events)

	h.upgrade(func() {})

	deleted := h.deletedGateways(installEvents)
	require.Len(t, deleted, len(controlplane.Locations), "gateways deleted")
	for _, e := range deleted {
		since := stampsAfter(t, e, "judged by readings since ")
		require.Len(t, since, 1, "the deletion %q names when its replacement began", e)
		read := stampsAfter(t, e, " read serving at ")
		require.Len(t, read, len(controlplane.Locations)-1, "the deletion %q names each other gateway's reading", e)
		for _, at := range read {
			assert.False(t, at.Before(since[0]), "the deletion %q rests on a reading at %s, before its replacement began at %s", e, at, since[0])
		}
		received := stampsAfter(t, e, "the node report received at ")
		require.Len(t, received, len(controlplane.Locations)-1, "the deletion %q names the node report each reading came from", e)
		for _, at := range received {
			assert.True(t, at.After(since[0]), "the deletion %q rests on a report received at %s, by its replacement's start at %s", e, at, since[0])
		}
	}
}

func TestPlane_UpgradeBeginsAGatewaysReplacementAgainWhenAnEarlierOneBeganForAnotherList(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.relabelGateways("an-earlier-list")
	abandoned := h.now.Add(-6 * time.Hour)
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		require.NoError(t, h.cloud.SetServerLabels(context.Background(), gw.ID, controlplane.BeginRemoval(gw, abandoned, "an-abandoned-list")))
	}
	started, installEvents := h.now, len(h.events)

	h.upgrade(func() {})

	deleted := h.deletedGateways(installEvents)
	require.Len(t, deleted, len(controlplane.Locations), "gateways deleted")
	for _, e := range deleted {
		since := stampsAfter(t, e, "judged by readings since ")
		require.Len(t, since, 1, "the deletion %q names when its replacement began", e)
		assert.False(t, since[0].Before(started), "the deletion %q is judged since %s, which an abandoned replacement for another list began", e, since[0])
	}
}

// stampsAfter is each time an event names right after marker.
func stampsAfter(t *testing.T, event, marker string) []time.Time {
	t.Helper()
	var out []time.Time
	for _, part := range strings.Split(event, marker)[1:] {
		at, err := time.Parse(time.RFC3339, part[:len("2006-01-02T15:04:05Z")])
		require.NoError(t, err, "the time after %q in %q", marker, event)
		out = append(out, at)
	}
	return out
}

func TestPlane_MovesTheDefaultRouteOnlyToAGatewayAReportReadsServing(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.failing = []string{controlplane.GatewayAddress}
	h.unread = []string{controlplane.GatewayAddresses["nbg1"]}
	h.now = h.now.Add(reportEvery)

	require.NoError(t, h.ensure())

	assert.Equal(t, []controlplane.Route{{Destination: "0.0.0.0/0", Gateway: controlplane.GatewayAddresses["hel1"]}}, h.network().Routes,
		"the network's routes, with the gateway in nbg1 unread")
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

func TestBuilder_TheOutsideProbeReadsEachGatewaysMirrorUnderTheGatewayCAAndTheReportATLSConnectionToLetsEncrypt(t *testing.T) {
	h := installedOnce(t)

	manifests := inlineManifests(t, builderConfiguration(t, h))

	env := map[string]map[string]string{}
	for _, job := range testutil.Kind(manifests["mitstack-node-report"], "CronJob") {
		env[testutil.Field(t, job, "metadata", "name").(string)] = testutil.ContainerEnv(t,
			testutil.Field(t, job, "spec", "jobTemplate", "spec", "template", "spec", "containers", "0"))
	}
	var gateways []string
	for _, loc := range controlplane.Locations {
		gateways = append(gateways, controlplane.GatewayAddresses[loc])
	}
	probe, report := env["mitstack-mesh-probe"], env["mitstack-node-report"]
	assert.Equal(t, strings.Join(gateways, " "), probe["GATEWAYS"], "each gateway's mirror the outside probe reads")
	assert.Equal(t, strings.Join(gateways, " "), report["GATEWAYS"], "each gateway the report names a reading of")
	assert.Equal(t, string(h.installed[0].GatewayCA), probe["GATEWAY_CA"], "the CA the outside probe trusts for the mirrors")
	assert.NotContains(t, probe["GATEWAY_CA"], "PRIVATE KEY", "the outside probe carries the CA's certificate alone")
	assert.Equal(t, "https://acme-v02.api.letsencrypt.org/directory", report["EGRESS_URL"], "the TLS connection the report opens")
}

// upgradeToFailingReplacement starts an upgrade whose first replacement no report reads serving, and drives the plane
// until that replacement runs at its address, past the minutes fsn1 waits on a node report without a gateway.
func upgradeToFailingReplacement(h *harness) controlplane.Server {
	h.t.Helper()
	h.built()
	h.relabelGateways("an-earlier-list")
	h.failing = []string{controlplane.GatewayAddress}
	h.plane.Upgrading = true
	require.NoError(h.t, h.ensure())
	h.plane.Upgrading = false
	for range 30 {
		require.NoError(h.t, h.converge())
		for _, gw := range controlplane.ServersIn(h.byRole(controlplane.RoleGateway), controlplane.GatewayLocation) {
			if gw.Labels[controlplane.KeyGatewayList] != "an-earlier-list" && gw.Status == controlplane.ServerRunning {
				return gw
			}
		}
		h.now = h.now.Add(time.Minute)
		h.cloud.Advance(h.buildTakes, h.imageTakes)
	}
	h.t.Fatal("the replacement in fsn1 never ran")
	return controlplane.Server{}
}

func TestPlane_NamesAReplacementRunningPastItsDeadlineUnhealthyAndKeepsWaitingWithoutDeletingAGateway(t *testing.T) {
	h := newHarness(t, withReports)
	replacement := upgradeToFailingReplacement(h)
	others := []int64{h.gatewayIn("nbg1").ID, h.gatewayIn("hel1").ID}
	deadline := replacement.Created.Add(gateway.Deadline)
	h.now = deadline.Add(time.Minute)

	require.NoError(t, h.converge())

	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	missed := fmt.Sprintf("gateway %d in fsn1 has neither served nor powered itself off by its deadline at %s", replacement.ID,
		deadline.UTC().Format(time.RFC3339))
	assert.Equal(t, controlplane.CauseGatewayOverdue, cause)
	assert.Contains(t, detail, missed)
	assert.Contains(t, strings.Join(h.plane.Problems(context.Background(), h.read()), "; "), missed, "the step still waits and names the deadline")
	assert.Equal(t, others, []int64{h.gatewayIn("nbg1").ID, h.gatewayIn("hel1").ID}, "the gateways the upgrade has yet to replace")
	assert.Equal(t, replacement.ID, h.gatewayIn(controlplane.GatewayLocation).ID, "the replacement past its deadline")

	h.failing = nil
	for range 2 {
		h.now = h.now.Add(reportEvery)
		require.NoError(t, h.converge())
	}

	assert.NotContains(t, controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID), others[0],
		"a replacement read serving after its deadline lets the upgrade move on, once a report from after nbg1's replacement began reads it")
	cause, _, err = h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Empty(t, cause)
}

func TestPlane_NamesNoDeadlineForAReplacementThatHasNotReachedIt(t *testing.T) {
	h := newHarness(t, withReports)
	replacement := upgradeToFailingReplacement(h)
	h.now = replacement.Created.Add(gateway.Deadline - time.Minute)

	require.NoError(t, h.converge())

	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Empty(t, cause, detail)
	problems := strings.Join(h.plane.Problems(context.Background(), h.read()), "; ")
	assert.Contains(t, problems, fmt.Sprintf("gateway %d in fsn1, built from this release's list, has not been read serving", replacement.ID))
	assert.NotContains(t, problems, "deadline")
}

// gatewayFailures are the two ways a gateway built from this release's list fails, each with the cause it reads.
var gatewayFailures = []struct {
	name, cause string
	fail        func(h *harness, gw controlplane.Server)
}{
	{"running past its deadline without serving", controlplane.CauseGatewayOverdue, func(h *harness, gw controlplane.Server) {
		h.failing = append(h.failing, controlplane.GatewayAddresses[gw.Location])
		h.now = gw.Created.Add(gateway.Deadline)
	}},
	{"down after its one power-on", controlplane.CauseGatewayDown, func(h *harness, gw controlplane.Server) {
		h.cloud.StopServer(gw.ID)
		require.NoError(h.t, h.converge())
		h.cloud.StopServer(gw.ID)
	}},
}

func (h *harness) gatewayIDs() []int64 {
	return controlplane.IDs(h.byRole(controlplane.RoleGateway), controlplane.ServerID)
}

// runsInPlaceOf reports whether a gateway other than failed runs at failed's address.
func (h *harness) runsInPlaceOf(failed controlplane.Server) bool {
	gateways := controlplane.ServersIn(h.byRole(controlplane.RoleGateway), failed.Location)
	return len(gateways) == 1 && gateways[0].ID != failed.ID && gateways[0].Status == controlplane.ServerRunning &&
		gateways[0].PrivateIPs[h.network().ID] == controlplane.GatewayAddresses[failed.Location]
}

// servesInPlaceOf reports whether a gateway other than failed runs at its address and a node report reads it serving.
func (h *harness) servesInPlaceOf(failed controlplane.Server) bool {
	h.t.Helper()
	if !h.runsInPlaceOf(failed) {
		return false
	}
	r, err := h.plane.ReadGateway(context.Background(), controlplane.GatewayAddresses[failed.Location], h.gatewayIn(failed.Location).Created)
	require.NoError(h.t, err)
	return r.Serving
}

func TestPlane_ReplacesAFailedGatewayOfThisReleasesListAtItsAddressFromTheSameListOnceAFreshReportReadsTheOthersServing(t *testing.T) {
	for _, tc := range gatewayFailures {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			failed := h.gatewayIn(controlplane.GatewayLocation)
			nbg1, from := h.gatewayIn("nbg1"), len(h.events)

			tc.fail(h, failed)
			var routesOnceGone []controlplane.Route
			require.NoError(t, h.advance(2*time.Hour, func() bool {
				if routesOnceGone == nil && !slices.Contains(h.gatewayIDs(), failed.ID) {
					routesOnceGone, h.failing = h.network().Routes, nil
				}
				return h.servesInPlaceOf(failed)
			}))

			successor := h.gatewayIn(controlplane.GatewayLocation)
			assert.Equal(t, failed.Labels[controlplane.KeyGatewayList], successor.Labels[controlplane.KeyGatewayList], "the successor's list")
			assert.Equal(t, strconv.FormatInt(failed.ID, 10), successor.Labels[controlplane.KeyGatewayReplaces], "the gateway the successor replaced")
			assert.Equal(t, []controlplane.Route{{Destination: "0.0.0.0/0", Gateway: controlplane.GatewayAddresses["nbg1"]}}, routesOnceGone,
				"the network's routes once the failed gateway is gone")
			assert.Contains(t, h.events[from:], fmt.Sprintf("routed the network's default route via gateway %d in nbg1 at %s, since the gateway at %s does not serve",
				nbg1.ID, controlplane.GatewayAddresses["nbg1"], controlplane.GatewayAddress))
			deleted := h.deletedGateways(from)
			require.Len(t, deleted, 1, "gateways deleted")
			assert.True(t, strings.HasPrefix(deleted[0], fmt.Sprintf("deleted gateway %d in fsn1, which ", failed.ID)), "the deletion: %s", deleted[0])
			since := stampsAfter(t, deleted[0], "judged by readings since ")
			require.Len(t, since, 1, "the deletion %q names when its replacement began", deleted[0])
			read := stampsAfter(t, deleted[0], " read serving at ")
			require.Len(t, read, len(controlplane.Locations)-1, "the deletion %q names each other gateway's reading", deleted[0])
			for _, at := range read {
				assert.False(t, at.Before(since[0]), "the deletion %q rests on a reading at %s, before its replacement began at %s", deleted[0], at, since[0])
			}
			assert.NotContains(t, h.network().Labels, controlplane.KeyGatewayFailed+controlplane.GatewayLocation, "the network's labels once the successor stands")
			assert.Empty(t, h.plane.Problems(context.Background(), h.read()), "problems")
		})
	}
}

// requireRouteOnARunningGateway fails unless the network's one route goes to a gateway that runs.
func (h *harness) requireRouteOnARunningGateway() {
	h.t.Helper()
	network := h.network()
	require.Len(h.t, network.Routes, 1, "the network's routes")
	gateways := controlplane.ServersIn(h.byRole(controlplane.RoleGateway), locationOf(network.Routes[0].Gateway))
	require.Len(h.t, gateways, 1, "gateways at %s, where the route goes", network.Routes[0].Gateway)
	require.Equal(h.t, controlplane.ServerRunning, gateways[0].Status, "the gateway the route goes to")
}

func TestPlane_AnUpgradeReplacesItsReplacementPastItsDeadlineWithoutServingFromTheSameListAndMovesOn(t *testing.T) {
	h := newHarness(t, withReports)
	failed := upgradeToFailingReplacement(h)
	earlier := []int64{h.gatewayIn("nbg1").ID, h.gatewayIn("hel1").ID}
	h.now = failed.Created.Add(gateway.Deadline)
	from := len(h.events)

	h.upgrade(func() {
		if !slices.Contains(h.gatewayIDs(), failed.ID) {
			h.failing = nil
		}
		h.requireRouteOnARunningGateway()
	})

	deleted := h.deletedGateways(from)
	require.Len(t, deleted, len(controlplane.Locations), "gateways deleted")
	assert.True(t, strings.HasPrefix(deleted[0], fmt.Sprintf("deleted gateway %d in fsn1, which has neither served nor powered itself off by its deadline", failed.ID)),
		"the first deletion: %s", deleted[0])
	assert.Equal(t, strconv.FormatInt(failed.ID, 10), h.gatewayIn(controlplane.GatewayLocation).Labels[controlplane.KeyGatewayReplaces], "the gateway fsn1's successor replaced")
	for _, loc := range controlplane.Locations {
		assert.Equal(t, failed.Labels[controlplane.KeyGatewayList], h.gatewayIn(loc).Labels[controlplane.KeyGatewayList], "the list of the gateway in %s", loc)
	}
	assert.NotContains(t, h.gatewayIDs(), earlier[0], "the gateway in nbg1 the upgrade moved on to")
	assert.NotContains(t, h.gatewayIDs(), earlier[1], "the gateway in hel1 the upgrade moved on to")
	assert.NotContains(t, h.network().Labels, controlplane.KeyGatewayUpgrade, "the network's labels once the upgrade is done")
}

func TestPlane_ReplacesNoFailedGatewayWhileASecondGatewayDoesNotServeAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		down    func(h *harness)
		reading string
	}{
		{"read not serving", func(h *harness) { h.failing = append(h.failing, controlplane.GatewayAddresses["hel1"]) }, "read not serving at"},
		{"with no reading", func(h *harness) { h.unread = []string{controlplane.GatewayAddresses["hel1"]} }, "has no reading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			gateways, from := h.gatewayIDs(), len(h.events)
			gatewayFailures[0].fail(h, h.gatewayIn(controlplane.GatewayLocation))
			tc.down(h)

			for range 30 {
				require.NoError(t, h.converge())
				h.now = h.now.Add(time.Minute)
			}

			assert.Equal(t, gateways, h.gatewayIDs(), "the gateways")
			assert.Empty(t, h.deletedGateways(from), "gateways deleted")
			events := strings.Join(h.events[from:], "\n")
			assert.Contains(t, events, fmt.Sprintf("%q %s", fmt.Sprintf("gateway %d in hel1", h.gatewayIn("hel1").ID), tc.reading),
				"the refusal names the second gateway that does not serve")
			assert.Contains(t, events, "refused:redundancy-below-minimum")
		})
	}
}

func TestPlane_LeavesASuccessorThatFailsFromTheSameListInPlaceAndReadsUnhealthyNamingBothGateways(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	failed := h.gatewayIn(controlplane.GatewayLocation)
	gatewayFailures[0].fail(h, failed)
	require.NoError(t, h.advance(2*time.Hour, func() bool { return h.runsInPlaceOf(failed) }))
	successor, from := h.gatewayIn(controlplane.GatewayLocation), len(h.events)

	h.now = successor.Created.Add(gateway.Deadline)
	for range 3 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(reportEvery)
	}

	assert.Equal(t, successor.ID, h.gatewayIn(controlplane.GatewayLocation).ID, "the successor that failed from the same list")
	assert.Empty(t, h.deletedGateways(from), "gateways deleted")
	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Equal(t, controlplane.CauseGatewayFailedAgain, cause)
	assert.Contains(t, detail, fmt.Sprintf("gateway %d in fsn1 has neither served nor powered itself off by its deadline", successor.ID))
	assert.Contains(t, detail, fmt.Sprintf("gateway %d", failed.ID), "the detail names the gateway the successor replaced")
}

func TestPlane_KeepsAGatewayAReportReadsServingJustAfterItsDeadlineAndEndsItsReplacement(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	gw := h.gatewayIn(controlplane.GatewayLocation)
	list := gw.Labels[controlplane.KeyGatewayList]
	gatewayFailures[0].fail(h, gw)
	require.NoError(t, h.converge())
	_, began := controlplane.RemovalBegan(h.gatewayIn(controlplane.GatewayLocation), list)
	require.True(t, began, "the replacement begun on the call past the deadline")
	from := len(h.events)

	h.failing = nil
	for range 3 {
		h.now = h.now.Add(reportEvery)
		require.NoError(t, h.converge())
	}

	kept := h.gatewayIn(controlplane.GatewayLocation)
	assert.Equal(t, gw.ID, kept.ID, "the gateway read serving after its deadline")
	assert.Empty(t, h.deletedGateways(from), "gateways deleted")
	_, began = controlplane.RemovalBegan(kept, list)
	assert.False(t, began, "a replacement still under way on a gateway that serves")
	cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Empty(t, cause, detail)
}

func TestPlane_DeletesAFailedGatewayOnTheCallAfterHetznerRefusedItsDeleteWithNoSecondRouteMove(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	failed := h.gatewayIn(controlplane.GatewayLocation)
	gatewayFailures[0].fail(h, failed)
	require.NoError(t, h.converge())
	h.now = h.now.Add(reportEvery)
	h.cloud.FailDelete = errors.New("server error (server_error)")
	routed := func() int {
		return len(slices.DeleteFunc(slices.Clone(h.events), func(e string) bool { return !strings.HasPrefix(e, "routed the network's default route") }))
	}
	err := h.ensure()
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("deleting the gateway %d in fsn1", failed.ID))
	require.Contains(t, h.gatewayIDs(), failed.ID, "the gateway whose delete Hetzner refused")
	moves := routed()

	require.NoError(t, h.ensure())

	assert.NotContains(t, h.gatewayIDs(), failed.ID, "the failed gateway on the next call")
	assert.Equal(t, moves, routed(), "route moves on the next call")
	assert.Equal(t, []controlplane.Route{{Destination: "0.0.0.0/0", Gateway: controlplane.GatewayAddresses["nbg1"]}}, h.network().Routes, "the network's routes")
}

func TestPlane_ReadsAFailedGatewayUnhealthyNamingItAndTheReplacementUnderWay(t *testing.T) {
	for _, tc := range gatewayFailures {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			failed := h.gatewayIn(controlplane.GatewayLocation)
			tc.fail(h, failed)
			require.NoError(t, h.converge())

			cause, detail, err := h.plane.Unhealthy(context.Background(), h.read())

			require.NoError(t, err)
			began, ok := controlplane.RemovalBegan(h.gatewayIn(controlplane.GatewayLocation), failed.Labels[controlplane.KeyGatewayList])
			require.True(t, ok, "the replacement under way")
			assert.Equal(t, tc.cause, cause)
			assert.Contains(t, cause, "the block replaces it from the same list", "the cause names the plane's own replacement")
			assert.Contains(t, detail, fmt.Sprintf("gateway %d in fsn1", failed.ID))
			assert.Contains(t, detail, "the block began replacing it at "+began.Format(time.RFC3339))
		})
	}
}

func TestPlane_LeavesAGatewayOfAnEarlierListDownAfterItsPowerOnToAnUpgradeAndNamesThatRemedy(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.relabelGateways("an-earlier-list")
	gw, from := h.gatewayIn(controlplane.GatewayLocation), len(h.events)
	gatewayFailures[1].fail(h, gw)

	for range 30 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
	}

	assert.Equal(t, gw.ID, h.gatewayIn(controlplane.GatewayLocation).ID, "the gateway built from an earlier list")
	assert.Empty(t, h.deletedGateways(from), "gateways deleted")
	cause, _, err := h.plane.Unhealthy(context.Background(), h.read())
	require.NoError(t, err)
	assert.Equal(t, controlplane.CauseEarlierGatewayDown, cause)
	assert.True(t, strings.HasSuffix(cause, "replace it with an upgrade"), "the cause names its remedy: %s", cause)
}
