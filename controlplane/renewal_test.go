package controlplane_test

// A gateway's serving certificate is renewed by replacing the gateway: once it reaches its renewal point the plane
// replaces one location's gateway at a time, at its address, with a certificate the cluster's gateway CA signs.

import (
	"context"
	"crypto/x509"
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
)

// renewalPoint is when the plane renews the certificate gw serves, read from the certificate itself.
func (h *harness) renewalPoint(gw controlplane.Server) time.Time {
	h.t.Helper()
	return h.gatewayLeaf(gw).NotAfter.Add(-controlplane.GatewayCertRenewBefore)
}

func (h *harness) reachRenewal() {
	h.t.Helper()
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		if at := h.renewalPoint(gw); at.After(h.now) {
			h.now = at
		}
	}
}

func (h *harness) renewed() bool {
	h.t.Helper()
	for _, loc := range controlplane.Locations {
		gateways := controlplane.ServersIn(h.byRole(controlplane.RoleGateway), loc)
		if len(gateways) != 1 || !h.renewalPoint(gateways[0]).After(h.now) {
			return false
		}
		r, err := h.plane.ReadGateway(context.Background(), controlplane.GatewayAddresses[loc], gateways[0].Created)
		require.NoError(h.t, err)
		if !r.Serving {
			return false
		}
	}
	return true
}

func (h *harness) renew(check func()) {
	h.t.Helper()
	h.drive("the renewal", h.renewed, check)
}

func (h *harness) gatewaysByLocation() map[string]controlplane.Server {
	h.t.Helper()
	out := map[string]controlplane.Server{}
	for _, loc := range controlplane.Locations {
		out[loc] = h.gatewayIn(loc)
	}
	return out
}

func (h *harness) dropExpiryLabels() {
	h.t.Helper()
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		labels := maps.Clone(gw.Labels)
		delete(labels, controlplane.KeyGatewayCertExpires)
		require.NoError(h.t, h.cloud.SetServerLabels(context.Background(), gw.ID, labels))
	}
}

func TestPlane_ReplacesEachGatewayPastItsRenewalPointAtItsAddressWithACertificateTheClustersGatewayCASigns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(h *harness)
	}{
		{"each gateway's label names when its certificate lapses", func(*harness) {}},
		{"each gateway was built before the label, so its certificate is judged from its creation", (*harness).dropExpiryLabels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			tc.arrange(h)
			old := h.gatewaysByLocation()
			serials := map[string]string{}
			for loc, gw := range old {
				serials[loc] = h.gatewayLeaf(gw).SerialNumber.Text(16)
			}
			h.reachRenewal()
			reached := h.now

			h.renew(func() {})

			network := h.network()
			roots := x509.NewCertPool()
			require.True(t, roots.AppendCertsFromPEM(h.ca.Cert))
			for _, loc := range controlplane.Locations {
				gw, address := h.gatewayIn(loc), controlplane.GatewayAddresses[loc]
				leaf := h.gatewayLeaf(gw)
				assert.NotEqual(t, old[loc].ID, gw.ID, "the gateway in %s was not replaced", loc)
				assert.Equal(t, address, gw.PrivateIPs[network.ID], "the replacement in %s's private address", loc)
				_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: h.now, DNSName: address})
				assert.NoError(t, err, "the replacement in %s's certificate for %s under the cluster's gateway CA", loc, address)
				assert.NotEqual(t, serials[loc], leaf.SerialNumber.Text(16), "the serial of the replacement in %s's certificate", loc)
				assert.False(t, leaf.NotBefore.Before(reached), "the replacement in %s's certificate is valid from %s, before the renewal point at %s",
					loc, leaf.NotBefore, reached)
				assert.Equal(t, strconv.FormatInt(leaf.NotAfter.Unix(), 10), gw.Labels[controlplane.KeyGatewayCertExpires],
					"the replacement in %s's label naming when its certificate lapses", loc)
			}
			assert.Len(t, h.installed, 1, "installs, each of which mints a gateway CA")
		})
	}
}

