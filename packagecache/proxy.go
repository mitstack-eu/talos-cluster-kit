package packagecache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// healthPath answers while the cache serves, whether or not its upstream does, since a job falls back past a cache
// that does not answer.
const healthPath = "/healthz"

func answerHealthy(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// passThrough answers r with what upstream answers it, keeping nothing.
func passThrough(w http.ResponseWriter, r *http.Request, upstream string, client *http.Client) {
	target, err := url.Parse(upstream)
	if err != nil {
		http.Error(w, fmt.Sprintf("the cache's upstream %q: %v", upstream, err), http.StatusBadGateway)
		return
	}
	transport := http.DefaultTransport
	if client.Transport != nil {
		transport = client.Transport
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(target) }, Transport: transport}
	proxy.ServeHTTP(w, r)
}

func fetch(r *http.Request, client *http.Client, upstream, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimSuffix(upstream, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// relayed answers w itself unless resp is a 200: a 404 or 410 as the upstream gave it, since the client then asks
// elsewhere, and any other status as a 502.
func relayed(w http.ResponseWriter, resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusOK:
		return false
	case http.StatusNotFound, http.StatusGone:
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	default:
		http.Error(w, fmt.Sprintf("the cache's upstream answered %s for %s", resp.Status, resp.Request.URL), http.StatusBadGateway)
	}
	return true
}

func serveEntry(w http.ResponseWriter, r *http.Request, entry *os.File, contentType string) {
	defer entry.Close()
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", time.Time{}, entry)
}
