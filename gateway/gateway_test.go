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

func TestList_KeepingMirrorsEachKeptImageOnceAndLeavesTheDigestTheReleaseListsAlone(t *testing.T) {
	in := inputs(t)
	release := in.List.Digest()
	kept, err := components.ParseImage("ghcr.io/acme/nightly-export:v2.1.0@sha256:2a3b62b53ddb9f167b63d22202a360811ba78df015021f704d01ee9abad4169c")
	require.NoError(t, err)
	tools, err := components.ParseImage(testutil.ToolsImage)
	require.NoError(t, err)

	in.List = in.List.Keeping([]components.Image{kept, tools, kept})
	got, err := gateway.Render(in)

	require.NoError(t, err)
	assert.Equal(t, release, in.List.Digest(), "a kept image never marks a gateway stale")
	assert.Equal(t, []components.Image{kept}, in.List.Kept(), "an image the release lists is not kept again")
	assert.Equal(t, 1, strings.Count(string(got), kept.Ref()+"\n"), "images.txt names the kept image once")
	assert.Equal(t, 1, strings.Count(string(got), tools.Ref()+"\n"), "images.txt names the release's image once")
}

func TestRender_RefusesACloudConfigPastHetznersLimitUnlessItTravelsGzipped(t *testing.T) {
	in, images := inputs(t), []components.Image{}
	for i := range 300 {
		images = append(images, components.Image{Name: "ghcr.io/example-org/service-" + strconv.Itoa(i), Tag: "v1", Digest: "sha256:" + strings.Repeat("a", 64)})
	}
	in.List = in.List.Keeping(images)

	_, err := gateway.Render(in)
	assert.ErrorContains(t, err, "over Hetzner's", "a cloud-config its caller sends as text")
	in.Gzipped = true
	_, err = gateway.Render(in)
	assert.NoError(t, err, "a cloud-config its caller gzips and judges once compressed")
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
	took  time.Duration // the script's own run, without the render that sets it up
}

const cilium = "quay.io/cilium/cilium:v1.19.9@sha256:f3014d42121a04272cb4a5f4f96dbe7836c9a7919990f17e9b6838e8fd4aa33a"

const talosctlURL = "https://github.com/siderolabs/talos/releases/download/v9.9.9/talosctl-linux-amd64"

func runPrepare(t *testing.T, manifestSHA256 string) prepareRun {
	t.Helper()
	return prepare(t, map[string]string{"skopeo": "", "docker": ""}, manifestSHA256, cilium)
}

// prepare runs the rendered prepare script over images with fake curl, systemctl and the fakes given, and a tools.env
// that pins fake talosctl and kubectl downloads beside every other line of the gateway's own.
func prepare(t *testing.T, fakes map[string]string, manifestSHA256 string, images ...string) prepareRun {
	t.Helper()
	return prepareWith(t, nil, fakes, manifestSHA256, images...)
}

