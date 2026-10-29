package gateway_test

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

func inputs(t *testing.T) gateway.Inputs {
	t.Helper()
	tools, err := components.ParseImage(testutil.ToolsImage)
	require.NoError(t, err)
	return gateway.Inputs{
		Address:         netip.MustParseAddr("10.0.0.2"),
		Network:         netip.MustParsePrefix("10.0.0.0/16"),
		TLS:             testutil.GatewayTLS(t),
		Components:      components.Default(),
		ExtraImages:     []components.Image{tools},
		ManifestBaseURL: gateway.ReleaseURL("v0.1.0"),
		Logs:            testutil.LogExport(t),
	}
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

// writtenFile is the content the rendered cloud-config writes to path.
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

func TestRender_RefusesAnAddressOutsideTheNetwork(t *testing.T) {
	in := inputs(t)
	in.Address = netip.MustParseAddr("192.168.0.2")

	_, err := gateway.Render(in)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "192.168.0.2 is not inside network 10.0.0.0/16")
}

func TestCAIssue_ServesEachGatewaysAddressUnderTheClustersOneCA(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	ca, err := gateway.NewCA(now, 10*365*24*time.Hour)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(ca.Cert))

	for _, address := range []string{"10.0.0.2", "10.0.1.2", "10.0.2.2"} {
		tls, err := ca.Issue(netip.MustParseAddr(address), now, 24*time.Hour)

		require.NoError(t, err)
		assert.Equal(t, ca.Cert, tls.CA)
		block, _ := pem.Decode(tls.Cert)
		require.NotNil(t, block)
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		_, err = cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now.Add(time.Hour), DNSName: address})
		require.NoError(t, err, "the certificate for %s does not verify under the CA", address)
		assert.Equal(t, []net.IP{net.ParseIP(address).To4()}, cert.IPAddresses)
		assert.Equal(t, now.Add(24*time.Hour), cert.NotAfter.UTC())
	}
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

// skopeoImage is the skopeo image the gateway's tools.env pins, which prepare runs every copy with.
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

// completionFile is where the run's prepare writes that the mirror and the manifests are in place.
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
