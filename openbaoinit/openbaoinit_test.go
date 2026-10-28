package openbaoinit_test

// The init script runs with bash against a fake OpenBao over TLS and a bare Git repository that a fake ssh reaches,
// so each test reads what it committed and which OpenBao calls carried the root token.

import (
	"bytes"
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
	defaultBranch   = "main"
)

// call is one request OpenBao received, with whether the repository held the init file when it arrived.
type call struct {
	method, path, body, log string
	root, committed         bool
}

type fakes struct {
	t            *testing.T
	mu           sync.Mutex
	calls        []call
	initialised  bool
	recoveryKeys *string
	enabled      map[string]bool
	bao          *httptest.Server
	// repo is the bare repository the fake ssh serves as the cluster repository.
	repo string
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	f := &fakes{t: t, enabled: map[string]bool{}, repo: filepath.Join(t.TempDir(), "shop-cluster.git")}
	git(t, "", "init", "--quiet", "--bare", "--initial-branch="+defaultBranch, f.repo)
	f.commit("README.md", "# shop-cluster\n")
	f.bao = httptest.NewTLSServer(http.HandlerFunc(f.serveOpenBao))
	t.Cleanup(f.bao.Close)
	return f
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// commit puts a file on the repository's default branch as another writer would.
func (f *fakes) commit(name, content string) {
	f.t.Helper()
	work := f.t.TempDir()
	git(f.t, work, "clone", "--quiet", f.repo, ".")
	require.NoError(f.t, os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755))
	require.NoError(f.t, os.WriteFile(filepath.Join(work, name), []byte(content), 0o644))
	git(f.t, work, "add", name)
	git(f.t, work, "commit", "--quiet", "-m", "Add "+name)
	git(f.t, work, "push", "--quiet", "origin", "HEAD:"+defaultBranch)
}

// file is the init file on the default branch, and whether there is one.
func (f *fakes) file() (string, bool) {
	out, err := exec.Command("git", "--git-dir", f.repo, "show", defaultBranch+":"+openbaoinit.InitFile).Output()
	return string(out), err == nil
}

func (f *fakes) head() string {
	return git(f.t, "", "--git-dir", f.repo, "rev-parse", defaultBranch)
}

