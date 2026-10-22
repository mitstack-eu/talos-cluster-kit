package openbaoinit_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	fakeRootToken   = "s.fake-root-token-0123456789"
	fakeRecoveryKey = "6b1e0c3a5d7f9b2e4c6a8d0f1b3e5c7a9d2f4b6e8c0a1d3f5b7e9c2a4d6f8b0e"
	fakeAccessor    = "auth_kubernetes_f1e2d3c4"
	readToken       = "ghs_fake-read-token"
	writeToken      = "ghs_fake-write-token"
	contentsPath    = "/repos/" + testutil.InitRepository + "/contents/" + openbaoinit.InitFile
	tokensPath      = "/app/installations/" + testutil.InitInstallationID + "/access_tokens"
)

// call is one request either fake took, whose log holds all it carried but OpenBao's token header, which root records.
type call struct {
	server, method, path, body, log string
	root                            bool
}

// fakes play OpenBao's HTTP API and GitHub's, each over TLS, and record every request.
type fakes struct {
	t           *testing.T
	appKey      *rsa.PrivateKey
	mu          sync.Mutex
	calls       []call
	initialised bool
	enabled     map[string]bool
	files       map[string]string
	bao, github *httptest.Server
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakes{t: t, appKey: appKey, enabled: map[string]bool{}, files: map[string]string{}}
	f.bao = httptest.NewTLSServer(http.HandlerFunc(f.serveOpenBao))
	f.github = httptest.NewTLSServer(http.HandlerFunc(f.serveGitHub))
	t.Cleanup(func() { f.bao.Close(); f.github.Close() })
	return f
}

