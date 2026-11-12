package rollout

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/siderolabs/talos/pkg/machinery/constants"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const (
	handoverSuffix = "-handover"
	handoverScript = tools.ScriptDir + "/handover.sh"
	// ArgoCDController is the account of Argo CD's application controller, which applies every component.
	ArgoCDController = "argocd-application-controller"
	// ArgoCDManager is the field manager Argo CD's application controller applies under.
	ArgoCDManager = "argocd-controller"
	// syncWaveAnnotation names the wave of a sync Argo CD applies a document in, each wave after the one before it
	// is applied and reads Healthy.
	syncWaveAnnotation = "argocd.argoproj.io/sync-wave"
	// ArgoCDTrackingAnnotation is the annotation Argo CD gives each object it applies, naming the Application.
	ArgoCDTrackingAnnotation = "argocd.argoproj.io/tracking-id"
	// adoptionTimeout bounds the Job's wait for Argo CD to apply each object, which its own sync does within the same
	// pass that creates the Job.
	adoptionTimeout = "120s"
)

//go:embed handover.yaml
var handoverTemplate string

var handover = tmpl.Must("a component's handover Job", handoverTemplate)

// handoverJob names the Job that hands the component's objects over.
func (c carried) handoverJob() object {
	return object{Group: "batch", Kind: "Job", Namespace: ArgoCDNamespace, Name: c.app + handoverSuffix}
}

// wave is the wave Argo CD applies the document in, which is zero for one that names none, as Argo CD reads it.
func (h documentHead) wave() int {
	wave, _ := strconv.Atoi(h.Metadata.Annotations[syncWaveAnnotation])
	return wave
}

// lastWave is the last wave any of heads syncs in, below 0 when every one does, for heads that are not empty. The
// handover runs in it and in no later one, because Argo CD starts a later wave only once every object reads Healthy, and
// an object a dropped field keeps unhealthy would then block it.
func lastWave(heads []documentHead) int {
	last := heads[0].wave()
	for _, head := range heads[1:] {
		last = max(last, head.wave())
	}
	return last
}

// stub names one object and states none of its fields.
type stub struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   stubMeta `json:"metadata"`
}

type stubMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// renderHandover renders the Job that gives up Talos's fields on each object heads state, to run in wave.
func (c carried) renderHandover(heads []documentHead, wave int, toolsImage string) ([]byte, error) {
	list := struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Items      []stub `json:"items"`
	}{APIVersion: "v1", Kind: "List"}
	for _, head := range heads {
		list.Items = append(list.Items, stub{APIVersion: head.APIVersion, Kind: head.Kind,
			Metadata: stubMeta{Name: head.Metadata.Name, Namespace: head.Metadata.Namespace}})
	}
	objects, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("the component %s's handover: %w", c.name, err)
	}
	job := c.handoverJob()
	return tmpl.Execute(handover, map[string]string{
		"Name": job.Name, "Namespace": job.Namespace, "Wave": strconv.Itoa(wave), "ServiceAccount": ArgoCDController,
		"Image": toolsImage, "Script": handoverScript, "Objects": string(objects),
		"TalosManager": constants.KubernetesFieldManagerName, "ArgoCDManager": ArgoCDManager,
		"ArgoCDAnnotation": ArgoCDTrackingAnnotation, "AdoptionTimeout": adoptionTimeout,
	})
}
