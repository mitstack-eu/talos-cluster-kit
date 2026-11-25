// package-cache runs one of the runner plane's shared caches, or the sweep of a directory another server fills, each
// holding its directory under a size and an age limit.
//
// Usage:
//
//	package-cache go --listen=:8080 --root=DIR --upstream=URL --checksum-db=URL --checksum-db-key=KEY --max-size=5Gi --max-age=336h
//	package-cache npm --listen=:8080 --root=DIR --upstream=URL --max-size=3Gi --max-age=336h
//	package-cache sweep --dir=DIR --max-size=10Gi --max-age=336h
//
// It exists so a runner job takes a Go module, an npm tarball or a Docker Hub image from inside the plane, checked by
// its digest, rather than from the internet each time.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/mitstack-eu/talos-cluster-kit/packagecache"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "package-cache:", err)
		os.Exit(1)
	}
}

// quantity is a size flag in Kubernetes' notation, as the cache's Deployment sizes its disk.
type quantity struct{ bytes int64 }

func (q *quantity) String() string { return fmt.Sprint(q.bytes) }

func (q *quantity) Set(value string) error {
	parsed, err := resource.ParseQuantity(value)
	if err != nil {
		return err
	}
	q.bytes = parsed.Value()
	return nil
}

func run(ctx context.Context, args []string, logs io.Writer) error {
	if len(args) == 0 || (args[0] != "go" && args[0] != "npm" && args[0] != "sweep") {
		return fmt.Errorf("package-cache runs go, npm or sweep, given %q", args)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(logs)
	var size quantity
	flags.Var(&size, "max-size", "the bytes past which the sweep removes the least recently used entries")
	age := flags.Duration("max-age", 0, "how long an entry may sit unused before the sweep removes it")
	every := flags.Duration("sweep-every", 5*time.Minute, "how often the sweep runs")
	dir := flags.String("dir", "", "the directory the sweep evicts from")
	root := flags.String("root", "", "the directory the cache keeps its entries under")
	listen := flags.String("listen", ":8080", "the address the cache serves on")
	upstream := flags.String("upstream", "", "the registry or proxy the cache fetches from")
	sumdbURL := flags.String("checksum-db", "", "the Go checksum database the Go cache checks each module against")
	sumdbKey := flags.String("checksum-db-key", "", "the key that verifies the checksum database's signatures")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	limits := packagecache.Limits{MaxBytes: size.bytes, MaxAge: *age}
	switch {
	case size.bytes <= 0:
		return errors.New("--max-size names no size above zero")
	case *age <= 0:
		return errors.New("--max-age names no age above zero")
	case args[0] == "sweep" && *dir == "":
		return errors.New("--dir names no directory to sweep")
	case args[0] == "sweep":
		return sweep(ctx, *dir, limits, *every, logs)
	case *root == "":
		return errors.New("--root names no directory to keep the cache's entries under")
	case *upstream == "":
		return errors.New("--upstream names no registry to fetch from")
	case args[0] == "go" && (*sumdbURL == "" || *sumdbKey == ""):
		return errors.New("--checksum-db and --checksum-db-key name no checksum database to check modules against")
	}
	store, err := packagecache.NewStore(*root, time.Now)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	var handler http.Handler = packagecache.NPMProxy{Upstream: *upstream, Store: store, Client: client}
	if args[0] == "go" {
		handler = packagecache.GoProxy{Upstream: *upstream, Store: store, Client: client,
			Sums: packagecache.NewChecksumDB(*sumdbURL, *sumdbKey, client, store)}
	}
	return serve(ctx, &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 30 * time.Second,
		ErrorLog: log.New(logs, "", log.LstdFlags)}, store.Dir(), limits, *every, logs)
}

// serve runs the cache and its sweep until ctx ends or either fails.
func serve(ctx context.Context, srv *http.Server, dir string, limits packagecache.Limits, every time.Duration, logs io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan error, 2)
	go func() { failed <- srv.ListenAndServe() }()
	go func() { failed <- sweep(ctx, dir, limits, every, logs) }()
	select {
	case <-ctx.Done():
	case err := <-failed:
		if !errors.Is(err, http.ErrServerClosed) && err != nil {
			return err
		}
	}
	shutdown, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()
	return srv.Shutdown(shutdown)
}

// sweep evicts from dir now and then every interval until ctx ends, failing on the first sweep that fails, so the
// container restarts and its pod reads unready rather than letting the disk fill.
func sweep(ctx context.Context, dir string, limits packagecache.Limits, every time.Duration, logs io.Writer) error {
	for {
		swept, err := packagecache.Evict(dir, limits, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(logs, "swept %s: removed %d entries, %d bytes kept\n", dir, swept.Removed, swept.Kept)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}
