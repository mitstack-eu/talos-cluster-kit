// Package clusterrepo names, dials, reads, creates, marks, seeds, deletes and observes a Talos cluster's GitHub
// repository, calling ghapi's allowlisted base directly for the topics and contents calls ghapi.Client lacks.
package clusterrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/github/ghapi"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/safe"
	"github.com/mitstack-eu/block-kit/observe"
)

const (
	githubAPIVersion     = "2022-11-28"
	githubTimeout        = 30 * time.Second
	maxGitHubAnswerBytes = 64 << 10
	defaultGitHubAPI     = "https://" + ghapi.APIGitHubHost
	SeedMarkerFile       = "cluster/README.md"
)

// seedFiles open the two directories the cluster's Argo CD Applications sync, so each syncs from its first commit.
var seedFiles = map[string]string{
	SeedMarkerFile: "# cluster\n\nArgo CD applies every manifest in this directory to the cluster.\n",
	"repository-credentials/README.md": "# repository-credentials\n\n" +
		"Each app slice commits its repository Secret and Role here, and the cluster's token renewal writes the Secret's password.\n",
}

var repoTopicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)

type Repo struct {
	Owner, Name string
}

func For(owner, appName string) Repo {
	return Repo{Owner: owner, Name: appName + "-cluster"}
}

func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

func (r Repo) URL() string { return "https://github.com/" + r.FullName() }

// Access reads and writes one subscription's cluster repository for one block.
type Access struct {
	Client  *ghapi.Client
	Repo    Repo
	blockID string
	subID   string
}

// DialFunc dials the cluster repository of a request's app, as an operator binds Dial to its block and client.
type DialFunc func(d contract.Desired, appName string) (*Access, error)

// NewClient builds the client Dial reads GitHub with, bounding each call's time.
func NewClient(base, token string) *ghapi.Client {
	return ghapi.NewClient(base, token, &http.Client{Timeout: githubTimeout})
}

// Dial refuses, before any request, a request whose GitHub connection cannot be acted on, and resolves the API base
// through the operator's allowlist, never from the request alone; newClient lets a test use an allowlisted fake.
func Dial(d contract.Desired, blockID, appName string, newClient func(base, token string) *ghapi.Client) (*Access, error) {
	token := d.Secrets["github"]["token"]
	if token == "" {
		return nil, handler.Refuse("missing-github-access",
			"the GitHub connection is missing: secrets.github.token is empty, so the cluster repository cannot be read, created or deleted; reconnect GitHub for this app and retry")
	}
	owner := d.Secrets["github"]["owner"]
	if owner == "" {
		return nil, handler.Refuse("missing-github-access",
			"the GitHub connection names no owner: secrets.github.owner is empty, so the cluster repository has no account to live in; reconnect GitHub for this app and retry")
	}
	repo := For(owner, appName)
	if _, err := ghapi.SafeRepo(repo.FullName()); err != nil {
		return nil, handler.Refuse("invalid-param", "the cluster repository name is not usable: %v", err)
	}
	if topic := SubscriptionTopic(d.SubscriptionID); !repoTopicRe.MatchString(topic) {
		return nil, handler.Refuse("invalid-subscription",
			"the subscription id cannot form the repository topic %s, which GitHub takes only as at most 50 lowercase letters, digits and hyphens", topic)
	}
	endpoint := d.Instances["github"].Endpoint
	if endpoint == "" {
		endpoint = defaultGitHubAPI
	}
	base, err := ghapi.Base(endpoint, strings.Split(os.Getenv(ghapi.HostsEnv), ",")...)
	if err != nil {
		return nil, handler.Refuse("untrusted-api-base", "github api base rejected: %v", err)
	}
	return &Access{Client: newClient(base.String(), token), Repo: repo, blockID: blockID, subID: d.SubscriptionID}, nil
}

// DialApp dials the repository of the app the request names, refusing a request that names none.
func DialApp(d contract.Desired, blockID string, newClient func(base, token string) *ghapi.Client) (*Access, error) {
	appName := d.Params["appName"]
	if appName == "" {
		return nil, handler.Refuse("missing-param", `the app name is required to name the cluster repository: params["appName"] is empty`)
	}
	return Dial(d, blockID, appName, newClient)
}

func (a *Access) SubscriptionID() string { return a.subID }

// SubscriptionTopic lowercases first, since GitHub takes only lowercase topics and block-kit's helper keeps case.
func SubscriptionTopic(subID string) string {
	return observe.GitHubSubscriptionTopic(strings.ToLower(subID))
}

// Mark is the pair of topics that marks a repository as one block's cluster repository for one subscription.
func Mark(blockID, subID string) []string {
	return []string{"mitstack-" + blockID, SubscriptionTopic(subID)}
}

// CarriesMark requires both topics, since Ensure adopts, Observe counts and an unsubscribe deletes only a whole mark.
func CarriesMark(topics []string, blockID, subID string) bool {
	for _, want := range Mark(blockID, subID) {
		if !slices.Contains(topics, want) {
			return false
		}
	}
	return true
}

type Read struct {
	Exists, Marked bool
}

// Read reads the topics from their own endpoint, since GitHub leaves them out of the repository.
func (a *Access) Read(ctx context.Context) (Read, error) {
	_, found, err := a.GetIfFound(ctx, "/repos/"+a.Repo.FullName(), a.Repo.FullName())
	if err != nil || !found {
		return Read{}, err
	}
	topics, found, err := a.readTopics(ctx)
	if err != nil || !found {
		return Read{}, err
	}
	return Read{Exists: true, Marked: CarriesMark(topics, a.blockID, a.subID)}, nil
}