func TestPlane_BeginsReplacingAGatewayAtItsCertificatesRenewalPointAndChangesNothingInTheMinuteBefore(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	first := h.gatewayIn(controlplane.GatewayLocation)
	writes := h.cloud.Writes()

	h.now = h.renewalPoint(first).Add(-time.Minute)
	require.NoError(t, h.converge())
	early := h.cloud.Writes()
	h.now = h.renewalPoint(first)
	require.NoError(t, h.converge())

	assert.Equal(t, writes, early, "what the plane changed in the minute before the renewal point")
	began := h.gatewayIn(controlplane.GatewayLocation).Labels[controlplane.KeyRemovalBegan]
	assert.Equal(t, strconv.FormatInt(h.now.Unix(), 10), began, "when the replacement of the gateway in fsn1 began, at its renewal point %s", h.now)
}

func TestPlane_RenewsOneLocationsGatewayAtATimeAndDeletesEachOnlyOnceTheGuardReadsTheOthersServing(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	old := h.gatewaysByLocation()
	lapses := map[string]time.Time{}
	for loc, gw := range old {
		lapses[loc] = h.gatewayLeaf(gw).NotAfter
	}
	nodes := controlplane.IDs(h.byRole(controlplane.RoleNode), controlplane.ServerID)
	h.reachRenewal()
	from := len(h.events)

	h.renew(func() { h.requireOneLocationWithoutItsGatewayAtMost(nodes) })

	deleted := h.deletedGateways(from)
	require.Len(t, deleted, len(controlplane.Locations), "gateways deleted")
	for i, loc := range controlplane.Locations {
		reason := fmt.Sprintf("deleted gateway %d in %s, whose certificate lapses at %s", old[loc].ID, loc, lapses[loc].UTC().Format(time.RFC3339))
		assert.True(t, strings.HasPrefix(deleted[i], reason), "deletion %d is %q, want it to open with %q", i, deleted[i], reason)
		since := stampsAfter(t, deleted[i], "judged by readings since ")
		require.Len(t, since, 1, "the deletion %q names when its replacement began", deleted[i])
		read := stampsAfter(t, deleted[i], " read serving at ")
		require.Len(t, read, len(controlplane.Locations)-1, "the deletion %q names each other gateway's reading", deleted[i])
		for _, at := range read {
			assert.False(t, at.Before(since[0]), "the deletion %q rests on a reading at %s, before its replacement began at %s", deleted[i], at, since[0])
		}
	}
}

func TestPlane_KeepsAGatewayPastItsRenewalPointWhileASecondGatewayReadsNotServingAndRenewsItOnceTheGuardReadsTheOthersServing(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	first, second := h.gatewayIn(controlplane.GatewayLocation), h.gatewayIn("nbg1")
	h.reachRenewal()
	h.failing = []string{controlplane.GatewayAddresses["hel1"]}
	from := len(h.events)

	for range 45 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
	}
	keptWhileDown := h.gatewayIDs()
	held := slices.DeleteFunc(slices.Clone(h.events[from:]), func(e string) bool {
		return !strings.HasPrefix(e, fmt.Sprintf("the renewal keeps gateway %d in fsn1 until the others serve: ", first.ID))
	})
	down := h.gatewayIn("hel1")
	h.failing = nil
	h.renew(func() {})

	assert.Contains(t, keptWhileDown, first.ID, "the gateway in fsn1 while the gateway in hel1 read not serving")
	assert.Contains(t, keptWhileDown, second.ID, "the gateway in nbg1 while the gateway in hel1 read not serving")
	require.NotEmpty(t, held, "the guard's refusals to remove the gateway in fsn1, among %q", h.events[from:])
	last := held[len(held)-1]
	assert.Contains(t, last, "refused:redundancy-below-minimum", "the refusal %q", last)
	assert.Contains(t, last, fmt.Sprintf("%q read not serving at", fmt.Sprintf("gateway %d in hel1", down.ID)),
		"the refusal names the second gateway that does not serve")
	assert.NotContains(t, h.gatewayIDs(), first.ID, "the gateway in fsn1 once the guard read the others serving")
	assert.NotContains(t, h.gatewayIDs(), second.ID, "the gateway in nbg1 once the guard read the others serving")
}