// prepareWith is prepare with settings appended to tools.env, which win over the gateway's own lines, a fake curl only
// when fakes holds none, and the console and issue file in the run's own directory.
func prepareWith(t *testing.T, settings []string, fakes map[string]string, manifestSHA256 string, images ...string) prepareRun {
	t.Helper()
	script := writtenFile(t, "/usr/local/bin/mitstack-gateway-prepare")
	if _, ok := fakes["curl"]; !ok {
		fakes["curl"] = fakeCurl
	}
	fakes["systemctl"] = ""
	run := prepareRun{tools: testutil.NewTools(t, fakes), dir: t.TempDir()}
	settings = append([]string{"GATEWAY_CONSOLE=" + run.console(), "GATEWAY_ISSUE=" + run.issue()}, settings...)
	write := func(name, content string) string {
		path := filepath.Join(run.dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	kubectl := "https://dl.k8s.io/release/v9.9.9/bin/linux/amd64/kubectl"
	env := "TALOS_VERSION=v9.9.9\nTALOSCTL_SHA256=" + sha256Of("bytes of "+talosctlURL) +
		"\nKUBERNETES_VERSION=v9.9.9\nKUBECTL_SHA256=" + sha256Of("bytes of "+kubectl) + "\n"
	for line := range strings.Lines(writtenFile(t, "/etc/mitstack-gateway/tools.env")) {
		if !strings.HasPrefix(line, "TALOS") && !strings.HasPrefix(line, "KUBE") {
			env += line
		}
	}
	toolsEnv := write("tools.env", env+strings.Join(settings, "\n")+"\n")
	imageList := write("images.txt", strings.Join(images, "\n")+"\n")
	manifests := write("manifests.txt", "argocd.yaml "+manifestSHA256+" https://example.test/argocd.yaml\n")
	require.NoError(t, os.Mkdir(filepath.Join(run.dir, "bin"), 0o755))

	began := time.Now()
	run.out, run.err = testutil.RunBash(t, script, run.tools.Env("PREPARE_BIN_DIR="+filepath.Join(run.dir, "bin")),
		toolsEnv, imageList, manifests, filepath.Join(run.dir, "manifests"), run.completionFile())
	run.took = time.Since(began)
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

// console stands in for the VGA console, and issue for the login prompt's issue file.
func (r prepareRun) console() string { return filepath.Join(r.dir, "tty1") }
func (r prepareRun) issue() string   { return filepath.Join(r.dir, "issue.d", "mitstack-gateway.issue") }

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
	var runcmd []string
	for _, c := range testutil.Field(t, testutil.Documents(t, rendered)[0], "runcmd").([]any) {
		var args []string
		for _, a := range c.([]any) {
			args = append(args, a.(string))
		}
		runcmd = append(runcmd, strings.Join(args, " "))
	}

	assert.Contains(t, writtenFile(t, "/etc/systemd/system/mitstack-caddy.service"), "\nConditionPathExists="+mirrorComplete+"\n",
		"Caddy can start, at the first boot or any later one, before prepare has filled the mirror")
	prepare := slices.Index(runcmd, "systemctl start mitstack-gateway-prepare")
	require.GreaterOrEqual(t, prepare, 0, "the cloud-config runs no prepare: %v", runcmd)
	execStart := strings.Fields(execStart(t, writtenFile(t, "/etc/systemd/system/mitstack-gateway-prepare.service")))
	assert.Equal(t, "/usr/local/bin/mitstack-gateway-prepare", execStart[0])
	assert.Equal(t, mirrorComplete, execStart[len(execStart)-1], "the file prepare writes once it has finished")
	start := slices.Index(runcmd, "systemctl start mitstack-caddy")
	assert.Greater(t, start, prepare, "Caddy is not started once prepare has finished: %v", runcmd)
	serve := slices.Index(runcmd, "systemctl start mitstack-gateway-serve")
	assert.Greater(t, serve, start, "the TLS server's start is not watched once Caddy starts: %v", runcmd)
}

// keyValues reads a systemd unit or tools.env written to path as its key=value lines.
func keyValues(t *testing.T, path string) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for line := range strings.Lines(writtenFile(t, path)) {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			keys[key] = value
		}
	}
	return keys
}

func unit(t *testing.T, name string) map[string]string {
	return keyValues(t, "/etc/systemd/system/"+name)
}

// bound reads a positive whole number of seconds.
func bound(t *testing.T, seconds string) time.Duration {
	t.Helper()
	n, err := strconv.Atoi(seconds)
	require.NoError(t, err)
	require.Positive(t, n)
	return time.Duration(n) * time.Second
}

func TestRender_RunsThePreparationAndTheTLSServersStartEachWithinItsBoundAndPowersOffOnEitherFailing(t *testing.T) {
	prepareUnit, serveUnit := unit(t, "mitstack-gateway-prepare.service"), unit(t, "mitstack-gateway-serve.service")
	env := keyValues(t, "/etc/mitstack-gateway/tools.env")

	for name, u := range map[string]map[string]string{"prepare": prepareUnit, "serve": serveUnit} {
		assert.Equal(t, "poweroff", u["FailureAction"], "the %s unit's failure or overrun leaves the gateway running", name)
		assert.Equal(t, "oneshot", u["Type"], "the %s unit is started before it has finished", name)
	}
	assert.Less(t, bound(t, env["DOWNLOAD_CONNECT_SECONDS"]), bound(t, env["DOWNLOAD_SECONDS"]))
	assert.Less(t, bound(t, env["COPY_SECONDS"]), bound(t, prepareUnit["TimeoutStartSec"]), "one copy may take the whole preparation")
	assert.Equal(t, mirrorComplete, serveUnit["ConditionPathExists"], "the TLS server's start is watched before the mirror is complete")
	assert.Contains(t, serveUnit["ExecStart"], "https://10.0.0.2/v2/", "the start is watched where the nodes pull")
	assert.Greater(t, gateway.Deadline, bound(t, prepareUnit["TimeoutStartSec"])+bound(t, serveUnit["TimeoutStartSec"]),
		"the plane's deadline leaves the boot no time before the preparation starts")
}

