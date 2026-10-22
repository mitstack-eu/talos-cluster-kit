package nodeconfig_test

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func TestRender_MatchesTheGoldenConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		profile   nodeconfig.Profile
		locations int
		hostnames []string
	}{
		{name: "a Cloud cluster of one control-plane node", profile: nodeconfig.HCloud, locations: 1, hostnames: []string{""}},
		{name: "a Cloud cluster of three control-plane nodes shares one image", profile: nodeconfig.HCloud, locations: 3, hostnames: []string{""}},
		{name: "a dedicated cluster of one control-plane node", profile: nodeconfig.Metal, locations: 1, hostnames: []string{"cp-fsn1"}},
		{name: "a dedicated cluster of three control-plane nodes has one image per node", profile: nodeconfig.Metal, locations: 3, hostnames: []string{"cp-fsn1", "cp-nbg1", "cp-hel1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile), testutil.WithLocations(tc.locations))

			for _, hostname := range tc.hostnames {
				got, err := nodeconfig.Render(cluster, nodeconfig.Node{Hostname: hostname})

				require.NoError(t, err)
				testutil.Golden(t, goldenPath(tc.profile, tc.locations, hostname), got)
			}
		})
	}
}

func goldenPath(profile nodeconfig.Profile, locations int, hostname string) string {
	name := fmt.Sprintf("%s-%d-control-plane", profile, locations)
	if hostname != "" {
		name += "-" + hostname
	}
	return filepath.Join("testdata", "golden", name+".yaml")
}

func TestRender_WithNoLogReceiverLeavesOutTheCollectorAndBothLogSinksAndNothingElse(t *testing.T) {
	noReceiver := testutil.NewCluster(t)
	noReceiver.Logs = components.LogExport{}
	withReceiver, err := nodeconfig.Render(testutil.NewCluster(t), nodeconfig.Node{})
	require.NoError(t, err)

	got, err := nodeconfig.Render(noReceiver, nodeconfig.Node{})

	require.NoError(t, err)
	assert.Equal(t, withoutLogParts(testutil.Documents(t, withReceiver)), testutil.Documents(t, got))
}

// withoutLogParts drops the service log destination, the kernel log sink and the Collector's manifest.
func withoutLogParts(docs []map[string]any) []map[string]any {
	var kept []map[string]any
	for _, doc := range docs {
		if doc["kind"] == "KmsgLogConfig" || doc["name"] == "mitstack-node-logs" {
			continue
		}
		if machine, ok := doc["machine"].(map[string]any); ok {
			delete(machine, "logging")
		}
		kept = append(kept, doc)
	}
	return kept
}

func TestRender_RefusesALogReceiverWithACAButNoEndpoint(t *testing.T) {
	cluster := testutil.NewCluster(t)
	cluster.Logs.Endpoint = ""

	_, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `log export endpoint "" is not host:port`)
}

func TestRender_PullsEveryImageOfTheComponentSetFromEveryGatewaysMirror(t *testing.T) {
	cluster := testutil.NewCluster(t, testutil.WithLocations(3))
	images, err := mirror.List(cluster.Components)
	require.NoError(t, err)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	mirrors := map[string]map[string]any{}
	for _, doc := range testutil.Kind(docs, "RegistryMirrorConfig") {
		mirrors[doc["name"].(string)] = doc
	}
	for _, image := range images {
		doc, ok := mirrors[image.Registry()]
		require.True(t, ok, "image %s comes from %s, which has no RegistryMirrorConfig", image.Ref(), image.Registry())
		assert.Equal(t, true, doc["skipFallback"], "registry %s falls back to the public registry", image.Registry())
		assert.Equal(t, gatewayEndpoints(cluster.Gateways, image.Registry()), doc["endpoints"], "registry %s endpoints", image.Registry())
	}
	for _, kind := range []string{"KubeletConfig", "KubeAPIServerConfig", "KubeControllerManagerConfig", "KubeSchedulerConfig"} {
		ref := testutil.Field(t, testutil.Kind(docs, kind)[0], "image").(string)
		assert.True(t, slices.ContainsFunc(images, func(i components.Image) bool { return i.Name+":"+i.Tag == ref }),
			"%s pulls %s, which the mirror list %v lacks", kind, ref, images)
	}
}

func gatewayEndpoints(gateways []netip.Addr, registry string) []any {
	var endpoints []any
	for _, gw := range gateways {
		endpoints = append(endpoints, map[string]any{"url": "https://" + gw.String() + "/v2/" + registry, "overridePath": true})
	}
	return endpoints
}

func TestRender_UsesHetznersResolversAndTimeServersInPlaceOfTalosDefaults(t *testing.T) {
	cluster := testutil.NewCluster(t)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	docs := testutil.Documents(t, got)
	resolvers := testutil.Kind(docs, "ResolverConfig")
	require.Len(t, resolvers, 1)
	assert.Equal(t, []any{map[string]any{"address": "185.12.64.1"}, map[string]any{"address": "185.12.64.2"}}, resolvers[0]["nameservers"])
	timeSync := testutil.Kind(docs, "TimeSyncConfig")
	require.Len(t, timeSync, 1)
	assert.Equal(t, []any{"ntp1.hetzner.de", "ntp2.hetzner.com", "ntp3.hetzner.net"}, testutil.Field(t, timeSync[0], "ntp", "servers"))
	assert.Empty(t, testutil.Kind(docs, "DiscoveryServiceConfig"), "the configuration still reaches Sidero's discovery service")
}

