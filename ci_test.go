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

// GitHub's hosted runners are free for a public repository, so every job of the kit's own CI runs on one.
func TestCI_EveryJobRunsOnGitHubsHostedUbuntuRunner(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)
	var ci struct {
		Jobs map[string]struct {
			RunsOn any `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ci))
	require.NotEmpty(t, ci.Jobs, "the CI workflow has jobs")

	for name, job := range ci.Jobs {
		assert.Equal(t, "ubuntu-latest", job.RunsOn, "the %s job runs on GitHub's hosted ubuntu-latest", name)
	}
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

func TestCI_ToolsImageJobShipsAContainerLogLineThroughThePinnedCollector(t *testing.T) {
	const run = "TOOLS_IMAGE=talos-tools:ci go test -tags apiserver,collector -count=1 -run '^TestCollector_' ."
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)
	var ci struct {
		Jobs map[string]struct {
			Steps []ciStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ci))
	steps := ci.Jobs["tools-image"].Steps

	at := slices.IndexFunc(steps, func(s ciStep) bool { return s.Run == run })

	require.GreaterOrEqual(t, at, 0, "the tools image job, the one with Docker and the API server's images, runs `%s`", run)
	assert.Equal(t, "${{ !cancelled() }}", steps[at].If, "the Collector runs even when an earlier check in the job failed")
	assert.Nil(t, steps[at].ContinueOnError, "a failed Collector test fails the job")
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

func ciJobSteps(t *testing.T, job string) []ciStep {
	t.Helper()
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)
	var ci struct {
		Jobs map[string]struct {
			Steps []ciStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ci))
	require.NotEmpty(t, ci.Jobs[job].Steps, "the CI workflow has a %s job", job)
	return ci.Jobs[job].Steps
}

// The root's go test never reaches a nested module, so the test job vets and tests the operator module from its own directory.
func TestCI_TestJobVetsAndTestsTheOperatorModule(t *testing.T) {
	steps := ciJobSteps(t, "test")

	for _, run := range []string{"go vet ./...", "go test -race ./..."} {
		at := slices.IndexFunc(steps, func(s ciStep) bool { return s.Run == run && s.WorkingDirectory == "operator" })

		require.GreaterOrEqual(t, at, 0, "the test job runs `%s` in the operator module", run)
		assert.Nil(t, steps[at].ContinueOnError, "a failed `%s` fails the job", run)
	}
}

// A block requires the operator module by its own tag, so each release tags it at the commit the kit's tag names.
func TestCI_ReleaseTagsTheOperatorModuleAtTheKitsReleaseCommit(t *testing.T) {
	steps := ciJobSteps(t, "release")

	at := slices.IndexFunc(steps, func(s ciStep) bool { return strings.Contains(s.Run, `git tag "$TAG"`) })

	require.GreaterOrEqual(t, at, 0, "the release job tags the kit's release")
	run := steps[at].Run
	assert.Contains(t, run, `git tag "operator/$TAG"`, "the release tags the operator module beside the kit")
	assert.Contains(t, run, `git push --atomic origin "refs/tags/$TAG" "refs/tags/operator/$TAG"`, "one atomic push carries both tags, so a rerun finds neither when the push failed")
}

// The robot cluster module is nested in the operator module, whose ./... skips it, so the test job vets and tests it from its own directory.
func TestCI_TestJobVetsAndTestsTheRobotClusterModule(t *testing.T) {
	steps := ciJobSteps(t, "test")

	for _, run := range []string{"go vet ./...", "go test -race ./..."} {
		at := slices.IndexFunc(steps, func(s ciStep) bool { return s.Run == run && s.WorkingDirectory == "operator/robotcluster" })

		require.GreaterOrEqual(t, at, 0, "the test job runs `%s` in the robot cluster module", run)
		assert.Nil(t, steps[at].ContinueOnError, "a failed `%s` fails the job", run)
	}
}

// An operator requires the robot cluster module by its own tag, so each release tags it in the same atomic push as the kit.
func TestCI_ReleaseTagsTheRobotClusterModuleInTheSamePushAsTheKit(t *testing.T) {
	steps := ciJobSteps(t, "release")

	at := slices.IndexFunc(steps, func(s ciStep) bool { return strings.Contains(s.Run, `git tag "$TAG"`) })

	require.GreaterOrEqual(t, at, 0, "the release job tags the kit's release")
	run := steps[at].Run
	assert.Contains(t, run, `git tag "operator/robotcluster/$TAG"`, "the release tags the robot cluster module beside the kit")
	assert.Contains(t, run, `git push --atomic origin "refs/tags/$TAG" "refs/tags/operator/$TAG" "refs/tags/operator/robotcluster/$TAG"`,
		"one atomic push carries all three tags, so a rerun finds none when the push failed")
}

// hcloud-talos-cluster builds on block-kit v0.40.0, so a higher floor here would move its block-kit with no change of its own.
func TestOperatorModule_RequiresBlockKitAtTheOldestReleaseATalosClusterOperatorBuildsOnAndNotTheKitItself(t *testing.T) {
	mod, err := os.ReadFile("operator/go.mod")
	require.NoError(t, err, "the operator module has its own go.mod")

	required := regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/mitstack-eu/block-kit (v\S+)`).FindSubmatch(mod)

	require.NotNil(t, required, "operator/go.mod requires github.com/mitstack-eu/block-kit")
	assert.Equal(t, "v0.40.0", string(required[1]), "the block-kit release operator/go.mod requires")
	assert.NotRegexp(t, `github\.com/mitstack-eu/talos-cluster-kit\s+v`, string(mod),
		"the operator module requires no release of the kit, so requiring it moves no block's kit")
}
