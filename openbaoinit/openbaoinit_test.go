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
	"github.com/ProtonMail/go-crypto/openpgp/armor"
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

// call is one request either fake took, in the order the script sent them. Its log line holds everything the
// request carried but OpenBao's token header, which root records instead.
type call struct {
	server, method, path, body string
	log                        string
	root                       bool
}

// fakes play OpenBao's HTTP API and GitHub's, each over TLS, and record every request.
type fakes struct {
	t           *testing.T
	appKey      *rsa.PrivateKey
	mu          sync.Mutex
	calls       []call
	initialised bool
	kv, auth    bool
	files       map[string]string
	bao, github *httptest.Server
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakes{t: t, appKey: appKey, files: map[string]string{}}
	f.bao = httptest.NewTLSServer(http.HandlerFunc(f.serveOpenBao))
	f.github = httptest.NewTLSServer(http.HandlerFunc(f.serveGitHub))
	t.Cleanup(func() { f.bao.Close(); f.github.Close() })
	return f
}

func (f *fakes) record(server string, r *http.Request) call {
	raw, _ := io.ReadAll(r.Body)
	headers := r.Header.Clone()
	root := headers.Get("X-Vault-Token") == fakeRootToken
	headers.Del("X-Vault-Token")
	c := call{server: server, method: r.Method, path: r.URL.Path, body: string(raw), root: root,
		log: fmt.Sprintf("%s %s %s %v %s", server, r.Method, r.URL.RequestURI(), headers, raw)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return c
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakes) serveOpenBao(w http.ResponseWriter, r *http.Request) {
	c := f.record("openbao", r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.path == "/v1/sys/init" {
		f.serveInit(w, c)
		return
	}
	if !c.root {
		reply(w, http.StatusForbidden, `{"errors":["permission denied"]}`)
		return
	}
	switch c.method + " " + c.path {
	case "GET /v1/sys/mounts":
		mounts := `"sys/":{"type":"system"}`
		if f.kv {
			mounts += `,"kv/":{"type":"kv"}`
		}
		reply(w, http.StatusOK, `{"data":{`+mounts+`}}`)
	case "POST /v1/sys/mounts/kv":
		f.kv = true
		w.WriteHeader(http.StatusNoContent)
	case "GET /v1/sys/auth":
		auths := `"token/":{"accessor":"auth_token_0a1b2c3d","type":"token"}`
		if f.auth {
			auths += `,"kubernetes/":{"accessor":"` + fakeAccessor + `","type":"kubernetes"}`
		}
		reply(w, http.StatusOK, `{"data":{`+auths+`}}`)
	case "POST /v1/sys/auth/kubernetes":
		f.auth = true
		w.WriteHeader(http.StatusNoContent)
	case "POST /v1/auth/kubernetes/config", "POST /v1/sys/policies/acl/app-secrets-reader":
		w.WriteHeader(http.StatusNoContent)
	default:
		reply(w, http.StatusNotFound, `{"errors":[]}`)
	}
}

// serveInit initialises once, and encrypts the recovery key to the binary key the request names, as OpenBao does.
func (f *fakes) serveInit(w http.ResponseWriter, c call) {
	if c.method == http.MethodGet {
		reply(w, http.StatusOK, fmt.Sprintf(`{"initialized":%t}`, f.initialised))
		return
	}
	var req struct {
		RecoveryPGPKeys []string `json:"recovery_pgp_keys"`
	}
	if f.initialised || json.Unmarshal([]byte(c.body), &req) != nil || len(req.RecoveryPGPKeys) != 1 {
		reply(w, http.StatusBadRequest, `{"errors":["bad init"]}`)
		return
	}
	key, err := base64.StdEncoding.DecodeString(req.RecoveryPGPKeys[0])
	ring, readErr := openpgp.ReadKeyRing(bytes.NewReader(key))
	if err != nil || readErr != nil {
		reply(w, http.StatusBadRequest, `{"errors":["the recovery PGP key is not a base64 binary key"]}`)
		return
	}
	var sealed bytes.Buffer
	plain, err := openpgp.Encrypt(&sealed, ring, nil, nil, nil)
	require.NoError(f.t, err)
	_, _ = io.WriteString(plain, fakeRecoveryKey)
	require.NoError(f.t, plain.Close())
	f.initialised = true
	reply(w, http.StatusOK, fmt.Sprintf(`{"keys":[],"keys_base64":[],"root_token":%q,"recovery_keys":["00"],"recovery_keys_base64":[%q]}`,
		fakeRootToken, base64.StdEncoding.EncodeToString(sealed.Bytes())))
}

// serveGitHub mints an installation token only for a JWT the App's key signed, and serves the contents API to it.
func (f *fakes) serveGitHub(w http.ResponseWriter, r *http.Request) {
	c := f.record("github", r)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.method == http.MethodPost && c.path == tokensPath {
		f.mintToken(w, auth, c.body)
		return
	}
	if c.path != contentsPath || (auth != readToken && auth != writeToken) {
		reply(w, http.StatusNotFound, `{"message":"Not Found"}`)
		return
	}
	content, found := f.files[openbaoinit.InitFile]
	switch {
	case c.method == http.MethodGet && found && r.Header.Get("Accept") == "application/vnd.github.raw+json":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	case c.method == http.MethodGet && found:
		reply(w, http.StatusOK, fmt.Sprintf(`{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(content))))
	case c.method == http.MethodGet:
		reply(w, http.StatusNotFound, `{"message":"Not Found"}`)
	case c.method == http.MethodPut && auth == writeToken && !found:
		var body struct{ Message, Content string }
		raw, err := base64.StdEncoding.DecodeString(func() string { _ = json.Unmarshal([]byte(c.body), &body); return body.Content }())
		if err != nil || body.Message == "" {
			reply(w, http.StatusUnprocessableEntity, `{"message":"Invalid request"}`)
			return
		}
		f.files[openbaoinit.InitFile] = string(raw)
		reply(w, http.StatusCreated, `{"content":{"name":"`+openbaoinit.InitFile+`"}}`)
	default:
		reply(w, http.StatusUnprocessableEntity, `{"message":"Invalid request"}`)
	}
}

func (f *fakes) mintToken(w http.ResponseWriter, jwt, body string) {
	if !f.signedByTheApp(jwt) {
		reply(w, http.StatusUnauthorized, `{"message":"A JSON web token could not be decoded"}`)
		return
	}
	var req struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	name := strings.Split(testutil.InitRepository, "/")[1]
	token := map[string]string{"read": readToken, "write": writeToken}[req.Permissions["contents"]]
	if !slices.Equal(req.Repositories, []string{name}) || len(req.Permissions) != 1 || token == "" {
		reply(w, http.StatusUnprocessableEntity, `{"message":"Invalid request"}`)
		return
	}
	reply(w, http.StatusCreated, fmt.Sprintf(`{"token":%q,"expires_at":"2026-10-22T22:00:00Z"}`, token))
}

// signedByTheApp checks the JWT as GitHub does: RS256 by the App's key, issued by the App, and alive at most ten minutes.
func (f *fakes) signedByTheApp(jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(&f.appKey.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	now := time.Now().Unix()
	return json.Unmarshal(raw, &claims) == nil && claims.Iss == testutil.InitAppID &&
		claims.Iat <= now && claims.Exp > now && claims.Exp-claims.Iat <= 600
}

func (f *fakes) callsSoFar() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakes) indexOf(server, method, path string) int {
	return slices.IndexFunc(f.callsSoFar(), func(c call) bool { return c.server == server && c.method == method && c.path == path })
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

// rendered returns the init Job's container as the manifest renders it for the fixture inputs.
func rendered(t *testing.T) map[string]any {
	t.Helper()
	out, err := openbaoinit.Manifests(testutil.InitInputs(t))
	require.NoError(t, err)
	jobs := testutil.Kind(testutil.Documents(t, out), "Job")
	require.Len(t, jobs, 1)
	return testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0").(map[string]any)
}

// run runs the rendered script with the Job's own environment, its files and addresses swapped for the fakes', and
// recipient as the mounted recipient key.
func (f *fakes) run(t *testing.T, recipient []byte) (string, error) {
	t.Helper()
	container := rendered(t)
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, e := range testutil.Field(t, container, "env").([]any) {
		env = append(env, fmt.Sprintf("%v=%v", testutil.Field(t, e, "name"), testutil.Field(t, e, "value")))
	}
	dir := t.TempDir()
	appKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.appKey)})
	env = append(env,
		"HOME="+dir, "GNUPGHOME="+filepath.Join(dir, "gnupg"),
		"MITSTACK_OPENBAO_ADDR="+f.bao.URL, "MITSTACK_GITHUB_API_URL="+f.github.URL,
		"MITSTACK_OPENBAO_CA="+writeFile(t, dir, "openbao-ca.crt", certificatePEM(f.bao)),
		"CURL_CA_BUNDLE="+writeFile(t, dir, "github-ca.crt", certificatePEM(f.github)),
		"MITSTACK_RECIPIENT_FILE="+writeFile(t, dir, "recipient.asc", recipient),
		"MITSTACK_APP_KEY_FILE="+writeFile(t, dir, "private-key", appKey),
		"MITSTACK_RETRY_SECONDS=0.05", "KUBERNETES_SERVICE_HOST=10.96.0.1", "KUBERNETES_SERVICE_PORT=443")
	script := testutil.Field(t, container, "command", "2").(string)
	cmd := exec.Command("bash", "-c", script, "openbao-init")
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		require.FailNow(t, "the init script ran past a minute", out.String())
		return "", nil
	}
}

// armoredPrivateKey returns entity's ASCII-armored private key, as `gpg --armor --export-secret-keys` writes one.
func armoredPrivateKey(t *testing.T, entity *openpgp.Entity) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := armor.Encode(&out, openpgp.PrivateKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.SerializePrivate(w, nil))
	require.NoError(t, w.Close())
	return out.Bytes()
}

// decryptions returns what each armored PGP message in file decrypts to, with go-crypto and with gpg, in order.
func decryptions(t *testing.T, owner *openpgp.Entity, file string) [][2]string {
	t.Helper()
	messages := regexp.MustCompile(`(?s)-----BEGIN PGP MESSAGE-----\n.*?-----END PGP MESSAGE-----\n`).FindAllString(file, -1)
	home := t.TempDir()
	gpg := func(stdin string, args ...string) string {
		cmd := exec.Command("gpg", append([]string{"--batch", "--homedir", home}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		require.NoError(t, err, "gpg %v", args)
		return string(out)
	}
	gpg(string(armoredPrivateKey(t, owner)), "--import")
	var plain [][2]string
	for _, m := range messages {
		block, err := armor.Decode(strings.NewReader(m))
		require.NoError(t, err)
		md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{owner}, nil, nil)
		require.NoError(t, err)
		body, err := io.ReadAll(md.UnverifiedBody)
		require.NoError(t, err)
		plain = append(plain, [2]string{string(body), gpg(m, "--decrypt")})
	}
	return plain
}

func TestInit_CommitsTheRootTokenAndRecoveryKeyOnlyAsPGPMessagesBeforeAnyRequestCarriesTheRootToken(t *testing.T) {
	owner, recipient := testutil.Recipient(t)
	f := newFakes(t)

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	calls := f.callsSoFar()
	var inits []call
	for _, c := range calls {
		if c.server == "openbao" && c.path == "/v1/sys/init" && c.method != http.MethodGet {
			inits = append(inits, c)
		}
	}
	require.Len(t, inits, 1, "the script sent %d init requests, want one", len(inits))
	assert.Equal(t, http.MethodPut, inits[0].method)
	var initBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(inits[0].body), &initBody))
	binary, err := base64.StdEncoding.DecodeString(fmt.Sprint(testutil.Field(t, initBody, "recovery_pgp_keys", "0")))
	require.NoError(t, err)
	sent, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	require.NoError(t, err)
	assert.Equal(t, owner.PrimaryKey.KeyId, sent[0].PrimaryKey.KeyId, "the recovery PGP key is not the recipient's")
	assert.Equal(t, map[string]any{"recovery_shares": 1.0, "recovery_threshold": 1.0, "recovery_pgp_keys": initBody["recovery_pgp_keys"]}, initBody,
		"the init asked for %v, want one recovery share of threshold one and a plain root token", initBody)

	committed, ok := f.files[openbaoinit.InitFile]
	require.True(t, ok, "the cluster repository holds no %s:\n%s", openbaoinit.InitFile, out)
	assert.Regexp(t, `^OpenBao root token:\n-----BEGIN PGP MESSAGE-----\n(?s:.*)\n-----END PGP MESSAGE-----\nOpenBao recovery key:\n-----BEGIN PGP MESSAGE-----\n(?s:.*)\n-----END PGP MESSAGE-----\n$`, committed)
	assert.Equal(t, [][2]string{{fakeRootToken, fakeRootToken}, {fakeRecoveryKey, fakeRecoveryKey}}, decryptions(t, owner, committed),
		"go-crypto and gpg must each read the root token, then the recovery key")
	commit := f.indexOf("github", http.MethodPut, contentsPath)
	firstRoot := slices.IndexFunc(calls, func(c call) bool { return c.root })
	require.GreaterOrEqual(t, firstRoot, 0, "no request carried the root token, so nothing was set up")
	assert.Less(t, commit, firstRoot, "%s %s carried the root token before %s was committed", calls[firstRoot].method, calls[firstRoot].path, openbaoinit.InitFile)
	for _, c := range calls {
		assert.False(t, c.root && c.server == "github", "GitHub received the root token at %s %s", c.method, c.path)
		for _, secret := range []string{fakeRootToken, fakeRecoveryKey} {
			assert.NotContains(t, c.log, secret, "a fake logged a plain secret")
		}
	}
	assert.NotContains(t, out, fakeRootToken, "the script printed the root token")
	assert.NotContains(t, out, fakeRecoveryKey, "the script printed the recovery key")
}

func TestInit_EnablesKVAndKubernetesAuthThenWritesTheReadPolicyOnTheAccessorItRead(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	f := newFakes(t)

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	var setUp []string
	for _, c := range f.callsSoFar() {
		if c.root {
			setUp = append(setUp, c.method+" "+c.path+" "+c.body)
		}
	}
	namespace := "{{identity.entity.aliases." + fakeAccessor + ".metadata.service_account_namespace}}"
	policy := "path \"kv/data/apps/" + namespace + "/*\" {\n  capabilities = [\"read\"]\n}\n" +
		"path \"kv/metadata/apps/" + namespace + "/*\" {\n  capabilities = [\"read\", \"list\"]\n}\n"
	wantPolicy, err := json.Marshal(map[string]string{"policy": policy})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"GET /v1/sys/mounts ",
		`POST /v1/sys/mounts/kv {"type":"kv","options":{"version":"2"}}`,
		"GET /v1/sys/auth ",
		`POST /v1/sys/auth/kubernetes {"type":"kubernetes"}`,
		`POST /v1/auth/kubernetes/config {"kubernetes_host":"https://10.96.0.1:443"}`,
		"GET /v1/sys/auth ",
		"POST /v1/sys/policies/acl/app-secrets-reader " + string(wantPolicy),
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
			continue
		}
		assert.Equal(t, http.MethodGet, c.method, "the script wrote %s %s to %s", c.method, c.path, c.server)
	}
	assert.Equal(t, "OpenBao root token:\nrecorded\n", f.files[openbaoinit.InitFile])
}

func TestInit_StopsBeforeInitialisingWhenItCannotHandTheKeysBack(t *testing.T) {
	owner, recipient := testutil.Recipient(t)
	signOnly := *owner
	signOnly.Subkeys = nil
	cases := []struct {
		name               string
		recipient          []byte
		initialised, file  bool
		want               string
		wantNoRequestAtAll bool
	}{
		{"a recipient key that cannot encrypt", testutil.ArmoredPublicKey(t, &signOnly), false, false,
			"cannot encrypt to the recipient key, so OpenBao was left uninitialised", true},
		{"a store initialised without the init file", recipient, true, false,
			"OpenBao is initialised, and the cluster repository holds no " + openbaoinit.InitFile + ", so its root token and recovery key are lost", false},
		{"an uninitialised store whose repository already holds an init file", recipient, false, true,
			"OpenBao is uninitialised, and the cluster repository already holds " + openbaoinit.InitFile + ", so OpenBao was left uninitialised", false},
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
				assert.False(t, c.method == http.MethodPut, "the script sent %s %s to %s", c.method, c.path, c.server)
			}
			if tc.wantNoRequestAtAll {
				assert.Empty(t, f.callsSoFar(), "the script reached a fake before it checked the recipient key")
			}
		})
	}
}

func TestManifests_RunsTheScriptAsAJobInTheAppKeysNamespaceThatMountsTheKeyTheRecipientAndOpenBaosAuthority(t *testing.T) {
	in := testutil.InitInputs(t)

	out, err := openbaoinit.Manifests(in)

	require.NoError(t, err)
	docs := testutil.Documents(t, out)
	job := testutil.Kind(docs, "Job")[0]
	assert.Equal(t, openbaoinit.AppKeyNamespace, testutil.Field(t, job, "metadata", "namespace"))
	pod := testutil.Field(t, job, "spec", "template").(map[string]any)
	assert.NotEmpty(t, testutil.Field(t, pod, "metadata", "labels", "mitstack.dev/app"), "the admission policy refuses a pod without mitstack.dev/app")
	container := testutil.Field(t, pod, "spec", "containers", "0").(map[string]any)
	assert.Equal(t, testutil.ToolsImage, container["image"])
	env := map[string]string{}
	for _, e := range container["env"].([]any) {
		env[testutil.Field(t, e, "name").(string)] = fmt.Sprint(testutil.Field(t, e, "value"))
	}
	assert.Equal(t, testutil.InitRepository, env["MITSTACK_REPOSITORY"])
	assert.Equal(t, testutil.InitAppID, env["MITSTACK_GITHUB_APP_ID"])
	assert.Equal(t, testutil.InitInstallationID, env["MITSTACK_GITHUB_INSTALLATION_ID"])
	assert.Equal(t, "https://api.github.com", env["MITSTACK_GITHUB_API_URL"])
	mounts := map[string]string{}
	for _, m := range container["volumeMounts"].([]any) {
		mounts[testutil.Field(t, m, "name").(string)] = testutil.Field(t, m, "mountPath").(string)
	}
	sources := map[string]string{}
	for _, v := range testutil.Field(t, pod, "spec", "volumes").([]any) {
		volume := v.(map[string]any)
		if s, ok := volume["secret"]; ok {
			sources[volume["name"].(string)] = "Secret " + testutil.Field(t, s, "secretName").(string)
		}
		if c, ok := volume["configMap"]; ok {
			sources[volume["name"].(string)] = "ConfigMap " + testutil.Field(t, c, "name").(string)
		}
	}
	for file, want := range map[string]string{
		"MITSTACK_APP_KEY_FILE":   "Secret " + openbaoinit.AppKeySecret + "/" + openbaoinit.AppKeyItem,
		"MITSTACK_RECIPIENT_FILE": "ConfigMap mitstack-openbao-init/recipient.asc",
		"MITSTACK_OPENBAO_CA":     "ConfigMap openbao-ca/ca.crt",
	} {
		dir, key := filepath.Split(env[file])
		var got string
		for volume, path := range mounts {
			if filepath.Clean(dir) == path {
				got = sources[volume] + "/" + key
			}
		}
		assert.Equal(t, want, got, "%s=%s reads no file of %s", file, env[file], want)
	}
	configMap := testutil.Kind(docs, "ConfigMap")[0]
	assert.Equal(t, string(in.Recipient), testutil.Field(t, configMap, "data", "recipient.asc"))
}

func TestManifests_RefusesInputsTheJobCannotRunWith(t *testing.T) {
	owner, _ := testutil.Recipient(t)
	signOnly := *owner
	signOnly.Subkeys = nil
	cases := map[string]struct {
		change func(*openbaoinit.Inputs)
		want   string
	}{
		"a tools image named by tag alone": {func(in *openbaoinit.Inputs) { in.ToolsImage = "ghcr.io/mitstack-eu/talos-tools:v0.1.0" }, "tools image"},
		"a repository with no owner":       {func(in *openbaoinit.Inputs) { in.Repository = "shop-cluster" }, `repository "shop-cluster" is not owner/name`},
		"an App id that is not a number":   {func(in *openbaoinit.Inputs) { in.AppID = "Iv1.abc" }, `GitHub App id "Iv1.abc" is not a number`},
		"an installation id that is empty": {func(in *openbaoinit.Inputs) { in.InstallationID = "" }, `installation id "" is not a number`},
		"a recipient that is not a key":    {func(in *openbaoinit.Inputs) { in.Recipient = []byte("not a key") }, "the recipient is not an armored OpenPGP public key"},
		"a recipient that cannot encrypt": {func(in *openbaoinit.Inputs) { in.Recipient = testutil.ArmoredPublicKey(t, &signOnly) },
			"the recipient key has no key that can encrypt"},
		"a recipient given as its private key": {func(in *openbaoinit.Inputs) { in.Recipient = armoredPrivateKey(t, owner) },
			"the recipient is not an armored OpenPGP public key"},
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