func (f *fakes) record(server string, r *http.Request) call {
	raw, _ := io.ReadAll(r.Body)
	headers := r.Header.Clone()
	headers.Del("X-Vault-Token")
	c := call{server: server, method: r.Method, path: r.URL.Path, body: string(raw), root: r.Header.Get("X-Vault-Token") == fakeRootToken,
		log: fmt.Sprintf("%s %s %s %v %s", server, r.Method, r.URL.RequestURI(), headers, raw)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return c
}

func reply(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// serveOpenBao lists under sys/mounts and sys/auth what a POST below either enabled, with the accessor for kubernetes/.
func (f *fakes) serveOpenBao(w http.ResponseWriter, r *http.Request) {
	c := f.record("openbao", r)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case c.path == "/v1/sys/init" && c.method == http.MethodGet:
		reply(w, http.StatusOK, fmt.Sprintf(`{"initialized":%t}`, f.initialised))
	case c.path == "/v1/sys/init":
		f.initialise(w, c)
	case !c.root:
		reply(w, http.StatusForbidden, `{"errors":["permission denied"]}`)
	case c.method == http.MethodGet && (c.path == "/v1/sys/mounts" || c.path == "/v1/sys/auth"):
		listed := map[string]map[string]string{"sys/": {}}
		for name, entry := range map[string]map[string]string{"kv": {}, "kubernetes": {"accessor": fakeAccessor}} {
			if f.enabled[c.path+"/"+name] {
				listed[name+"/"] = entry
			}
		}
		data, _ := json.Marshal(map[string]any{"data": listed})
		reply(w, http.StatusOK, string(data))
	case c.method == http.MethodPost:
		f.enabled[c.path] = true
		w.WriteHeader(http.StatusNoContent)
	default:
		reply(w, http.StatusNotFound, `{"errors":[]}`)
	}
}

// initialise initialises once, and encrypts the recovery key to the binary key the request names, as OpenBao does.
func (f *fakes) initialise(w http.ResponseWriter, c call) {
	var req struct {
		RecoveryPGPKeys []string `json:"recovery_pgp_keys"`
	}
	_ = json.Unmarshal([]byte(c.body), &req)
	key, _ := base64.StdEncoding.DecodeString(strings.Join(req.RecoveryPGPKeys, ""))
	ring, err := openpgp.ReadKeyRing(bytes.NewReader(key))
	if f.initialised || len(req.RecoveryPGPKeys) != 1 || err != nil {
		reply(w, http.StatusBadRequest, `{"errors":["bad init"]}`)
		return
	}
	var sealed bytes.Buffer
	plain, err := openpgp.Encrypt(&sealed, ring, nil, nil, nil)
	require.NoError(f.t, err)
	_, _ = io.WriteString(plain, fakeRecoveryKey)
	require.NoError(f.t, plain.Close())
	f.initialised = true
	reply(w, http.StatusOK, fmt.Sprintf(`{"root_token":%q,"recovery_keys_base64":[%q]}`, fakeRootToken, base64.StdEncoding.EncodeToString(sealed.Bytes())))
}

// serveGitHub mints a token for a JWT the App's key signed, and serves openbao-init.asc raw to a token it minted.
func (f *fakes) serveGitHub(w http.ResponseWriter, r *http.Request) {
	c := f.record("github", r)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	content, found := f.files[openbaoinit.InitFile]
	var body struct{ Message, Content string }
	_ = json.Unmarshal([]byte(c.body), &body)
	raw, err := base64.StdEncoding.DecodeString(body.Content)
	switch {
	case c.method == http.MethodPost && c.path == tokensPath:
		f.mintToken(w, auth, c.body)
	case c.path != contentsPath || (auth != readToken && auth != writeToken):
		reply(w, http.StatusNotFound, `{"message":"Not Found"}`)
	case c.method == http.MethodGet && r.Header.Get("Accept") != "application/vnd.github.raw+json":
		reply(w, http.StatusUnsupportedMediaType, `{"message":"want the raw media type"}`)
	case c.method == http.MethodGet && found:
		_, _ = io.WriteString(w, content)
	case c.method == http.MethodGet:
		reply(w, http.StatusNotFound, `{"message":"Not Found"}`)
	case c.method == http.MethodPut && auth == writeToken && !found && err == nil && body.Message != "":
		f.files[openbaoinit.InitFile] = string(raw)
		reply(w, http.StatusCreated, `{"content":{"name":"`+openbaoinit.InitFile+`"}}`)
	default:
		reply(w, http.StatusUnprocessableEntity, `{"message":"Invalid request"}`)
	}
}

// mintToken answers a token for contents:read or contents:write on the cluster repository alone, to a JWT GitHub takes:
// RS256 by the App's key, issued by the App, and alive at most ten minutes.
func (f *fakes) mintToken(w http.ResponseWriter, jwt, body string) {
	var req struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	_ = json.Unmarshal([]byte(body), &req)
	token := map[string]string{"read": readToken, "write": writeToken}[req.Permissions["contents"]]
	parts := append(strings.Split(jwt, "."), "", "")[:3]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	now := time.Now().Unix()
	switch {
	case err != nil || rsa.VerifyPKCS1v15(&f.appKey.PublicKey, crypto.SHA256, sum[:], sig) != nil || json.Unmarshal(raw, &claims) != nil ||
		claims.Iss != testutil.InitAppID || claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600:
		reply(w, http.StatusUnauthorized, `{"message":"A JSON web token could not be decoded"}`)
	case !slices.Equal(req.Repositories, []string{strings.Split(testutil.InitRepository, "/")[1]}) || len(req.Permissions) != 1 || token == "":
		reply(w, http.StatusUnprocessableEntity, `{"message":"Invalid request"}`)
	default:
		reply(w, http.StatusCreated, fmt.Sprintf(`{"token":%q}`, token))
	}
}

func (f *fakes) callsSoFar() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func certificatePEM(s *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// rendered returns the documents Manifests renders for the fixture inputs, and the Job's pod template.
func rendered(t *testing.T) ([]map[string]any, map[string]any) {
	t.Helper()
	out, err := openbaoinit.Manifests(testutil.InitInputs(t))
	require.NoError(t, err)
	docs := testutil.Documents(t, out)
	jobs := testutil.Kind(docs, "Job")
	require.Len(t, jobs, 1)
	return docs, testutil.Field(t, jobs[0], "spec", "template").(map[string]any)
}

func envOf(t *testing.T, container any) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, e := range testutil.Field(t, container, "env").([]any) {
		env[testutil.Field(t, e, "name").(string)] = fmt.Sprint(testutil.Field(t, e, "value"))
	}
	return env
}

// run runs the rendered script in the Job's environment, its files, recipient key and addresses swapped for the fakes'.
func (f *fakes) run(t *testing.T, recipient []byte) (string, error) {
	t.Helper()
	_, pod := rendered(t)
	container := testutil.Field(t, pod, "spec", "containers", "0")
	dir := t.TempDir()
	appKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.appKey)})
	env := envOf(t, container)
	for k, v := range map[string]string{
		"PATH": os.Getenv("PATH"), "HOME": dir, "GNUPGHOME": filepath.Join(dir, "gnupg"),
		"MITSTACK_OPENBAO_ADDR": f.bao.URL, "MITSTACK_GITHUB_API_URL": f.github.URL,
		"MITSTACK_OPENBAO_CA":     writeFile(t, dir, "openbao-ca.crt", certificatePEM(f.bao)),
		"CURL_CA_BUNDLE":          writeFile(t, dir, "github-ca.crt", certificatePEM(f.github)),
		"MITSTACK_RECIPIENT_FILE": writeFile(t, dir, "recipient.asc", recipient),
		"MITSTACK_APP_KEY_FILE":   writeFile(t, dir, "private-key", appKey),
		"MITSTACK_RETRY_SECONDS":  "0.05", "KUBERNETES_SERVICE_HOST": "10.96.0.1", "KUBERNETES_SERVICE_PORT": "443",
	} {
		env[k] = v
	}
	cmd := exec.Command("bash", "-c", testutil.Field(t, container, "command", "2").(string), "openbao-init")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	timer := time.AfterFunc(60*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	err := cmd.Wait()
	require.True(t, timer.Stop(), "the init script ran past a minute:\n%s", out.String())
	return out.String(), err
}