func (f *fakes) record(r *http.Request) call {
	raw, _ := io.ReadAll(r.Body)
	headers := r.Header.Clone()
	headers.Del("X-Vault-Token")
	_, committed := f.file()
	c := call{method: r.Method, path: r.URL.Path, body: string(raw), root: r.Header.Get("X-Vault-Token") == fakeRootToken, committed: committed,
		log: fmt.Sprintf("%s %s %v %s", r.Method, r.URL.RequestURI(), headers, raw)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return c
}

func reply(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakes) serveOpenBao(w http.ResponseWriter, r *http.Request) {
	c := f.record(r)
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
	recovery := fmt.Sprintf(`,"recovery_keys_base64":[%q]`, base64.StdEncoding.EncodeToString(sealed.Bytes()))
	if f.recoveryKeys != nil {
		recovery = *f.recoveryKeys
	}
	reply(w, http.StatusOK, fmt.Sprintf(`{"root_token":%q%s}`, fakeRootToken, recovery))
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

func rendered(t *testing.T) ([]map[string]any, map[string]any) {
	t.Helper()
	out, err := openbaoinit.Manifests(testutil.InitInputs(t))
	require.NoError(t, err)
	docs := testutil.Documents(t, out)
	jobs := testutil.Kind(docs, "Job")
	require.Len(t, jobs, 1)
	return docs, testutil.Field(t, jobs[0], "spec", "template").(map[string]any)
}

// fakeSSH runs the Git service it is asked for against the bare repository, as GitHub's SSH endpoint does for a deploy
// key it accepts.
const fakeSSH = `service=${*: -1}
exec ${service%% *} "$FAKE_REPO"`

// run runs the Job's own script with the Job's own environment, pointed at the fakes.
func (f *fakes) run(t *testing.T, recipient []byte) (string, error) {
	t.Helper()
	_, pod := rendered(t)
	container := testutil.Field(t, pod, "spec", "containers", "0")
	tools := testutil.NewTools(t, map[string]string{"ssh": fakeSSH})
	dir := t.TempDir()
	env := testutil.ContainerEnv(t, container)
	for k, v := range map[string]string{
		"PATH": tools.Dir + ":" + os.Getenv("PATH"), "HOME": tools.Dir, "GNUPGHOME": filepath.Join(dir, "gnupg"), "FAKE_REPO": f.repo,
		"MITSTACK_OPENBAO_ADDR":     f.bao.URL,
		"MITSTACK_OPENBAO_CA":       writeFile(t, dir, "openbao-ca.crt", certificatePEM(f.bao)),
		"MITSTACK_RECIPIENT_FILE":   writeFile(t, dir, "recipient.asc", recipient),
		"MITSTACK_DEPLOY_KEY_FILE":  writeFile(t, dir, "ssh-privatekey", []byte(testutil.WriteKey(t))),
		"MITSTACK_KNOWN_HOSTS_FILE": writeFile(t, dir, "known_hosts", []byte(testutil.GitHubKnownHosts)),
		"MITSTACK_RETRY_SECONDS":    "0.05", "KUBERNETES_SERVICE_HOST": "10.96.0.1", "KUBERNETES_SERVICE_PORT": "443",
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
	return out.String() + strings.Join(tools.Calls(t), "\n"), err
}

func TestInit_CommitsTheKeysAsPGPMessagesBeforeAnyRequestCarriesTheRootTokenThenWritesTheReadPolicyOnTheAccessorItRead(t *testing.T) {
	_, recipient := testutil.Recipient(t)
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
	committed, ok := f.file()
	require.True(t, ok, "the script never committed %s:\n%s", openbaoinit.InitFile, out)
	assert.Regexp(t, `^OpenBao root token:\n-----BEGIN PGP MESSAGE-----\n(?s:.*)\n-----END PGP MESSAGE-----\nOpenBao recovery key:\n-----BEGIN PGP MESSAGE-----\n(?s:.*)\n-----END PGP MESSAGE-----\n$`, committed)
	assert.Equal(t, []string{fakeRootToken, fakeRecoveryKey}, testutil.RecipientDecryptions(t, committed), "gpg must read the root token, then the recovery key")
	var setUp []string
	for _, c := range calls {
		assert.False(t, c.root && !c.committed, "%s %s carried the root token before %s was committed", c.method, c.path, openbaoinit.InitFile)
		assert.NotContains(t, c.log, fakeRootToken, "a fake logged the root token")
		assert.NotContains(t, c.log, fakeRecoveryKey, "a fake logged the recovery key")
		if c.root {
			setUp = append(setUp, c.method+" "+c.path+" "+c.body)
		}
	}
	assert.NotContains(t, out, fakeRootToken, "the script printed the root token")
	assert.NotContains(t, out, fakeRecoveryKey, "the script printed the recovery key")
	assert.NotContains(t, git(t, "", "--git-dir", f.repo, "log", "--format=%B", defaultBranch), fakeRootToken, "a commit message holds the root token")
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

func TestInit_ReachesTheRepositoryOverSSHOn443WithTheDeployKeyAndGitHubsPinnedHostKeysAlone(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	f := newFakes(t)

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	var sshCalls []string
	for line := range strings.Lines(out) {
		if strings.Contains(line, "git@ssh.github.com") {
			sshCalls = append(sshCalls, strings.TrimSpace(line))
		}
	}
	require.NotEmpty(t, sshCalls, "the script never reached the repository over ssh:\n%s", out)
	for _, c := range sshCalls {
		for _, want := range []string{"-p 443", "-o IdentitiesOnly=yes", "-o StrictHostKeyChecking=yes", "-o UserKnownHostsFile=", "-i "} {
			assert.Contains(t, c, want, "an ssh call to GitHub")
		}
		assert.Contains(t, c, "'/"+testutil.InitRepository+".git'", "an ssh call to GitHub")
	}
}

func TestInit_CommitsTheRootTokenWhenTheInitsAnswerHoldsNoReadableRecoveryKey(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	for name, recoveryKeys := range map[string]string{"missing": "", "malformed": `,"recovery_keys_base64":["not base64!"]`} {
		t.Run(name, func(t *testing.T) {
			f := newFakes(t)
			f.recoveryKeys = &recoveryKeys

			out, err := f.run(t, recipient)

			require.Error(t, err, "the script exited 0 with the recovery key %s:\n%s", name, out)
			assert.Contains(t, out, "the answer to the init holds no readable recovery key")
			committed, _ := f.file()
			assert.Equal(t, []string{fakeRootToken}, testutil.RecipientDecryptions(t, committed), "the root token is lost:\n%s", out)
			assert.NotContains(t, out, fakeRootToken, "the script printed the root token")
		})
	}
}

func TestInit_CommitsBesideAnotherWritersCommitThatLandedFirst(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	f := newFakes(t)
	f.commit("cluster/app.yaml", "kind: ConfigMap\n")

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	_, ok := f.file()
	assert.True(t, ok, "the init file is not on %s:\n%s", defaultBranch, out)
	assert.Equal(t, "kind: ConfigMap", git(t, "", "--git-dir", f.repo, "show", defaultBranch+":cluster/app.yaml"), "the other writer's file is gone")
}

func TestInit_OnAStoreInitialisedWithItsInitFileCommittedWritesNothing(t *testing.T) {
	_, recipient := testutil.Recipient(t)
	f := newFakes(t)
	f.initialised = true
	f.commit(openbaoinit.InitFile, "OpenBao root token:\nrecorded\n")
	before := f.head()

	out, err := f.run(t, recipient)

	require.NoError(t, err, out)
	assert.Contains(t, out, "holds "+openbaoinit.InitFile+", and this Job no longer holds the root token, so it changes nothing")
	for _, c := range f.callsSoFar() {
		assert.Equal(t, http.MethodGet, c.method, "the script wrote %s %s to OpenBao", c.method, c.path)
	}
	assert.Equal(t, before, f.head(), "the script committed to the repository")
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
				f.commit(openbaoinit.InitFile, "OpenBao root token:\nanother store's\n")
			}
			before := f.head()

			out, err := f.run(t, tc.recipient)

			require.Error(t, err, "the script exited 0 on %s:\n%s", tc.name, out)
			assert.Contains(t, out, tc.want)
			for _, c := range f.callsSoFar() {
				assert.Equal(t, http.MethodGet, c.method, "the script sent %s %s to OpenBao", c.method, c.path)
			}
			assert.Equal(t, before, f.head(), "the script committed to the repository")
			assert.False(t, tc.noCalls && len(f.callsSoFar()) > 0, "the script reached OpenBao before it checked the recipient key")
		})
	}
}

