package gateway_test

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/mirror"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func inputs(t *testing.T) gateway.Inputs {
	t.Helper()
	tools, err := components.ParseImage(testutil.ToolsImage)
	require.NoError(t, err)
	list, err := gateway.NewList(components.Default(), tools)
	require.NoError(t, err)
	return gateway.Inputs{
		Address:         netip.MustParseAddr("10.0.0.2"),
		Network:         netip.MustParsePrefix("10.0.0.0/16"),
		Ingress:         netip.MustParseAddr("198.51.100.7"),
		TLS:             testutil.GatewayTLS(t),
		List:            list,
		ManifestBaseURL: gateway.ReleaseURL("v0.1.0"),
		Logs:            testutil.LogExport(t),
	}
}

func TestRender_MirrorsAndServesTheListItIsGiven(t *testing.T) {
	tools, err := components.ParseImage(testutil.ToolsImage)
	require.NoError(t, err)
	set := components.HostingServed()
	in := inputs(t)
	in.List, err = gateway.NewList(set, tools)
	require.NoError(t, err)

	got, err := gateway.Render(in)

	require.NoError(t, err)
	images, err := mirror.List(set, tools)
	require.NoError(t, err)
	for _, image := range images {
		assert.Contains(t, string(got), image.Ref()+"\n", "the gateway mirrors %s", image.Ref())
	}
	for _, m := range set.Served {
		assert.Contains(t, string(got), m.TalosName()+".yaml "+m.SHA256()+" ", "the gateway serves %s", m.Name)
	}
}

func TestList_MirrorsThePostgres16ImagePostgresDBKubePinsAsCloudNativePGsDefault(t *testing.T) {
	const postgres16 = "ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie@sha256:8ca5699981c25db72e549e9fa69561bac233df15b9b1e3a6282847b0629bb5ea"
	set := components.Default()

	images, err := mirror.List(set)

	require.NoError(t, err)
	operator := string(testutil.Manifest(t, "cloudnative-pg", set.Served, set.Inline).Bytes)
	assert.True(t, strings.Contains(operator, "\n  POSTGRES_IMAGE_NAME: "+postgres16+"\n"), "CloudNativePG's default image is not Postgres 16.15")
	assert.True(t, strings.Contains(string(mirror.Render(images)), postgres16+"\n"), "the mirror list lacks the Postgres 16.15 image")
	assert.False(t, strings.Contains(string(mirror.Render(images)), "postgresql:18.6"), "the mirror list holds a Postgres 18.6 image")
}

func TestRender_MatchesTheGoldenCloudConfigWithinHetznersUserDataLimit(t *testing.T) {
	in := inputs(t)

	got, err := gateway.Render(in)

	require.NoError(t, err)
	assert.LessOrEqual(t, len(got), gateway.UserDataLimit)
	require.Len(t, testutil.Documents(t, got), 1, "the cloud-config is not one YAML document")
	testutil.Golden(t, filepath.Join("testdata", "golden", "cloud-config.yaml"), got)
}

func TestRender_WithNoLogReceiverForwardsHTTPSAloneAndChangesNothingElse(t *testing.T) {
	const withLogPort, httpsAlone = "tcp dport { 443, 4317 } accept", "tcp dport { 443 } accept"
	in := inputs(t)
	in.Logs = components.LogExport{}
	withReceiver, err := gateway.Render(inputs(t))
	require.NoError(t, err)
	require.Contains(t, string(withReceiver), withLogPort)

	got, err := gateway.Render(in)

	require.NoError(t, err)
	assert.Equal(t, strings.Replace(string(withReceiver), withLogPort, httpsAlone, 1), string(got))
}