// stopPost runs a unit's ExecStopPost as systemd would after the unit ended with result, with the console and the
// issue file moved into dir, and returns what each holds.
func stopPost(t *testing.T, name, result, dir string, replace ...string) (console, issue string) {
	t.Helper()
	replace = append(replace, "$$", "$", "/dev/tty1", filepath.Join(dir, "tty1"), "/etc/issue.d/mitstack-gateway.issue", filepath.Join(dir, "issue"))
	command := strings.NewReplacer(replace...).Replace(unit(t, name)["ExecStopPost"])
	fields := strings.SplitN(command, " ", 3)
	require.Equal(t, []string{"/bin/sh", "-c"}, fields[:2], "the unit's ExecStopPost")
	cmd := exec.Command("sh", "-c", strings.Trim(fields[2], "'"))
	cmd.Env = []string{"SERVICE_RESULT=" + result, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(dir, name)); return string(b) }
	return read("tty1"), read("issue")
}

// unfolded is a console message with the line breaks its folding added taken out again.
func unfolded(message string) string { return strings.ReplaceAll(message, "\n", "") }

// assertOnOneScreen fails a console message that does not lead with what failed, or would not fit one 80 by 25 screen
// beside its login prompt.
func assertOnOneScreen(t *testing.T, message, stage string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(message, "\n"), "\n")
	assert.True(t, strings.HasPrefix(lines[0], "mitstack gateway: "), "the console's first line: %q", lines[0])
	assert.Contains(t, unfolded(message), stage, "the stage the console names")
	assert.Contains(t, unfolded(message), "powers itself off")
	assert.LessOrEqual(t, len(lines), 20, "lines on the console: %q", message)
	for _, line := range lines {
		assert.LessOrEqual(t, len(line), 80, "a console line: %q", line)
	}
}

func TestRender_ATLSServerThatDoesNotStartWritesTheStageAndCurlsErrorToTheVGAConsoleAndTheIssueFile(t *testing.T) {
	dir := t.TempDir()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	serveUnit := unit(t, "mitstack-gateway-serve.service")
	start := strings.NewReplacer("$$", "$", "10.0.0.2", closed.Addr().String(), "/etc/mitstack-gateway/ca.crt", "/dev/null",
		"/run/mitstack-gateway-serve.err", filepath.Join(dir, "serve.err")).Replace(serveUnit["ExecStart"])
	fields := strings.SplitN(start, " ", 3)
	require.Equal(t, []string{"/bin/sh", "-c"}, fields[:2], "the unit's ExecStart")
	began := time.Now()

	err = exec.Command("timeout", "3", "sh", "-c", strings.Trim(fields[2], "'")).Run()

	assert.Error(t, err, "the start of a TLS server that never answers ended")
	assert.Less(t, time.Since(began), 10*time.Second, "systemd's TimeoutStartSec, which timeout stands in for, could not end the wait")
	console, issue := stopPost(t, "mitstack-gateway-serve.service", "timeout", dir, "/run/mitstack-gateway-serve.err", filepath.Join(dir, "serve.err"))
	assertOnOneScreen(t, console, "the TLS server start")
	assert.Contains(t, unfolded(console), "within "+serveUnit["TimeoutStartSec"]+"s (timeout)")
	assert.Contains(t, unfolded(console), "curl: (7)", "the console names curl's last error")
	assert.Equal(t, console, issue, "the login prompt's issue file shows the failure again on every later boot")
	console, _ = stopPost(t, "mitstack-gateway-serve.service", "success", t.TempDir())
	assert.Empty(t, console, "a TLS server that started writes nothing to the console")
	prepared := t.TempDir()
	console, issue = stopPost(t, "mitstack-gateway-prepare.service", "timeout", prepared)
	assertOnOneScreen(t, console, "the preparation ended with timeout")
	assert.Equal(t, console, issue)
	again, issueAgain := stopPost(t, "mitstack-gateway-prepare.service", "exit-code", prepared)
	assert.Equal(t, []string{console, issue}, []string{again, issueAgain}, "a message already on the screen, as the script's is, stays the only one")
}