func TestManifests_RunsTheScriptAsAJobInTheDeployKeysNamespaceThatMountsTheKeyTheHostKeysTheRecipientAndOpenBaosAuthority(t *testing.T) {
	docs, pod := rendered(t)

	container := testutil.Field(t, pod, "spec", "containers", "0").(map[string]any)
	env := testutil.ContainerEnv(t, container)

	assert.Equal(t, openbaoinit.DeployKeyNamespace, testutil.Field(t, testutil.Kind(docs, "Job")[0], "metadata", "namespace"))
	assert.NotEmpty(t, testutil.Field(t, pod, "metadata", "labels", "mitstack.dev/app"), "the admission policy refuses a pod without mitstack.dev/app")
	assert.Equal(t, testutil.ToolsImage, container["image"])
	assert.Equal(t, "ssh://git@ssh.github.com:443/"+testutil.InitRepository+".git", env["MITSTACK_REPOSITORY_URL"])
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
		"MITSTACK_DEPLOY_KEY_FILE":  "secret " + openbaoinit.DeployKeySecret + "/" + openbaoinit.DeployKeyItem,
		"MITSTACK_KNOWN_HOSTS_FILE": "configMap mitstack-openbao-init/known_hosts",
		"MITSTACK_RECIPIENT_FILE":   "configMap mitstack-openbao-init/recipient.asc",
		"MITSTACK_OPENBAO_CA":       "configMap openbao-ca/ca.crt",
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
		"a tools image named by tag alone": {func(in *openbaoinit.Inputs) { in.ToolsImage = "ghcr.io/x/tools:v1" }, "tools image"},
		"a repository with no owner":       {func(in *openbaoinit.Inputs) { in.Repository = "shop-cluster" }, `repository "shop-cluster" is not owner/name`},
		"a recipient that is not a key":    {func(in *openbaoinit.Inputs) { in.Recipient = []byte("not a key") }, "the recipient is not an armored OpenPGP public key"},
		"a recipient that cannot encrypt":  {func(in *openbaoinit.Inputs) { in.Recipient = testutil.SignOnlyRecipient(t) }, "has no key that can encrypt"},
		"a recipient given as its private key": {func(in *openbaoinit.Inputs) {
			in.Recipient = testutil.ArmoredPrivateKey(t, owner, openpgp.PrivateKeyType)
		}, "is not an armored OpenPGP public key"},
		"a private key under a public key header": {func(in *openbaoinit.Inputs) {
			in.Recipient = testutil.ArmoredPrivateKey(t, owner, openpgp.PublicKeyType)
		},
			"is not an armored OpenPGP public key"},
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