func TestRender_GivesACloudNodeADefaultRouteThroughHetznersRouterAndKeepsDHCPOnItsLink(t *testing.T) {
	cases := []struct {
		name      string
		profile   nodeconfig.Profile
		hostname  string
		wantLinks []map[string]any
		wantDHCP  []map[string]any
	}{
		{
			name: "a Cloud node, whose only link is on the private network", profile: nodeconfig.HCloud,
			wantLinks: []map[string]any{{"apiVersion": "v1alpha1", "kind": "LinkConfig", "name": "eth0", "routes": []any{map[string]any{"gateway": "10.0.0.1"}}}},
			wantDHCP:  []map[string]any{{"apiVersion": "v1alpha1", "kind": "DHCPv4Config", "name": "eth0"}},
		},
		{name: "a dedicated node, whose addresses its own trial decides", profile: nodeconfig.Metal, hostname: "cp-fsn1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile))

			got, err := nodeconfig.Render(cluster, nodeconfig.Node{Hostname: tc.hostname})

			require.NoError(t, err)
			docs := testutil.Documents(t, got)
			assert.Equal(t, tc.wantLinks, testutil.Kind(docs, "LinkConfig"))
			assert.Equal(t, tc.wantDHCP, testutil.Kind(docs, "DHCPv4Config"))
		})
	}
}

func TestRender_CarriesNoAdminClientCertificate(t *testing.T) {
	cluster := testutil.NewCluster(t)

	got, err := nodeconfig.Render(cluster, nodeconfig.Node{})

	require.NoError(t, err)
	var certs []*x509.Certificate
	collectCertificates(t, got, &certs)
	for _, doc := range testutil.Documents(t, got) {
		collectBase64Certificates(t, doc, &certs)
	}
	require.NotEmpty(t, certs, "the configuration holds no certificate at all")
	for _, cert := range certs {
		assert.True(t, cert.IsCA, "certificate %q in the configuration is not a CA", cert.Subject)
		assert.NotContains(t, cert.Subject.Organization, "os:admin", "certificate %q is an admin client certificate", cert.Subject)
	}
}

// collectCertificates parses every PEM certificate block in data.
func collectCertificates(t *testing.T, data []byte, certs *[]*x509.Certificate) {
	t.Helper()
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		*certs = append(*certs, cert)
	}
}

// collectBase64Certificates finds the base64-encoded PEM certificates the v1alpha1 document keeps under crt fields.
func collectBase64Certificates(t *testing.T, node any, certs *[]*x509.Certificate) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if s, ok := child.(string); ok && key == "crt" {
				data, err := base64.StdEncoding.DecodeString(s)
				require.NoError(t, err, "crt field %q", s)
				collectCertificates(t, data, certs)
				continue
			}
			collectBase64Certificates(t, child, certs)
		}
	case []any:
		for _, child := range v {
			collectBase64Certificates(t, child, certs)
		}
	}
}

func TestRender_RefusesANodeItsProfileCannotName(t *testing.T) {
	cases := []struct {
		name    string
		profile nodeconfig.Profile
		node    nodeconfig.Node
		want    string
	}{
		{name: "a Cloud node with a hostname of its own", profile: nodeconfig.HCloud, node: nodeconfig.Node{Hostname: "cp-fsn1"}, want: `hostname "cp-fsn1" is refused`},
		{name: "a dedicated node with no hostname", profile: nodeconfig.Metal, node: nodeconfig.Node{}, want: "needs its own hostname"},
		{name: "a profile imager has no part in", profile: "aws", node: nodeconfig.Node{}, want: `profile "aws"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := testutil.NewCluster(t, testutil.WithProfile(tc.profile))

			_, err := nodeconfig.Render(cluster, tc.node)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAdminTalosconfig_HoldsTheAdminRoleForItsTTLAlone(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	got, err := testutil.Secrets(t).AdminTalosconfig(netip.MustParseAddr("10.0.0.5"), 10*time.Minute, now)

	require.NoError(t, err)
	var cfg struct {
		Contexts map[string]struct {
			Endpoints []string `yaml:"endpoints"`
			Crt       string   `yaml:"crt"`
		} `yaml:"contexts"`
	}
	require.NoError(t, yaml.Unmarshal(got, &cfg))
	ctx := cfg.Contexts["admin"]
	assert.Equal(t, []string{"10.0.0.5"}, ctx.Endpoints)
	der, err := base64.StdEncoding.DecodeString(ctx.Crt)
	require.NoError(t, err)
	block, _ := pem.Decode(der)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, []string{"os:admin"}, cert.Subject.Organization)
	assert.Equal(t, now.Add(10*time.Minute), cert.NotAfter.UTC())
}
