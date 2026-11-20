package cloudtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Refused is a refusal Hetzner answers with an HTTP status and an error code, as a 429 naming rate_limit_exceeded, and
// the reset it names in its RateLimit-Reset header when Reset is set.
type Refused struct {
	Status        int
	Code, Message string
	Reset         time.Time
}

func (r *Refused) Error() string {
	return fmt.Sprintf("Hetzner answered %d %s: %s", r.Status, r.Code, r.Message)
}

// Serve answers a server list at the address it returns with the Refused that FailNext holds, until the test ends,
// and fails the test on any call it does not model.
func (f *Fake) Serve(t testing.TB) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(t, w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *Fake) serve(t testing.TB, w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	if call != "GET /servers" {
		unmodelled(t, w, "the fake serves no %s over HTTP", call)
		return
	}
	_, err := f.ListServers(r.Context(), r.URL.Query().Get("label_selector"))
	var refused *Refused
	if !errors.As(err, &refused) {
		unmodelled(t, w, "the fake serves a server list over HTTP only as the Refused FailNext holds, and the list returned %v", err)
		return
	}
	writeRefusal(w, refused)
}

// unmodelled fails the test from the server's goroutine, where only a non-fatal failure is allowed.
func unmodelled(t testing.TB, w http.ResponseWriter, format string, args ...any) {
	t.Errorf(format, args...)
	w.WriteHeader(http.StatusNotImplemented)
}

func writeRefusal(w http.ResponseWriter, r *Refused) {
	if !r.Reset.IsZero() {
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(r.Reset.Unix(), 10))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": r.Code, "message": r.Message}})
}
