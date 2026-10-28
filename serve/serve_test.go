package serve_test

// A serving server hands one file, once, over TLS 1.3 to the one address allowed to fetch it, behind a pinned key.

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/serve"
)

func TestNewKey_PinsTheSHA256OfTheCertificatesPublicKey(t *testing.T) {
	key, err := serve.NewKey("a serving server", time.Now())
	require.NoError(t, err)

	block, _ := pem.Decode([]byte(key.CertPEM))
	require.NotNil(t, block, "the certificate is not PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	require.NoError(t, err)
	sum := sha256.Sum256(spki)

	assert.Equal(t, "sha256//"+base64.StdEncoding.EncodeToString(sum[:]), key.Pin)
	assert.Equal(t, "a serving server", cert.Subject.CommonName)
	assert.Contains(t, key.KeyPEM, "PRIVATE KEY")
}

func TestUserData_WritesTheKeyTheProgramAndTheFilesThenRunsTheCommands(t *testing.T) {
	key, err := serve.NewKey("a serving server", time.Now())
	require.NoError(t, err)
	payload, err := serve.Gzipped([]byte("the file served"))
	require.NoError(t, err)
	file := serve.File{Path: serve.Dir + "/payload", Permissions: "0600", Content: payload, Encoding: serve.GzipBase64}
	command := serve.Command(netip.MustParseAddr("10.0.0.9"), serve.DefaultPort, file.Path)

	out, err := serve.UserData(append(key.Files(), file), command)

	require.NoError(t, err)
	require.True(t, strings.HasPrefix(out, "#cloud-config\n"), "the user data is not a cloud-config: %.40s", out)
	var config struct {
		WriteFiles []serve.File `yaml:"write_files"`
		RunCmd     [][]string   `yaml:"runcmd"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(out), &config))
	written := map[string]serve.File{}
	for _, f := range config.WriteFiles {
		written[f.Path] = f
	}
	assert.Equal(t, key.CertPEM, written[serve.CertPath].Content)
	assert.Equal(t, key.KeyPEM, written[serve.KeyPath].Content)
	assert.Equal(t, "0600", written[serve.KeyPath].Permissions)
	assert.Equal(t, serve.Program, written[serve.ProgramPath].Content)
	assert.Equal(t, file, written[file.Path])
	assert.Equal(t, [][]string{{"python3", "-I", serve.ProgramPath, "10.0.0.9", strconv.Itoa(serve.DefaultPort), serve.CertPath, serve.KeyPath, file.Path}}, config.RunCmd)
}

func TestUserData_RefusesUserDataOverHetznersLimit(t *testing.T) {
	big := serve.File{Path: serve.Dir + "/payload", Permissions: "0600", Content: strings.Repeat("x", gateway.UserDataLimit)}

	_, err := serve.UserData([]serve.File{big})

	require.Error(t, err)
	assert.Contains(t, err.Error(), strconv.Itoa(gateway.UserDataLimit))
}

func TestProgram_ServesTheFileOnceToItsClientOverTLS13AndRemovesTheFileAndTheKey(t *testing.T) {
	key, err := serve.NewKey("a serving server", time.Now())
	require.NoError(t, err)
	dir := t.TempDir()
	paths := map[string]string{"cert": filepath.Join(dir, "tls.crt"), "key": filepath.Join(dir, "tls.key"), "file": filepath.Join(dir, "hosting.yaml"), "program": filepath.Join(dir, "serve.py")}
	for name, content := range map[string]string{"cert": key.CertPEM, "key": key.KeyPEM, "file": "the hosting set", "program": serve.Program} {
		require.NoError(t, os.WriteFile(paths[name], []byte(content), 0o600))
	}
	port := freePort(t)
	server := exec.Command("python3", "-I", paths["program"], "127.0.0.1", strconv.Itoa(port), paths["cert"], paths["key"], paths["file"])
	output := &strings.Builder{}
	server.Stdout, server.Stderr = output, output
	require.NoError(t, server.Start())
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()
	t.Cleanup(func() { _ = server.Process.Kill() })
	url := "https://127.0.0.1:" + strconv.Itoa(port) + "/hosting.yaml"

	first := fetch(t, url, key.Pin)
	select {
	case err := <-exited:
		require.NoError(t, err, output.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("the serving program still runs after serving once:\n%s", output.String())
	}

	assert.Equal(t, "the hosting set", first)
	assert.NoFileExists(t, paths["file"], "the file outlives its one fetch")
	assert.NoFileExists(t, paths["key"], "the TLS key outlives the program's start")
}

func TestProgram_AnswersNotFoundForAnyOtherPathAndKeepsServing(t *testing.T) {
	key, err := serve.NewKey("a serving server", time.Now())
	require.NoError(t, err)
	dir := t.TempDir()
	for name, content := range map[string]string{"tls.crt": key.CertPEM, "tls.key": key.KeyPEM, "hosting.yaml": "the hosting set", "serve.py": serve.Program} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	port := freePort(t)
	server := exec.Command("python3", "-I", filepath.Join(dir, "serve.py"), "127.0.0.1", strconv.Itoa(port),
		filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "hosting.yaml"))
	require.NoError(t, server.Start())
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	base := "https://127.0.0.1:" + strconv.Itoa(port)

	status := curlStatus(t, base+"/image.raw", key.Pin)
	served := fetch(t, base+"/hosting.yaml", key.Pin)

	assert.Equal(t, "404", status)
	assert.Equal(t, "the hosting set", served)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// fetch reads url as a builder does, trusting nothing but the pinned public key, and retries while the program starts.
func fetch(t *testing.T, url, pin string) string {
	t.Helper()
	var out []byte
	var err error
	for range 50 {
		out, err = exec.Command("curl", "-sSf", "--tlsv1.3", "-k", "--pinnedpubkey", pin, url).Output()
		if err == nil {
			return string(out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, err, "fetching %s", url)
	return ""
}

func curlStatus(t *testing.T, url, pin string) string {
	t.Helper()
	var out []byte
	var err error
	for range 50 {
		out, err = exec.Command("curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-k", "--pinnedpubkey", pin, url).Output()
		if err == nil {
			return string(out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, err, "fetching %s", url)
	return ""
}
