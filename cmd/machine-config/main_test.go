// machine-config prints the configuration a release gives a running node from that node's configuration on stdin.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const release = "sha256:5f0c1b2a3d4e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcd"

func TestRun_PrintsTheConfigurationTheReleaseGivesTheRunningNodeOnStdin(t *testing.T) {
	cluster := testutil.NewCluster(t)
	running, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	cluster.Release = release
	want, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer

	code := run([]string{"--release", release}, bytes.NewReader(running), &stdout, &stderr)

	require.Equal(t, 0, code, stderr.String())
	assert.Equal(t, testutil.Documents(t, want), testutil.Documents(t, stdout.Bytes()))
	assert.Empty(t, stderr.String())
}

func TestRun_ExitsWithRebootOnlyAndPrintsNothingWhenTheReleaseChangesASettingTheNodeTakesOnlyAtBoot(t *testing.T) {
	cluster := testutil.NewCluster(t)
	current, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	running := strings.Replace(string(current), "    etcd:\n", "    etcd:\n        extraArgs:\n            election-timeout: \"5000\"\n", 1)
	require.NotEqual(t, string(current), running, "the running configuration carries an etcd setting the release does not")
	var stdout, stderr bytes.Buffer

	code := run([]string{"--release", release}, strings.NewReader(running), &stdout, &stderr)

	assert.Equal(t, exitRebootOnly, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "cluster.etcd")
}

func TestRun_RefusesNamingWhatIsWrongWhenItCannotRenderTheNode(t *testing.T) {
	cluster := testutil.NewCluster(t)
	running, err := nodeconfig.Render(cluster, nodeconfig.Node{})
	require.NoError(t, err)
	cases := []struct {
		name     string
		args     []string
		stdin    string
		wantCode int
		wantText string
	}{
		{"no release", nil, string(running), exitUsage, "--release"},
		{"an argument beside the release", []string{"--release", release, "extra"}, string(running), exitUsage, "extra"},
		{"no configuration on stdin", []string{"--release", release}, "", exitFailed, "configuration"},
		{"no machine configuration on stdin", []string{"--release", release}, "kind: ConfigMap\napiVersion: v1\n", exitFailed, "configuration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)

			assert.Equal(t, tc.wantCode, code)
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), tc.wantText)
		})
	}
}
