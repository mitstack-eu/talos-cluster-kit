package talosclusterkit

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// The record delivery job runs only on a push to main, so a pin that moved or stopped naming a release fails here first.
func TestCI_RunsEachBoardCommandFromPlainDeliveryAtAReleaseTag(t *testing.T) {
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
		pinned := regexp.MustCompile(`^go run github\.com/mitstack-eu/plain-delivery/cmd/` + command + `@v\d+\.\d+\.\d+ `)
		runs := 0
		for _, step := range ci.Jobs[job].Steps {
			if pinned.MatchString(step.Run) {
				runs++
			}
		}
		assert.Equal(t, 1, runs, "the %s job runs %s from plain-delivery at a release tag", job, command)
	}
}
