package clusterrepo_test

// A fake GitHub answering the repository, topics and contents calls a cluster repository's handling makes.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mitstack-eu/block-kit/capabilities/github/ghapi"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

const (
	testBlock = "hcloud-talos-cluster"
	testSub   = "sub_Ab12"
	testOwner = "acme"
	testApp   = "shop"
	testRepo  = "acme/shop-cluster"
	testToken = "gh-test-token"
)

type fakeAnswer struct {
	status int
	body   string
}

type seededFile struct {
	content, message string
}

type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// repos holds each repository's topics by full name.
	repos map[string][]string
	// files holds each file by "owner/name/path".
	files map[string]seededFile
	// createdTopics are the topics GitHub gives a repository it creates.
	createdTopics []string
	// answers override the answer to "METHOD /path".
	answers  map[string]fakeAnswer
	calls    []string
	bodies   map[string]map[string]any
	headers  map[string]http.Header
	bases    []string
	clientOf string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{
		t:       t,
		repos:   map[string][]string{},
		files:   map[string]seededFile{},
		answers: map[string]fakeAnswer{},
		bodies:  map[string]map[string]any{},
		headers: map[string]http.Header{},
	}
	gh.server = httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(gh.server.Close)
	return gh
}

// client stands in for ghapi.NewClient, recording the base Dial resolved and pointing the client at the fake.
func (gh *fakeGitHub) client(base, token string) *ghapi.Client {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.bases = append(gh.bases, base)
	gh.clientOf = token
	return ghapi.NewClient(gh.server.URL, token, gh.server.Client())
}

func (gh *fakeGitHub) dial(d contract.Desired, appName string) (*clusterrepo.Access, error) {
	return clusterrepo.Dial(d, testBlock, appName, gh.client)
}

func (gh *fakeGitHub) access(t *testing.T) *clusterrepo.Access {
	t.Helper()
	a, err := gh.dial(desired(), testApp)
	require.NoError(t, err)
	return a
}

func (gh *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	gh.calls = append(gh.calls, call)
	gh.headers[call] = r.Header.Clone()
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		var body map[string]any
		require.NoError(gh.t, json.Unmarshal(raw, &body), "the body of %s is JSON", call)
		gh.bodies[call] = body
	}
	if a, ok := gh.answers[call]; ok {
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
		return
	}
	gh.answer(w, r)
}

func (gh *fakeGitHub) answer(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/orgs/"+testOwner+"/repos":
		gh.repos[testOwner+"/"+gh.bodies[r.Method+" "+path]["name"].(string)] = append([]string(nil), gh.createdTopics...)
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(path, "/repos/"+testRepo+"/contents/"):
		gh.serveContents(w, r, strings.TrimPrefix(path, "/repos/"))
	case path == "/repos/"+testRepo+"/topics":
		gh.serveTopics(w, r)
	case path == "/repos/"+testRepo:
		gh.serveRepo(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (gh *fakeGitHub) serveRepo(w http.ResponseWriter, r *http.Request) {
	_, exists := gh.repos[testRepo]
	switch {
	case !exists:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodDelete:
		delete(gh.repos, testRepo)
		w.WriteHeader(http.StatusNoContent)
	default:
		_, _ = io.WriteString(w, `{"full_name":"`+testRepo+`"}`)
	}
}

func (gh *fakeGitHub) serveTopics(w http.ResponseWriter, r *http.Request) {
	if _, exists := gh.repos[testRepo]; !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPut {
		var names []string
		for _, n := range gh.bodies[r.Method+" "+r.URL.Path]["names"].([]any) {
			names = append(names, n.(string))
		}
		gh.repos[testRepo] = names
	}
	_ = json.NewEncoder(w).Encode(map[string][]string{"names": gh.repos[testRepo]})
}

func (gh *fakeGitHub) serveContents(w http.ResponseWriter, r *http.Request, file string) {
	path := strings.Replace(file, "/contents/", "/", 1)
	if r.Method == http.MethodPut {
		body := gh.bodies[r.Method+" "+r.URL.Path]
		content, err := base64.StdEncoding.DecodeString(body["content"].(string))
		require.NoError(gh.t, err, "the content of %s is base64", path)
		gh.files[path] = seededFile{content: string(content), message: body["message"].(string)}
		w.WriteHeader(http.StatusCreated)
		return
	}
	if _, ok := gh.files[path]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = io.WriteString(w, `{"type":"file"}`)
}

func (gh *fakeGitHub) holdRepo(topics ...string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.repos[testRepo] = topics
}

func (gh *fakeGitHub) holdFile(path, content string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.files[testRepo+"/"+path] = seededFile{content: content}
}

func (gh *fakeGitHub) answerWith(call string, status int, body string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.answers[call] = fakeAnswer{status: status, body: body}
}

func (gh *fakeGitHub) madeCalls() []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]string(nil), gh.calls...)
}

func (gh *fakeGitHub) topics() []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]string(nil), gh.repos[testRepo]...)
}

func (gh *fakeGitHub) file(path string) (seededFile, bool) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	f, ok := gh.files[testRepo+"/"+path]
	return f, ok
}

type desiredOption func(*contract.Desired)

func desired(opts ...desiredOption) contract.Desired {
	d := contract.Desired{
		Identity: contract.Identity{SubscriptionID: testSub},
		Params:   map[string]string{"appName": testApp},
		Secrets:  map[string]map[string]string{"github": {"token": testToken, "owner": testOwner}},
	}
	for _, opt := range opts {
		opt(&d)
	}
	return d
}

func withGitHubSecret(key, value string) desiredOption {
	return func(d *contract.Desired) { d.Secrets["github"][key] = value }
}

func withParam(key, value string) desiredOption {
	return func(d *contract.Desired) { d.Params[key] = value }
}

func withSubscription(id string) desiredOption {
	return func(d *contract.Desired) { d.SubscriptionID = id }
}

func withGitHubEndpoint(endpoint string) desiredOption {
	return func(d *contract.Desired) {
		d.Instances = map[string]contract.InstanceConfig{"github": {Endpoint: endpoint}}
	}
}

func ctx() context.Context { return context.Background() }