// RefuseUnmarked refuses a repository that exists without the subscription's mark, which is not the cluster's to take.
func (a *Access) RefuseUnmarked(read Read) error {
	if read.Exists && !read.Marked {
		return handler.Refuse("repository-not-marked",
			"the repository %s exists but does not carry this subscription's mark, the topics %s, so nothing was changed. Add both topics if it is this cluster's repository, or move it out of the way, and retry",
			a.Repo.FullName(), strings.Join(Mark(a.blockID, a.subID), " and "))
	}
	return nil
}

// Ensure creates and marks the cluster repository only when none exists, since the caller has already refused an
// unmarked one, and seeds whatever of the seed is missing.
func (a *Access) Ensure(ctx context.Context, read Read) error {
	if !read.Exists {
		if err := a.create(ctx); err != nil {
			return err
		}
		if err := a.mark(ctx); err != nil {
			return err
		}
	}
	return a.seed(ctx, seedFiles)
}

func (a *Access) readTopics(ctx context.Context) ([]string, bool, error) {
	raw, found, err := a.GetIfFound(ctx, "/repos/"+a.Repo.FullName()+"/topics", "the topics of "+a.Repo.FullName())
	if err != nil || !found {
		return nil, found, err
	}
	var topics struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(raw, &topics); err != nil {
		return nil, false, fmt.Errorf("reading the topics of %s: %w", a.Repo.FullName(), err)
	}
	return topics.Names, true, nil
}

// GetIfFound reads a GitHub path, answering found false on a 404; what names the path in an error.
func (a *Access) GetIfFound(ctx context.Context, path, what string) ([]byte, bool, error) {
	status, raw, err := a.Call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, false, err
	}
	switch status {
	case http.StatusOK:
		return raw, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("reading %s: github returned %d", what, status)
	}
}

// create skips a read, so GitHub's 422 stops a repository that appeared after Read being marked.
func (a *Access) create(ctx context.Context) error {
	status, raw, err := a.Call(ctx, http.MethodPost, "/orgs/"+url.PathEscape(a.Repo.Owner)+"/repos", map[string]any{
		"name": a.Repo.Name, "private": true, "auto_init": true,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("creating the cluster repository %s: github returned %d: %s",
			a.Repo.FullName(), status, safe.LogSafe(string(raw), a.Client.Token))
	}
	return nil
}

// mark writes the mark beside the topics already there, since GitHub replaces the whole list.
func (a *Access) mark(ctx context.Context) error {
	topics, found, err := a.readTopics(ctx)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("marking the cluster repository %s: github answered 404 for its topics", a.Repo.FullName())
	}
	for _, topic := range Mark(a.blockID, a.subID) {
		if !slices.Contains(topics, topic) {
			topics = append(topics, topic)
		}
	}
	status, raw, err := a.Call(ctx, http.MethodPut, "/repos/"+a.Repo.FullName()+"/topics", map[string]any{"names": topics})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("marking the cluster repository %s: github returned %d: %s",
			a.Repo.FullName(), status, safe.LogSafe(string(raw), a.Client.Token))
	}
	return nil
}

// HoldsSeed reads whether the seed's marker file is in the repository.
func (a *Access) HoldsSeed(ctx context.Context) (bool, error) {
	_, found, err := a.GetIfFound(ctx, "/repos/"+a.Repo.FullName()+"/contents/"+SeedMarkerFile, SeedMarkerFile+" in "+a.Repo.FullName())
	return found, err
}

// seed writes only the files not there yet, since one there is an earlier seed or the owner's own commit.
func (a *Access) seed(ctx context.Context, files map[string]string) error {
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := a.seedFile(ctx, path, files[path]); err != nil {
			return err
		}
	}
	return nil
}

func (a *Access) seedFile(ctx context.Context, path, content string) error {
	contents := "/repos/" + a.Repo.FullName() + "/contents/" + path
	_, found, err := a.GetIfFound(ctx, contents, path+" in "+a.Repo.FullName())
	if err != nil || found {
		return err
	}
	status, raw, err := a.Call(ctx, http.MethodPut, contents, map[string]any{
		"message": "Seed " + path + "\n\n" + observe.GitHubSubscriptionTrailer(a.subID),
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("seeding %s in the cluster repository %s: github returned %d: %s",
			path, a.Repo.FullName(), status, safe.LogSafe(string(raw), a.Client.Token))
	}
	return nil
}

// Delete deletes the cluster repository; the caller reads its mark first.
func (a *Access) Delete(ctx context.Context) error {
	if err := a.Client.DeleteRepo(ctx, a.Repo.Owner, a.Repo.Name); err != nil {
		return fmt.Errorf("deleting the cluster repository %s: %w", a.Repo.FullName(), err)
	}
	return nil
}

// Call reads at most 64 KiB of the body, so an answer of any size is read in bounded memory.
func (a *Access) Call(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	return a.CallLimited(ctx, method, path, payload, maxGitHubAnswerBytes)
}

func (a *Access) CallLimited(ctx context.Context, method, path string, payload any, limit int64) (int, []byte, error) {
	req, err := a.newRequest(ctx, method, path, payload)
	if err != nil {
		return 0, nil, err
	}
	httpClient := a.Client.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("github %s %s: github unreachable: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return 0, nil, fmt.Errorf("github %s %s: reading the answer: %w", method, path, err)
	}
	return resp.StatusCode, raw, nil
}

func (a *Access) newRequest(ctx context.Context, method, path string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding github %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Client.Base+path, body)
	if err != nil {
		return nil, fmt.Errorf("building github %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+a.Client.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
