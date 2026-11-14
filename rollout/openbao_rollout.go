package rollout

import (
	_ "embed"
	"fmt"
	"strconv"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const (
	// openBaoRollout names the Job and the account the OpenBao chart's extraObjects give it, in
	// components/render/openbao-values.yaml.
	openBaoRollout       = "mitstack-openbao-rollout"
	openBaoRolloutScript = tools.ScriptDir + "/openbao-rollout.sh"
	// memberSeconds bounds one member's return after its eviction: its preStop hook and grace period, its start on its
	// own node, a new image's pull, and its unseal.
	memberSeconds = 300
	// callSeconds bounds each read inside a member, each wait for the disruption budget to allow a disruption, and each
	// eviction, so a call that hangs ends the run there.
	callSeconds = 30
	// deadlineSlack is what the Job's deadline leaves past every member's bounds, for the reads of the StatefulSet and
	// its members that open the run.
	deadlineSlack = 60
)

//go:embed openbao-rollout.yaml
var openBaoRolloutTemplate string

var openBaoRolloutJob = tmpl.Must("OpenBao's rollout Job", openBaoRolloutTemplate)

// renderOpenBaoRollout renders the Job that moves OpenBao's members onto the revision of the one StatefulSet heads
// state, through the one disruption budget they state, which runs in that StatefulSet's namespace.
func renderOpenBaoRollout(heads []documentHead, toolsImage string) ([]byte, error) {
	members, err := onlyHead(heads, "StatefulSet", "the members its rollout Job moves")
	if err != nil {
		return nil, err
	}
	budget, err := onlyHead(heads, "PodDisruptionBudget", "the disruption budget its rollout Job waits on")
	if err != nil {
		return nil, err
	}
	return tmpl.Execute(openBaoRolloutJob, map[string]string{
		"Name": openBaoRollout, "Namespace": members.Metadata.Namespace, "App": components.OpenBaoName, "Image": toolsImage,
		"Script": openBaoRolloutScript, "StatefulSet": members.Metadata.Name, "DisruptionBudget": budget.Metadata.Name,
		"MemberTimeout": strconv.Itoa(memberSeconds) + "s", "CallTimeout": strconv.Itoa(callSeconds) + "s",
		// Each member takes a read of the leader, a wait for the budget, its eviction, its return and a read of its own.
		"Deadline": strconv.Itoa(components.OpenBaoMembers*(memberSeconds+4*callSeconds) + deadlineSlack),
	})
}

// onlyHead returns the one object of kind that heads state, which OpenBao's rollout Job reads as what.
func onlyHead(heads []documentHead, kind, what string) (documentHead, error) {
	var found []documentHead
	for _, head := range heads {
		if head.Kind == kind {
			found = append(found, head)
		}
	}
	if len(found) != 1 {
		return documentHead{}, fmt.Errorf("OpenBao's component states %d %ss, want the one that is %s", len(found), kind, what)
	}
	return found[0], nil
}
