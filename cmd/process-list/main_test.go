// process-list writes each control-plane component's command line from Talos's process list.
package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/api/common"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fakeTalos answers MachineService/Processes with one node's process list, and keeps the node each call was sent to.
type fakeTalos struct {
	machine.UnimplementedMachineServiceServer
	answer *machine.Process
	nodes  []string
}

func (f *fakeTalos) Processes(ctx context.Context, _ *emptypb.Empty) (*machine.ProcessesResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.nodes = append(f.nodes, md.Get("node")...)
	return &machine.ProcessesResponse{Messages: []*machine.Process{f.answer}}, nil
}

// talosAPI serves fake over an in-memory connection and returns a client of it.
func talosAPI(t *testing.T, fake *fakeTalos) machine.MachineServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	machine.RegisterMachineServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///talos", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return machine.NewMachineServiceClient(conn)
}

// controlPlane is a control-plane node's process list, its API server running with flags.
func controlPlane(flags string) []*machine.ProcessInfo {
	return []*machine.ProcessInfo{
		{Pid: 1, Args: "/sbin/init"},
		{Pid: 2, Command: "kthreadd"},
		{Pid: 10, Args: "/usr/local/bin/kube-apiserver " + flags},
		{Pid: 11, Args: "/usr/local/bin/kube-controller-manager --profiling=false"},
		{Pid: 12, Args: "/usr/local/bin/kube-scheduler --profiling=false"},
		{Pid: 13, Args: "/usr/local/bin/etcd --data-dir=/var/lib/etcd --client-cert-auth=true"},
		{Pid: 14, Args: "/usr/local/bin/kubelet --config=/etc/kubernetes/kubelet.yaml"},
		{Pid: 15, Args: "/usr/local/bin/etcdctl --endpoints=https://127.0.0.1:2379 member list"},
		{Pid: 16, Args: "/usr/bin/containerd-shim-runc-v2 -namespace k8s.io -id kube-apiserver"},
	}
}

func written(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	files := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		files[entry.Name()] = string(data)
	}
	return files
}

func TestWrite_WritesEachComponentsCommandLineFromItsNodesProcessListWithAFlagATalosPatchSet(t *testing.T) {
	fake := &fakeTalos{answer: &machine.Process{Processes: controlPlane("--profiling=false --audit-log-maxage=45")}}
	dir := t.TempDir()

	require.NoError(t, write(t.Context(), talosAPI(t, fake), "10.0.0.6", dir))

	assert.Equal(t, []string{"10.0.0.6"}, fake.nodes, "the reader asks its own node")
	assert.Equal(t, map[string]string{
		"apiserver":         "/usr/local/bin/kube-apiserver --profiling=false --audit-log-maxage=45\n",
		"controllermanager": "/usr/local/bin/kube-controller-manager --profiling=false\n",
		"scheduler":         "/usr/local/bin/kube-scheduler --profiling=false\n",
		"etcd":              "/usr/local/bin/etcd --data-dir=/var/lib/etcd --client-cert-auth=true\n",
		"kubelet":           "/usr/local/bin/kubelet --config=/etc/kubernetes/kubelet.yaml\n",
	}, written(t, dir), "each file holds only the processes that run that component's executable, under the name kube-bench gives it")
}

func TestWrite_WritesNothingAndNamesWhyWhenTheProcessListLacksAComponent(t *testing.T) {
	withoutEtcd := controlPlane("--profiling=false")
	withoutEtcd = append(withoutEtcd[:5], withoutEtcd[6:]...)
	for _, tc := range []struct {
		name   string
		answer *machine.Process
		want   string
	}{
		{"a node whose etcd process is not in the list", &machine.Process{Processes: withoutEtcd},
			"10.0.0.6 runs no etcd process, so no command line was written"},
		{"a node that runs none of them", &machine.Process{},
			"10.0.0.6 runs no etcd or kube-apiserver or kube-controller-manager or kube-scheduler or kubelet process, so no command line was written"},
		{"a node whose answer carries an error", &machine.Process{Metadata: &common.Metadata{Error: "permission denied"}, Processes: controlPlane("")},
			"reading the process list of 10.0.0.6: permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			err := write(t.Context(), talosAPI(t, &fakeTalos{answer: tc.answer}), "10.0.0.6", dir)

			require.EqualError(t, err, tc.want)
			assert.Empty(t, written(t, dir), "kube-bench reads no partial set as defaults")
		})
	}
}