func TestRender_ForwardsPort80ToTheIngressLoadBalancerAloneAndNowhereWithoutOne(t *testing.T) {
	const toIngress = "          ip saddr 10.0.0.0/16 ip daddr 198.51.100.7 tcp dport 80 accept\n"
	in := inputs(t)
	in.Ingress = netip.Addr{}
	withIngress, err := gateway.Render(inputs(t))
	require.NoError(t, err)

	got, err := gateway.Render(in)

	require.NoError(t, err)
	require.Contains(t, string(withIngress), toIngress, "cert-manager's HTTP-01 self-check to an app's hostname")
	assert.Len(t, regexp.MustCompile(`\b80\b`).FindAllString(writtenFile(t, "/etc/nftables.conf"), -1), 1, "the firewall's rules naming port 80")
	assert.Equal(t, strings.Replace(string(withIngress), toIngress, "", 1), string(got))
}

func TestRender_RefusesInputsItCannotServe(t *testing.T) {
	valid := inputs(t)
	for want, change := range map[string]func(*gateway.Inputs){
		"ingress address 10.0.0.4 is inside network 10.0.0.0/16": func(in *gateway.Inputs) { in.Ingress = netip.MustParseAddr("10.0.0.4") },
		"192.168.0.2 is not inside network 10.0.0.0/16":          func(in *gateway.Inputs) { in.Address = netip.MustParseAddr("192.168.0.2") },
		"the gateway has no list; build one with NewList":        func(in *gateway.Inputs) { in.List = gateway.List{} },
	} {
		in := valid
		change(&in)
		_, err := gateway.Render(in)
		assert.ErrorContains(t, err, want)
	}
}

func TestRender_LeavesTheFirewallToNftablesAndRunsEveryContainerOnTheHostNetwork(t *testing.T) {
	var daemon map[string]any
	require.NoError(t, json.Unmarshal([]byte(writtenFile(t, "/etc/docker/daemon.json")), &daemon))
	assert.Equal(t, map[string]any{"iptables": false, "ip6tables": false, "bridge": "none"}, daemon,
		"Docker writes iptables rules, which the gateway's flush ruleset deletes")
	assert.True(t, strings.HasPrefix(writtenFile(t, "/etc/nftables.conf"), "#!/usr/sbin/nft -f\nflush ruleset\n"))

	for _, unit := range []string{"mitstack-registry", "mitstack-caddy"} {
		start := execStart(t, writtenFile(t, "/etc/systemd/system/"+unit+".service"))
		assert.Contains(t, start, " --network host ", "%s runs off the host network", unit)
		assert.NotRegexp(t, `\s(-p|--publish)\s`, start, "%s publishes a port, which needs Docker's rules", unit)
	}
	assert.Contains(t, execStart(t, writtenFile(t, "/etc/systemd/system/mitstack-registry.service")),
		" -e REGISTRY_HTTP_ADDR=127.0.0.1:5000 ", "on the host network the registry must be told to bind 127.0.0.1:5000 alone")
}

func TestRender_SendsMirrorPullsToTheRegistryAndRefusesEveryOtherMethod(t *testing.T) {
	caddyfile := writtenFile(t, "/etc/mitstack-gateway/Caddyfile")

	assert.Contains(t, caddyfile, "@pull method GET HEAD\n")
	assert.NotContains(t, caddyfile, "handle /v2/*", "a handle block runs respond before reverse_proxy, so every pull answers 405")
	var block []string
	inRoute := false
	for line := range strings.Lines(caddyfile) {
		line = strings.TrimSpace(line)
		switch {
		case line == "route /v2/* {":
			inRoute = true
		case inRoute && line == "}":
			inRoute = false
		case inRoute && !strings.HasPrefix(line, "#"):
			block = append(block, line)
		}
	}
	assert.Equal(t, []string{"reverse_proxy @pull 127.0.0.1:5000", "respond 405"}, block,
		"the /v2/ route does not try the registry for a pull before refusing")
}

func writtenFile(t *testing.T, path string) string {
	t.Helper()
	rendered, err := gateway.Render(inputs(t))
	require.NoError(t, err)
	for _, f := range testutil.Field(t, testutil.Documents(t, rendered)[0], "write_files").([]any) {
		if file := f.(map[string]any); file["path"] == path {
			return file["content"].(string)
		}
	}
	require.Fail(t, "the cloud-config writes no "+path)
	return ""
}

