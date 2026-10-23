package talosclusterkit

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// The record delivery job runs only on a push to main, so a pin that moved or stopped naming one commit fails here first.
func TestCI_RunsEachBoardCommandFromBlockKitsBoardModuleAtACommitPseudoVersion(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)
	var ci struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ci))

	for job, command := range map[string]string{"pr-key-gate": "pr-keys", "record-delivery": "board-delivery"} {
		pinned := regexp.MustCompile(`^go run github\.com/mitstack-eu/block-kit/board/cmd/` + command + `@v\d+\.\d+\.\d+-\d{14}-[0-9a-f]{12} `)
		runs := 0
		for _, step := range ci.Jobs[job].Steps {
			if pinned.MatchString(step.Run) {
				runs++
			}
		}
		assert.Equal(t, 1, runs, "the %s job runs %s from block-kit's board module at a commit pseudo-version", job, command)
	}
}
