// Command machine-config prints the configuration a release of this kit gives a running node.
//
// Usage: machine-config --release <tools image digest> < running.yaml > next.yaml
//
// It reads the node's current machine configuration on stdin and writes the configuration the release renders for
// that node on stdout, through nodeconfig.Rerender. It exits 3, printing nothing, when the release changes a setting
// the node takes only at reboot, so the caller leaves that node as it runs; 2 on a wrong argument; 1 on any other
// failure. It exists because the cluster's keys live only on its nodes, so the rendering runs inside the cluster.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

const (
	exitFailed     = 1
	exitUsage      = 2
	exitRebootOnly = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("machine-config", flag.ContinueOnError)
	flags.SetOutput(stderr)
	release := flags.String("release", "", "the digest of the tools image whose release renders the configuration")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case *release == "":
		fmt.Fprintln(stderr, "machine-config: --release names no release")
		return exitUsage
	case flags.NArg() > 0:
		fmt.Fprintf(stderr, "machine-config: takes no argument beside --release, got %q\n", flags.Args())
		return exitUsage
	}
	running, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "machine-config: read the running configuration: %v\n", err)
		return exitFailed
	}
	next, err := nodeconfig.Rerender(running, *release)
	switch {
	case errors.Is(err, nodeconfig.ErrRebootOnly):
		fmt.Fprintf(stderr, "machine-config: %v\n", err)
		return exitRebootOnly
	case err != nil:
		fmt.Fprintf(stderr, "machine-config: render the running configuration: %v\n", err)
		return exitFailed
	}
	if _, err := stdout.Write(next); err != nil {
		fmt.Fprintf(stderr, "machine-config: write the configuration: %v\n", err)
		return exitFailed
	}
	return 0
}