// decryptions returns what gpg decrypts each armored PGP message in file to, in order.
func decryptions(t *testing.T, owner *openpgp.Entity, file string) []string {
	t.Helper()
	home := t.TempDir()
	gpg := func(stdin string, args ...string) string {
		cmd := exec.Command("gpg", append([]string{"--batch", "--quiet", "--homedir", home}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		require.NoError(t, err, "gpg %v", args)
		return string(out)
	}
	gpg(string(testutil.ArmoredPrivateKey(t, owner)), "--import")
	var plain []string
	for _, m := range regexp.MustCompile(`(?s)-----BEGIN PGP MESSAGE-----\n.*?-----END PGP MESSAGE-----\n`).FindAllString(file, -1) {
		plain = append(plain, gpg(m, "--decrypt"))
	}
	return plain
}

func TestInit_CommitsTheKeysAsPGPMessagesBeforeAnyRequestCarriesTheRootTokenThenWritesTheReadPolicyOnTheAccessorItRead(t *testing.T) {
	owner, recipient := testutil.Recipient(t)
	f := newFakes(t)

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	calls := f.callsSoFar()
	inits := slices.DeleteFunc(slices.Clone(calls), func(c call) bool { return c.path != "/v1/sys/init" || c.method == http.MethodGet })
	require.Len(t, inits, 1, "the script sent %d init requests, want one", len(inits))
	assert.Equal(t, http.MethodPut, inits[0].method)
	var initBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(inits[0].body), &initBody))
	assert.Equal(t, map[string]any{"recovery_shares": 1.0, "recovery_threshold": 1.0, "recovery_pgp_keys": initBody["recovery_pgp_keys"]}, initBody,
		"the init must ask for one recovery share of threshold one and a plain root token")
	committed := f.files[openbaoinit.InitFile]
	assert.Regexp(t, `^OpenBao root token:\n-----BEGIN PGP MESSAGE-----\n(?s:.*)\n-----END PGP MESSAGE-----\nOpenBao recovery key:\n-----BEGIN PGP MESSAGE-----\n(?s:.*)\n-----END PGP MESSAGE-----\n$`, committed)
	assert.Equal(t, []string{fakeRootToken, fakeRecoveryKey}, decryptions(t, owner, committed), "gpg must read the root token, then the recovery key")
	commit := slices.IndexFunc(calls, func(c call) bool { return c.method == http.MethodPut && c.path == contentsPath })
	firstRoot := slices.IndexFunc(calls, func(c call) bool { return c.root })
	require.GreaterOrEqual(t, firstRoot, 0, "no request carried the root token, so nothing was set up")
	assert.Less(t, commit, firstRoot, "%s %s carried the root token before %s was committed", calls[firstRoot].method, calls[firstRoot].path, openbaoinit.InitFile)
	var setUp []string
	for _, c := range calls {
		assert.False(t, c.root && c.server == "github", "GitHub received the root token at %s %s", c.method, c.path)
		assert.NotContains(t, c.log, fakeRootToken, "a fake logged the root token")
		assert.NotContains(t, c.log, fakeRecoveryKey, "a fake logged the recovery key")
		if c.root {
			setUp = append(setUp, c.method+" "+c.path+" "+c.body)
		}
	}
	assert.NotContains(t, out, fakeRootToken, "the script printed the root token")
	assert.NotContains(t, out, fakeRecoveryKey, "the script printed the recovery key")
	namespace := "{{identity.entity.aliases." + fakeAccessor + ".metadata.service_account_namespace}}"
	policy, err := json.Marshal(map[string]string{"policy": "path \"kv/data/apps/" + namespace + "/*\" {\n  capabilities = [\"read\"]\n}\n" +
		"path \"kv/metadata/apps/" + namespace + "/*\" {\n  capabilities = [\"read\", \"list\"]\n}\n"})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"GET /v1/sys/mounts ",
		`POST /v1/sys/mounts/kv {"type":"kv","options":{"version":"2"}}`,
		"GET /v1/sys/auth ",
		`POST /v1/sys/auth/kubernetes {"type":"kubernetes"}`,
		`POST /v1/auth/kubernetes/config {"kubernetes_host":"https://10.96.0.1:443"}`,
		"GET /v1/sys/auth ",
		"POST /v1/sys/policies/acl/app-secrets-reader " + string(policy),
	}, setUp, "the setup sent these requests with the root token")
}

