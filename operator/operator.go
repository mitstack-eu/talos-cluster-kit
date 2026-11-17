// Package operator holds the start-up and the request reads every Talos cluster operator built on block-kit shares.
package operator

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/webhook"
)

const (
	defaultListenAddr = ":8080"
	logBufferLines    = 500
	readHeaderTimeout = 10 * time.Second
)

type ConfigFunc func(jwksURL string, logs *webhook.LogBuffer) handler.Config

type RoutesFunc func(hook http.HandlerFunc) *http.ServeMux

type Settings struct {
	JWKSURL string
	Addr    string
}

// ReadSettings refuses a start without MITSTACK_JWKS_URL, and listens on LISTEN_ADDR or port 8080.
func ReadSettings(getenv func(string) string) (Settings, error) {
	s := Settings{JWKSURL: getenv("MITSTACK_JWKS_URL"), Addr: getenv("LISTEN_ADDR")}
	if s.JWKSURL == "" {
		return Settings{}, errors.New("MITSTACK_JWKS_URL is required: the trust boundary resolves mitstack's signing key from it")
	}
	if s.Addr == "" {
		s.Addr = defaultListenAddr
	}
	return s, nil
}

// NewServer bounds the time a client may take to send its request's headers.
func NewServer(addr string, mux http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
}

// Main serves the operator until its server stops, and /logs serves only what passes through the standard logger.
func Main(config ConfigFunc, routes RoutesFunc) {
	settings, err := ReadSettings(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	logs := webhook.NewLogBuffer(logBufferLines)
	log.SetOutput(io.MultiWriter(os.Stderr, logs.Writer()))

	h, err := handler.New(config(settings.JWKSURL, logs))
	if err != nil {
		log.Fatalf("building the lifecycle handler: %v", err)
	}

	srv := NewServer(settings.Addr, routes(h.Serve))
	log.Printf("operator listening on %s", settings.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serving: %v", err)
	}
}