func TestRender_NamesTheVGAConsoleAndTheIssueFileAsWhereABuildFailureIsWritten(t *testing.T) {
	env := keyValues(t, "/etc/mitstack-gateway/tools.env")

	assert.Equal(t, []string{"/dev/tty1", "/etc/issue.d/mitstack-gateway.issue"}, []string{env["GATEWAY_CONSOLE"], env["GATEWAY_ISSUE"]})
	rendered, err := gateway.Render(inputs(t))
	require.NoError(t, err)
	for key := range testutil.Documents(t, rendered)[0] {
		assert.NotContains(t, []string{"users", "ssh_authorized_keys", "chpasswd", "password", "ssh_pwauth"}, key,
			"the console is the gateway's only screen, and it holds no login")
	}
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

const scriptAllowance = 5 * time.Second // what the script's own steps around a stage may add to its bound when loaded

// assertPoweredOff fails a run that did not stop at stage within bound, power the gateway off and write why to the
// console and the issue file.
func assertPoweredOff(t *testing.T, run prepareRun, took, bound time.Duration, stage, why string) {
	t.Helper()
	require.Error(t, run.err, run.out)
	assert.Less(t, took, bound, "the stage ran past its bound: %s", run.out)
	assert.Contains(t, run.tools.Calls(t), "systemctl poweroff", "the gateway stays up with its build stopped")
	assert.NoFileExists(t, run.completionFile())
	console, err := os.ReadFile(run.console())
	require.NoError(t, err, "nothing reached the console: %s", run.out)
	assertOnOneScreen(t, string(console), stage)
	assert.Contains(t, unfolded(string(console)), why, "the console names the stage's error")
	issue, err := os.ReadFile(run.issue())
	require.NoError(t, err)
	assert.Equal(t, string(console), string(issue), "the login prompt's issue file shows the failure again on every later boot")
}

func TestPrepareScript_PowersTheGatewayOffNamingTheDownloadWhenTheTalosctlDownloadHangs(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0") // the kernel completes the handshake, and nothing ever answers
	require.NoError(t, err)
	t.Cleanup(func() { _ = silent.Close() })
	curl, err := exec.LookPath("curl")
	require.NoError(t, err)
	hanging := `case "$*" in *` + talosctlURL + `*) exec ` + curl + ` "${@/#https:\/\/github.com\/*/https://` + silent.Addr().String() + `/}" ;; esac
` + fakeCurl

	run := prepareWith(t, []string{"DOWNLOAD_CONNECT_SECONDS=1", "DOWNLOAD_SECONDS=2"}, map[string]string{"docker": "", "curl": hanging},
		sha256Of("bytes of https://example.test/argocd.yaml"), cilium)

	assertPoweredOff(t, run, run.took, 2*time.Second+scriptAllowance, "the download of "+talosctlURL, "curl: (28)")
	assert.NoFileExists(t, filepath.Join(run.dir, "bin", "talosctl"))
}

func TestPrepareScript_PowersTheGatewayOffNamingTheManifestDownloadThatAnsweredAnError(t *testing.T) {
	refusing := `case "$*" in *argocd.yaml*) echo 'curl: (22) The requested URL returned error: 503' >&2; exit 22 ;; esac
` + fakeCurl

	run := prepare(t, map[string]string{"docker": "", "curl": refusing}, sha256Of("bytes of https://example.test/argocd.yaml"), cilium)

	assertPoweredOff(t, run, 0, time.Second, "the download of https://example.test/argocd.yaml", "curl: (22)")
}

func TestPrepareScript_PowersTheGatewayOffNamingTheImageWhoseCopyHangs(t *testing.T) {
	run := prepareWith(t, []string{"COPY_SECONDS=1"}, map[string]string{"docker": "exec sleep 60"},
		sha256Of("bytes of https://example.test/argocd.yaml"), cilium)

	assertPoweredOff(t, run, run.took, 1*time.Second+scriptAllowance, "the copy of "+cilium, "did not copy into the gateway's image mirror within 1s")
}

func TestPrepareScript_FinishesALargeListCopiedSlowlyButSteadilyWithinEachCopysBound(t *testing.T) {
	const trustManager = "quay.io/jetstack/trust-manager:v0.25.0@sha256:0521921c2a46d14d0417336b20bcde69215ef3164f2977cc939d53c6e5224a17"
	images := []string{cilium, trustManager, cilium, trustManager, cilium}

	run := prepareWith(t, []string{"COPY_SECONDS=2"}, map[string]string{"docker": "sleep 0.6"},
		sha256Of("bytes of https://example.test/argocd.yaml"), images...)

	require.NoError(t, run.err, run.out)
	assert.FileExists(t, run.completionFile(), "a build whose every copy finished inside its bound, though all of them took longer than one")
	assert.NotContains(t, run.tools.Calls(t), "systemctl poweroff")
	assert.NoFileExists(t, run.console(), "a build that finished wrote to the console")
}