func TestInit_OnAStoreInitialisedWithItsInitFileCommittedWritesNothing(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	f := newFakes(t)
	f.initialised = true
	f.files[openbaoinit.InitFile] = "OpenBao root token:\nrecorded\n"

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	assert.Contains(t, out, "holds "+openbaoinit.InitFile+", and this Job no longer holds the root token, so it changes nothing")
	for _, c := range f.callsSoFar() {
		if c.method == http.MethodPost && c.path == tokensPath {
			assert.JSONEq(t, `{"repositories":["shop-cluster"],"permissions":{"contents":"read"}}`, c.body, "the check asked for a token that may write")
		} else {
			assert.Equal(t, http.MethodGet, c.method, "the script wrote %s %s to %s", c.method, c.path, c.server)
		}
	}
	assert.Equal(t, "OpenBao root token:\nrecorded\n", f.files[openbaoinit.InitFile])
}

func TestInit_StopsBeforeInitialisingWhenItCannotHandTheKeysBack(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	cases := []struct {
		name                       string
		recipient                  []byte
		initialised, file, noCalls bool
		want                       string
	}{
		{"a recipient key that cannot encrypt", testutil.SignOnlyRecipient(t), false, false, true,
			"cannot encrypt to the recipient key, so OpenBao was left uninitialised"},
		{"a store initialised without the init file", recipient, true, false, false,
			"OpenBao is initialised, and the cluster repository holds no " + openbaoinit.InitFile + ", so its root token and recovery key are lost"},
		{"an uninitialised store whose repository already holds an init file", recipient, false, true, false,
			"OpenBao is uninitialised, and the cluster repository already holds " + openbaoinit.InitFile + ", so OpenBao was left uninitialised"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakes(t)
			f.initialised = tc.initialised
			if tc.file {
				f.files[openbaoinit.InitFile] = "OpenBao root token:\nanother store's\n"
			}

			out, err := f.run(t, tc.recipient)

			require.Error(t, err, "the script exited 0 on %s:\n%s", tc.name, out)
			assert.Contains(t, out, tc.want)
			for _, c := range f.callsSoFar() {
				assert.False(t, c.server == "openbao" && c.method != http.MethodGet, "the script sent %s %s to OpenBao", c.method, c.path)
				assert.NotEqual(t, http.MethodPut, c.method, "the script sent %s %s to %s", c.method, c.path, c.server)
			}
			assert.False(t, tc.noCalls && len(f.callsSoFar()) > 0, "the script reached a fake before it checked the recipient key")
		})
	}
}