func TestPlane_AnUpgradePastTheRenewalPointReplacesEachGatewayOnceAndTheRenewalReplacesNoneAfterIt(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	h.relabelGateways("an-earlier-list")
	h.reachRenewal()
	from := len(h.events)

	h.upgrade(func() {})
	replacements := h.gatewayIDs()
	for range 30 {
		require.NoError(t, h.converge())
		h.now = h.now.Add(time.Minute)
	}

	deleted := h.deletedGateways(from)
	require.Len(t, deleted, len(controlplane.Locations), "gateways deleted by the upgrade and the half hour after it")
	for _, e := range deleted {
		assert.Contains(t, e, ", built from an earlier list, ", "the deletion %q is the upgrade's", e)
	}
	assert.Equal(t, replacements, h.gatewayIDs(), "the gateways half an hour after the upgrade replaced them")
	for _, gw := range h.byRole(controlplane.RoleGateway) {
		assert.True(t, h.renewalPoint(gw).After(h.now), "gateway %d in %s reaches its renewal point at %s, by %s", gw.ID, gw.Location, h.renewalPoint(gw), h.now)
		assert.Equal(t, strconv.FormatInt(h.gatewayLeaf(gw).NotAfter.Unix(), 10), gw.Labels[controlplane.KeyGatewayCertExpires],
			"the label of the upgrade's replacement in %s naming when its certificate lapses", gw.Location)
	}
}

func TestPlane_KeepsAGatewayPastItsRenewalPointWhileItCouldBuildNoReplacementAndNamesWhy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(h *harness)
		why     string
	}{
		{"the block holds no gateway CA", func(h *harness) { h.lostCA, h.plane.GatewayCA = true, nil },
			"the block holds no gateway CA to sign a new one's certificate with"},
		{"no node report lists the images its replacement keeps", func(h *harness) { h.noImageList = true },
			"the report lists no images"},
		{"Hetzner can order no gateway type in its location", func(h *harness) { h.cloud.SoldOut = controlplane.GatewayServerTypes },
			"Hetzner can order none of the gateway's server types cx23, cpx22 in fsn1 now"},
		{"the block passes no redundancy guard", func(h *harness) { h.plane.Guard = nil },
			"the block passes no redundancy guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withReports)
			h.built()
			first := h.gatewayIn(controlplane.GatewayLocation)
			lapses := h.gatewayLeaf(first).NotAfter.UTC().Format(time.RFC3339)
			h.reachRenewal()
			tc.arrange(h)
			gateways, writes, from := h.gatewayIDs(), h.cloud.Writes(), len(h.events)

			for range 10 {
				require.NoError(t, h.converge())
				h.now = h.now.Add(time.Minute)
			}

			assert.Equal(t, gateways, h.gatewayIDs(), "the gateways")
			assert.Equal(t, writes, h.cloud.Writes(), "what the plane changed")
			kept := fmt.Sprintf("the renewal keeps gateway %d in fsn1, whose certificate lapses at %s, since ", first.ID, lapses)
			require.NotEmpty(t, h.events[from:], "events")
			assert.True(t, strings.HasPrefix(h.events[from], kept), "the first event is %q, want it to open with %q", h.events[from], kept)
			assert.Contains(t, h.events[from], tc.why, "the event names why")
		})
	}
}

func TestPlane_NamesAnExpiryLabelThatNamesNoTimeAsAProblemAndJudgesThatGatewaysCertificateFromItsCreation(t *testing.T) {
	h := newHarness(t, withReports)
	h.built()
	first := h.gatewayIn(controlplane.GatewayLocation)
	renewal := h.renewalPoint(first)
	labels := maps.Clone(first.Labels)
	labels[controlplane.KeyGatewayCertExpires] = "next-year"
	require.NoError(t, h.cloud.SetServerLabels(context.Background(), first.ID, labels))

	problems := h.plane.Problems(context.Background(), h.read())
	h.now = renewal
	require.NoError(t, h.converge())

	assert.Equal(t, []string{fmt.Sprintf("gateway %d's %s label is %q, which names no time in Unix seconds, so its certificate is judged from its creation",
		first.ID, controlplane.KeyGatewayCertExpires, "next-year")}, problems, "problems")
	began := h.gatewayIn(controlplane.GatewayLocation).Labels[controlplane.KeyRemovalBegan]
	assert.Equal(t, strconv.FormatInt(renewal.Unix(), 10), began, "when the replacement of the gateway in fsn1 began, at the renewal point %s its creation gives", renewal)
}
