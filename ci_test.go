package talosclusterkit

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

const (
	pollingCheckRun  = `go tool checkpolls -base "$BASE_SHA"`
	pollingCheckTool = "github.com/mitstack-eu/block-kit/cmd/checkpolls"
	pullRequestBase  = "${{ github.event.pull_request.base.sha }}"
)

var firstKitWithPollingCheck = []int{0, 49, 1}

type ciStep struct {
	Uses             string            `yaml:"uses"`
	With             map[string]any    `yaml:"with"`
	If               string            `yaml:"if"`
	Run              string            `yaml:"run"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
	ContinueOnError  any               `yaml:"continue-on-error"`
}

func TestCI_TestJobFailsAPullRequestThatAddsAFixedSleepOrAPollingIntervalAgainstItsBaseCommit(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)
	var ci struct {
		Jobs map[string]struct {
			Steps []ciStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ci))
	steps := ci.Jobs["test"].Steps
	require.NotEmpty(t, steps, "the CI workflow has a test job")

	at := slices.IndexFunc(steps, func(s ciStep) bool { return s.Run == pollingCheckRun })

	require.GreaterOrEqual(t, at, 0, "the test job runs `%s`, so a pull request adding a fixed sleep or a polling interval fails", pollingCheckRun)
	check := steps[at]
	assert.Equal(t, "github.event_name == 'pull_request'", check.If, "the polling check runs on a pull request alone")
	assert.Equal(t, pullRequestBase, check.Env["BASE_SHA"], "the polling check compares the tree with the pull request's base commit")
	assert.Equal(t, "ci", check.WorkingDirectory, "the polling check runs from the ci module, whose go.mod names its block-kit")
	assert.Nil(t, check.ContinueOnError, "a failed polling check fails the job")
	assert.EqualValues(t, 0, steps[0].With["fetch-depth"], "the test job checks out the full history, so the base commit is present")
}

// The ci module's go.mod alone names the check's block-kit release, so no workflow names it and a block requiring this kit inherits none of it.
func TestCI_ThePollingCheckRunsAtTheReleaseTheCIModuleRequires(t *testing.T) {
	ciMod, err := os.ReadFile("ci/go.mod")
	require.NoError(t, err, "the ci module holds the polling check")
	rootMod, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	workflows, err := filepath.Glob(".github/workflows/*.y*ml")
	require.NoError(t, err)
	require.NotEmpty(t, workflows)

	toolLine := regexp.MustCompile(`(?m)^\s*(?:tool\s+)?` + regexp.QuoteMeta(pollingCheckTool) + `\s*$`)
	required := regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/mitstack-eu/block-kit v(\d+)\.(\d+)\.(\d+)\b`).FindSubmatch(ciMod)

	assert.Regexp(t, toolLine, string(ciMod), "ci/go.mod declares checkpolls as a tool")
	require.NotNil(t, required, "ci/go.mod requires github.com/mitstack-eu/block-kit")
	version := make([]int, 3)
	for i := range version {
		version[i], _ = strconv.Atoi(string(required[i+1]))
	}
	assert.GreaterOrEqual(t, slices.Compare(version, firstKitWithPollingCheck), 0,
		"ci/go.mod requires block-kit v%d.%d.%d, want v0.49.1 or later, the first release that holds checkpolls", version[0], version[1], version[2])
	assert.NotRegexp(t, toolLine, string(rootMod), "the kit's own go.mod carries no checkpolls tool, so a block requiring the kit inherits no block-kit from it")
	for _, path := range workflows {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(body), pollingCheckTool+"@", "%s names no version of checkpolls, which ci/go.mod alone holds", path)
	}
}

func TestCI_ComponentsJobJudgesTheSignaturePolicyReachingOnlyTheHostsItsEgressLimitAdmitsAndAgainWithoutSigstore(t *testing.T) {
	const (
		tests  = "components/admission/tests/mitstack-verify-image-signatures"
		proxy  = "HTTPS_PROXY=http://127.0.0.1:9"
		hosts  = "sed -n 's/^ *- matchName: //p' " + tests + "/egress.yaml"
		system = "kyverno apply " + tests + "/policy.yaml --resource " + tests + "/system.yaml"
	)
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)
	var ci struct {
		Jobs map[string]struct {
			Steps []ciStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ci))
	var admitted, unreachable, systemPods int
	for _, step := range ci.Jobs["components"].Steps {
		behindTheProxy := strings.Contains(step.Run, proxy) && strings.Contains(step.Run, hosts) && strings.Contains(step.Run, `NO_PROXY="$hosts"`)
		switch {
		case behindTheProxy && strings.Contains(step.Run, "grep -v sigstore") && strings.Contains(step.Run, "kyverno test "+tests+" -f sigstore-unreachable.yaml"):
			unreachable++
		case behindTheProxy && strings.HasSuffix(strings.TrimSpace(step.Run), "kyverno test "+tests):
			admitted++
		case strings.Contains(step.Run, system) && strings.Contains(step.Run, "pass: 0, fail: 0, warn: 0, error: 0, skip: 0"):
			systemPods++
		}
	}

	assert.Equal(t, 1, admitted, "steps that run the signature policy's test behind a proxy no host answers, with only the egress limit's hosts exempt")
	assert.Equal(t, 1, unreachable, "steps that run it again with Sigstore's host no longer exempt, against sigstore-unreachable.yaml")
	assert.Equal(t, 1, systemPods, "steps that apply the policy to the pods in mitstack-node and kube-system and want no result")
}

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
