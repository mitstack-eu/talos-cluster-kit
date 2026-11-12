package rollout

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/constants"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

const (
	handoverSuffix = "-handover"
	handoverScript = tools.ScriptDir + "/handover.sh"
	// argoCDController is the account of Argo CD's application controller, which applies every component.
	argoCDController = "argocd-application-controller"
	// argoCDManager is the field manager Argo CD's application controller applies under.
	argoCDManager = "argocd-controller"
)

//go:embed handover.yaml
var handoverTemplate string

var handover = tmpl.Must("a component's handover Job", handoverTemplate)

// handoverJob is the Job that ends the component's sync, as Argo CD lists it while it has not succeeded.
func (c carried) handoverJob() object {
	return object{Group: "batch", Kind: "Job", Namespace: argoCDNamespace, Name: c.app + handoverSuffix}
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

// renderHandover renders the Job that gives up Talos's fields on each object heads state, ending with a separator.
func (c carried) renderHandover(heads []documentHead, toolsImage string) ([]byte, error) {
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
		"Name": job.Name, "Namespace": job.Namespace, "ServiceAccount": argoCDController, "Image": toolsImage,
		"Script": handoverScript, "Objects": string(objects),
		"TalosManager": constants.KubernetesFieldManagerName, "ArgoCDManager": argoCDManager,
	})
}