func TestManifests_RunsTheScriptAsAJobInTheAppKeysNamespaceThatMountsTheKeyTheRecipientAndOpenBaosAuthority(t *testing.T) {
	docs, pod := rendered(t)

	container := testutil.Field(t, pod, "spec", "containers", "0").(map[string]any)
	env := envOf(t, container)

	assert.Equal(t, openbaoinit.AppKeyNamespace, testutil.Field(t, testutil.Kind(docs, "Job")[0], "metadata", "namespace"))
	assert.NotEmpty(t, testutil.Field(t, pod, "metadata", "labels", "mitstack.dev/app"), "the admission policy refuses a pod without mitstack.dev/app")
	assert.Equal(t, testutil.ToolsImage, container["image"])
	assert.Equal(t, []string{testutil.InitRepository, testutil.InitAppID, testutil.InitInstallationID, "https://api.github.com"},
		[]string{env["MITSTACK_REPOSITORY"], env["MITSTACK_GITHUB_APP_ID"], env["MITSTACK_GITHUB_INSTALLATION_ID"], env["MITSTACK_GITHUB_API_URL"]})
	sources, mounted := map[string]string{}, map[string]string{}
	for _, v := range testutil.Field(t, pod, "spec", "volumes").([]any) {
		for kind, field := range map[string]string{"secret": "secretName", "configMap": "name"} {
			if source, ok := v.(map[string]any)[kind]; ok {
				sources[testutil.Field(t, v, "name").(string)] = kind + " " + testutil.Field(t, source, field).(string)
			}
		}
	}
	for _, m := range container["volumeMounts"].([]any) {
		mounted[testutil.Field(t, m, "mountPath").(string)] = sources[testutil.Field(t, m, "name").(string)]
	}
	for file, want := range map[string]string{
		"MITSTACK_APP_KEY_FILE":   "secret " + openbaoinit.AppKeySecret + "/" + openbaoinit.AppKeyItem,
		"MITSTACK_RECIPIENT_FILE": "configMap mitstack-openbao-init/recipient.asc",
		"MITSTACK_OPENBAO_CA":     "configMap openbao-ca/ca.crt",
	} {
		assert.Equal(t, want, mounted[filepath.Dir(env[file])]+"/"+filepath.Base(env[file]), "%s=%s", file, env[file])
	}
	assert.Equal(t, string(testutil.InitInputs(t).Recipient), testutil.Field(t, testutil.Kind(docs, "ConfigMap")[0], "data", "recipient.asc"))
}

func TestManifests_RefusesInputsTheJobCannotRunWith(t *testing.T) {
	owner, _ := testutil.Recipient(t)
	cases := map[string]struct {
		change func(*openbaoinit.Inputs)
		want   string
	}{
		"a tools image named by tag alone":     {func(in *openbaoinit.Inputs) { in.ToolsImage = "ghcr.io/x/tools:v1" }, "tools image"},
		"a repository with no owner":           {func(in *openbaoinit.Inputs) { in.Repository = "shop-cluster" }, `repository "shop-cluster" is not owner/name`},
		"an App id that is not a number":       {func(in *openbaoinit.Inputs) { in.AppID = "Iv1.abc" }, `GitHub App id "Iv1.abc" is not a number`},
		"an installation id that is empty":     {func(in *openbaoinit.Inputs) { in.InstallationID = "" }, `installation id "" is not a number`},
		"a recipient that is not a key":        {func(in *openbaoinit.Inputs) { in.Recipient = []byte("not a key") }, "the recipient is not an armored OpenPGP public key"},
		"a recipient that cannot encrypt":      {func(in *openbaoinit.Inputs) { in.Recipient = testutil.SignOnlyRecipient(t) }, "has no key that can encrypt"},
		"a recipient given as its private key": {func(in *openbaoinit.Inputs) { in.Recipient = testutil.ArmoredPrivateKey(t, owner) }, "is not an armored OpenPGP public key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := testutil.InitInputs(t)
			tc.change(&in)

			_, err := openbaoinit.Manifests(in)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