func execStart(t *testing.T, unit string) string {
	t.Helper()
	for line := range strings.Lines(unit) {
		if rest, ok := strings.CutPrefix(line, "ExecStart="); ok {
			return rest
		}
	}
	require.Fail(t, "the unit has no ExecStart", unit)
	return ""
}

var caGateways = []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.248"), netip.MustParseAddr("10.0.0.249")}

func TestCAIssue_ServesEachGatewaysAddressUnderTheClustersOneCA(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	ca, err := gateway.NewCA(now, 10*365*24*time.Hour, caGateways)
	require.NoError(t, err)

	for _, address := range caGateways {
		tls, err := ca.Issue(address, now, 24*time.Hour)

		require.NoError(t, err)
		assert.Equal(t, ca.Cert, tls.CA)
		cert, err := tls.Leaf()
		require.NoError(t, err)
		require.NoError(t, verify(ca, cert, address.String(), now), "the certificate for %s under the CA", address)
		assert.Equal(t, []net.IP{address.AsSlice()}, cert.IPAddresses)
		assert.Equal(t, now.Add(24*time.Hour), cert.NotAfter.UTC())
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.pem"), ca.Cert, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "leaf.pem"), tls.Cert, 0o600))
		out, err := exec.Command("openssl", "verify", "-attime", strconv.FormatInt(now.Add(time.Minute).Unix(), 10), "-CAfile", filepath.Join(dir, "ca.pem"),
			"-verify_ip", address.String(), filepath.Join(dir, "leaf.pem")).CombinedOutput()
		require.NoError(t, err, "openssl refuses the certificate for %s: %s", address, out)
	}
}

func TestNewCA_RefusesACertificateForAnyOtherAddressOrAHostNameUnderCriticalNameConstraints(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	ca, err := gateway.NewCA(now, time.Hour, caGateways)
	require.NoError(t, err)
	caCert, caKey := parseCA(t, ca)
	constraints := slices.IndexFunc(caCert.Extensions, func(e pkix.Extension) bool { return e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 30}) })
	require.GreaterOrEqual(t, constraints, 0, "the CA carries no name constraints")
	assert.True(t, caCert.Extensions[constraints].Critical, "the CA's name constraints are not critical")

	other, err := ca.Issue(netip.MustParseAddr("10.0.0.7"), now, time.Hour)
	require.NoError(t, err)
	leaf, err := other.Leaf()
	require.NoError(t, err)
	assert.Error(t, verify(ca, leaf, "10.0.0.7", now), "a certificate for an address that is no gateway's")
	template := &x509.Certificate{SerialNumber: big.NewInt(7), DNSNames: []string{"registry.example.com"}, NotBefore: now, NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, caKey.Public(), caKey)
	require.NoError(t, err)
	named, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	assert.Error(t, verify(ca, named, "registry.example.com", now), "a certificate for a host name")
}

func verify(ca gateway.CA, cert *x509.Certificate, name string, now time.Time) error {
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.Cert)
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now.Add(time.Minute), DNSName: name})
	return err
}

func parseCA(t *testing.T, ca gateway.CA) (*x509.Certificate, crypto.Signer) {
	t.Helper()
	certBlock, _ := pem.Decode(ca.Cert)
	keyBlock, _ := pem.Decode(ca.Key)
	require.NotNil(t, certBlock)
	require.NotNil(t, keyBlock)
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	require.NoError(t, err)
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	require.NoError(t, err)
	return cert, key
}

// fakeCurl writes "bytes of <url>" to the -o path, so a test knows each download's sha256.
const fakeCurl = `
out="" url=""
while [ "$#" -gt 0 ]; do
  case $1 in
    -o) out=$2; shift 2 ;;
    https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
printf 'bytes of %s' "$url" > "$out"
`

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// prepareRun is one run of the prepare script the rendered cloud-config carries, against fake curl and skopeo.
type prepareRun struct {
	tools *testutil.Tools
	dir   string
	out   string
	err   error
}

