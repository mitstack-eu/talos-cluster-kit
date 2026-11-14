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
	// callSeconds bounds each read inside a member and each eviction, so a call that hangs ends the run there.
	callSeconds = 30
	// deadlineSlack is what the Job's deadline leaves past every member's bounds, for the reads of the StatefulSet and
	// its members that open the run.
	deadlineSlack = 60
)

//go:embed openbao-rollout.yaml
var openBaoRolloutTemplate string

var openBaoRolloutJob = tmpl.Must("OpenBao's rollout Job", openBaoRolloutTemplate)

// renderOpenBaoRollout renders the Job that moves OpenBao's members onto the revision of the one StatefulSet heads
// state, which runs in that StatefulSet's namespace.
func renderOpenBaoRollout(heads []documentHead, toolsImage string) ([]byte, error) {
	var statefulSets []documentHead
	for _, head := range heads {
		if head.Kind == "StatefulSet" {
			statefulSets = append(statefulSets, head)
		}
	}
	if len(statefulSets) != 1 {
		return nil, fmt.Errorf("OpenBao's component states %d StatefulSets, want the one its rollout Job moves", len(statefulSets))
	}
	members := statefulSets[0].Metadata
	return tmpl.Execute(openBaoRolloutJob, map[string]string{
		"Name": openBaoRollout, "Namespace": members.Namespace, "App": components.OpenBaoName, "Image": toolsImage,
		"Script": openBaoRolloutScript, "StatefulSet": members.Name, "MemberTimeout": strconv.Itoa(memberSeconds) + "s",
		"CallTimeout": strconv.Itoa(callSeconds) + "s",
		// Each member takes a read of the leader, its eviction, its return and a read of its own.
		"Deadline": strconv.Itoa(components.OpenBaoMembers*(memberSeconds+3*callSeconds) + deadlineSlack),
	})
}
