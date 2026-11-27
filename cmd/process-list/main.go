// Command process-list writes the command line of each control-plane component running on one node, read from Talos's
// process list, into one file per component under the name kube-bench's cis-1.10 audits read it by.
//
// Usage: process-list <node address> <directory>
//
// It writes nothing and exits 1, naming each missing component, when one of them runs in no process on the node, since
// kube-bench would otherwise judge a default in place of a flag it could not read; it exits 2 on a wrong argument. It
// exists because no pod reads another's command line unless it shares the host's process table, which the security
// scan does not, while Talos's MachineService/Processes gives the same command lines to an os:reader identity.
package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	exitFailed = 1
	exitUsage  = 2
)

// components maps each control-plane executable to the name kube-bench's audits read its command line by.
var components = map[string]string{
	"kube-apiserver":          "apiserver",
	"kube-controller-manager": "controllermanager",
	"kube-scheduler":          "scheduler",
	"etcd":                    "etcd",
	"kubelet":                 "kubelet",
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintf(stderr, "process-list: takes a node address and a directory, got %q\n", args)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := client.New(ctx, client.WithDefaultConfig())
	if err != nil {
		fmt.Fprintf(stderr, "process-list: connect to the Talos API: %v\n", err)
		return exitFailed
	}
	defer c.Close()
	if err := write(ctx, c.MachineClient, args[0], args[1]); err != nil {
		fmt.Fprintf(stderr, "process-list: %v\n", err)
		return exitFailed
	}
	return 0
}

// write asks node for its process list and writes each component's command lines, one per line, into dir.
func write(ctx context.Context, talos machine.MachineServiceClient, node, dir string) error {
	resp, err := talos.Processes(client.WithNode(ctx, node), &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("reading the process list of %s: %w", node, err)
	}
	lines := map[string]string{}
	for _, message := range resp.GetMessages() {
		if e := message.GetMetadata().GetError(); e != "" {
			return fmt.Errorf("reading the process list of %s: %s", node, e)
		}
		for _, process := range message.GetProcesses() {
			fields := strings.Fields(process.GetArgs())
			if len(fields) == 0 {
				continue
			}
			if component, ok := components[path.Base(fields[0])]; ok {
				lines[component] += process.GetArgs() + "\n"
			}
		}
	}
	var missing []string
	for executable, component := range components {
		if lines[component] == "" {
			missing = append(missing, executable)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("%s runs no %s process, so no command line was written", node, strings.Join(missing, " or "))
	}
	for _, component := range slices.Sorted(maps.Keys(lines)) {
		if err := os.WriteFile(filepath.Join(dir, component), []byte(lines[component]), 0o644); err != nil {
			return err
		}
	}
	return nil
}