const cilium = "quay.io/cilium/cilium:v1.19.9@sha256:f3014d42121a04272cb4a5f4f96dbe7836c9a7919990f17e9b6838e8fd4aa33a"

func runPrepare(t *testing.T, manifestSHA256 string) prepareRun {
	t.Helper()
	return prepare(t, map[string]string{"skopeo": "", "docker": ""}, manifestSHA256, cilium)
}

// prepare runs the rendered prepare script over images with fake curl, systemctl and the fakes given, and a tools.env
// that pins fake talosctl and kubectl downloads beside every other line of the gateway's own.
func prepare(t *testing.T, fakes map[string]string, manifestSHA256 string, images ...string) prepareRun {
	t.Helper()
	script := writtenFile(t, "/usr/local/bin/mitstack-gateway-prepare")
	fakes["curl"], fakes["systemctl"] = fakeCurl, ""
	run := prepareRun{tools: testutil.NewTools(t, fakes), dir: t.TempDir()}
	write := func(name, content string) string {
		path := filepath.Join(run.dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	talosctl := "https://github.com/siderolabs/talos/releases/download/v9.9.9/talosctl-linux-amd64"
	kubectl := "https://dl.k8s.io/release/v9.9.9/bin/linux/amd64/kubectl"
	env := "TALOS_VERSION=v9.9.9\nTALOSCTL_SHA256=" + sha256Of("bytes of "+talosctl) +
		"\nKUBERNETES_VERSION=v9.9.9\nKUBECTL_SHA256=" + sha256Of("bytes of "+kubectl) + "\n"
	for line := range strings.Lines(writtenFile(t, "/etc/mitstack-gateway/tools.env")) {
		if !strings.HasPrefix(line, "TALOS") && !strings.HasPrefix(line, "KUBE") {
			env += line
		}
	}
	toolsEnv := write("tools.env", env)
	imageList := write("images.txt", strings.Join(images, "\n")+"\n")
	manifests := write("manifests.txt", "argocd.yaml "+manifestSHA256+" https://example.test/argocd.yaml\n")
	require.NoError(t, os.Mkdir(filepath.Join(run.dir, "bin"), 0o755))

	run.out, run.err = testutil.RunBash(t, script, run.tools.Env("PREPARE_BIN_DIR="+filepath.Join(run.dir, "bin")),
		toolsEnv, imageList, manifests, filepath.Join(run.dir, "manifests"), run.completionFile())
	return run
}

func skopeoImage(t *testing.T) string {
	t.Helper()
	for line := range strings.Lines(writtenFile(t, "/etc/mitstack-gateway/tools.env")) {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(line), "SKOPEO_IMAGE="); ok {
			_, err := components.ParseImage(ref)
			require.NoError(t, err, "the gateway's skopeo image is not pinned by its digest")
			return ref
		}
	}
	require.Fail(t, "the gateway's tools.env pins no SKOPEO_IMAGE")
	return ""
}

func (r prepareRun) completionFile() string {
	return filepath.Join(r.dir, "state", "mirror-complete")
}

func TestPrepareScript_MarksTheMirrorCompleteOnlyOnceEveryImageAndManifestIsInPlace(t *testing.T) {
	done := runPrepare(t, sha256Of("bytes of https://example.test/argocd.yaml"))
	refused := runPrepare(t, sha256Of("other bytes"))

	require.NoError(t, done.err, done.out)
	assert.FileExists(t, done.completionFile(), "a prepare that filled the mirror and fetched every manifest")
	require.Error(t, refused.err)
	assert.NoFileExists(t, refused.completionFile(), "a prepare that refused a manifest marked the mirror complete")
}

// The cloud-config's own path for the file prepare writes last, which Caddy waits for.
const mirrorComplete = "/var/lib/mitstack-gateway/mirror-complete"

func TestRender_ServesNoMirrorRequestBeforePrepareHasFinished(t *testing.T) {
	rendered, err := gateway.Render(inputs(t))
	require.NoError(t, err)
	var runcmd [][]string
	for _, c := range testutil.Field(t, testutil.Documents(t, rendered)[0], "runcmd").([]any) {
		var args []string
		for _, a := range c.([]any) {
			args = append(args, a.(string))
		}
		runcmd = append(runcmd, args)
	}

	assert.Contains(t, writtenFile(t, "/etc/systemd/system/mitstack-caddy.service"), "\nConditionPathExists="+mirrorComplete+"\n",
		"Caddy can start, at the first boot or any later one, before prepare has filled the mirror")
	prepare := slices.IndexFunc(runcmd, func(c []string) bool { return c[0] == "/usr/local/bin/mitstack-gateway-prepare" })
	require.GreaterOrEqual(t, prepare, 0, "the cloud-config runs no prepare: %v", runcmd)
	assert.Equal(t, mirrorComplete, runcmd[prepare][len(runcmd[prepare])-1], "the file prepare writes once it has finished")
	start := slices.IndexFunc(runcmd, func(c []string) bool { return slices.Equal(c, []string{"systemctl", "start", "mitstack-caddy"}) })
	assert.Greater(t, start, prepare, "Caddy is not started once prepare has finished: %v", runcmd)
}

func TestPrepareScript_InstallsThePinnedToolsMirrorsByDigestAndFetchesTheManifests(t *testing.T) {
	run := runPrepare(t, sha256Of("bytes of https://example.test/argocd.yaml"))

	require.NoError(t, run.err, run.out)
	assert.FileExists(t, filepath.Join(run.dir, "bin", "talosctl"))
	assert.FileExists(t, filepath.Join(run.dir, "bin", "kubectl"))
	assert.FileExists(t, filepath.Join(run.dir, "manifests", "argocd.yaml"))
	calls := run.tools.Calls(t)
	assert.Contains(t, calls, "docker run --rm --network host "+skopeoImage(t)+
		" copy --all --preserve-digests --retry-times 3 --dest-tls-verify=false "+
		"docker://quay.io/cilium/cilium@sha256:f3014d42121a04272cb4a5f4f96dbe7836c9a7919990f17e9b6838e8fd4aa33a "+
		"docker://127.0.0.1:5000/quay.io/cilium/cilium:v1.19.9")
	assert.False(t, slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "skopeo ") }),
		"prepare ran the host's own skopeo, which no pin names: %v", calls)
}

