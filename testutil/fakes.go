package testutil

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// Update rewrites every golden file a test compares against, when go test runs with -update.
var Update = flag.Bool("update", false, "rewrite the golden files from the current render")

// Golden compares got with the golden file at path, or rewrites the file under -update.
func Golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *Update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "no golden file at %s; run go test with -update to record it", path)
	require.Equal(t, string(want), string(got), "the render differs from %s; run go test with -update to record it on purpose", path)
}

// Tools is a directory of fake commands that each append their arguments to one calls log.
type Tools struct {
	Dir   string
	calls string
}

// NewTools writes each fake, a bash body run after its call is logged, into a new directory.
func NewTools(t *testing.T, fakes map[string]string) *Tools {
	t.Helper()
	tools := &Tools{Dir: t.TempDir()}
	tools.calls = filepath.Join(tools.Dir, "calls.log")
	for name, body := range fakes {
		script := "#!/usr/bin/env bash\nset -euo pipefail\nprintf '%s %s\\n' " + name + " \"$*\" >> " + shellQuote(tools.calls) + "\n" + body + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(tools.Dir, name), []byte(script), 0o755))
	}
	return tools
}

// Calls returns each logged call as "<command> <arguments>", in the order they ran.
func (tools *Tools) Calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(tools.calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// Env returns an environment whose PATH finds the fakes before the system's own commands.
func (tools *Tools) Env(extra ...string) []string {
	return append([]string{"PATH=" + tools.Dir + ":" + os.Getenv("PATH"), "HOME=" + tools.Dir}, extra...)
}

// RunBash runs script with bash in env and returns its combined output and exit error.
func RunBash(t *testing.T, script string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", script, "script"}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func Documents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		require.NoError(t, err)
		if doc != nil {
			docs = append(docs, doc)
		}
	}
}

// Kind returns the documents of one kind, in their order.
func Kind(docs []map[string]any, kind string) []map[string]any {
	var matching []map[string]any
	for _, doc := range docs {
		if doc["kind"] == kind {
			matching = append(matching, doc)
		}
	}
	return matching
}

// Field walks path through nested maps and lists, where a list step is its decimal index.
func Field(t *testing.T, doc any, path ...string) any {
	t.Helper()
	node := doc
	for _, step := range path {
		switch v := node.(type) {
		case map[string]any:
			node = v[step]
		case []any:
			i, err := strconv.Atoi(step)
			require.NoError(t, err, "step %q of %v indexes a list", step, path)
			require.Less(t, i, len(v), "index %s past a list of %d", step, len(v))
			node = v[i]
		default:
			require.Failf(t, "no such field", "step %q of %v reaches %T", step, path, node)
		}
	}
	return node
}

func ContainerEnv(t *testing.T, container any) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, e := range Field(t, container, "env").([]any) {
		env[Field(t, e, "name").(string)] = fmt.Sprint(Field(t, e, "value"))
	}
	return env
}