func TestPrepareScript_PowersTheGatewayOffAndLeavesTheMirrorIncompleteWhenACopyFails(t *testing.T) {
	const trustManager = "quay.io/jetstack/trust-manager:v0.25.0@sha256:0521921c2a46d14d0417336b20bcde69215ef3164f2977cc939d53c6e5224a17"
	failTrustManager := `case "$*" in *trust-manager*) echo 'Instructed to preserve digests' >&2; exit 1 ;; esac`

	run := prepare(t, map[string]string{"docker": failTrustManager}, sha256Of("bytes of https://example.test/argocd.yaml"),
		cilium, trustManager)

	require.Error(t, run.err)
	assert.NoFileExists(t, run.completionFile(), "a prepare whose copy failed marked the mirror complete")
	assert.Contains(t, run.tools.Calls(t), "systemctl poweroff", "the gateway stays up with its mirror incomplete")
	assert.Contains(t, run.out, trustManager)
}

func TestPrepareScript_RefusesAManifestWhoseBytesDifferFromItsPin(t *testing.T) {
	run := runPrepare(t, sha256Of("other bytes"))

	require.Error(t, run.err)
	assert.Contains(t, run.out, "https://example.test/argocd.yaml does not have sha256")
	assert.NoFileExists(t, filepath.Join(run.dir, "manifests", "argocd.yaml"))
}
