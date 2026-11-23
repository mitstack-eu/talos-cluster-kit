package report

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Request is CPU in millicores and memory in bytes, as a pod asks them of a node or a node offers them to pods.
type Request struct {
	MilliCPU, Memory int64
}

func (r Request) Plus(o Request) Request {
	return Request{MilliCPU: r.MilliCPU + o.MilliCPU, Memory: r.Memory + o.Memory}
}

func (r Request) Minus(o Request) Request {
	return Request{MilliCPU: r.MilliCPU - o.MilliCPU, Memory: r.Memory - o.Memory}
}

// Holds is whether a node offering r has room for need in both CPU and memory.
func (r Request) Holds(need Request) bool {
	return need.MilliCPU <= r.MilliCPU && need.Memory <= r.Memory
}

func (r Request) String() string {
	return fmt.Sprintf("%dm CPU and %dMi", r.MilliCPU, r.Memory>>20)
}

func (r Request) max(o Request) Request {
	return Request{MilliCPU: max(r.MilliCPU, o.MilliCPU), Memory: max(r.Memory, o.Memory)}
}

// quantities reads the cpu and memory of a resource list, each zero when absent.
func quantities(list map[string]resource.Quantity) Request {
	cpu, memory := list["cpu"], list["memory"]
	return Request{MilliCPU: cpu.MilliValue(), Memory: memory.Value()}
}

// podRequests is what each container of a pod asks the scheduler for, as a pod's spec and the node report carry it.
type podRequests struct {
	InitContainers []containerRequests `json:"initContainers"`
	Containers     []containerRequests `json:"containers"`
}

type containerRequests struct {
	RestartPolicy *string `json:"restartPolicy"`
	Resources     struct {
		Requests map[string]resource.Quantity `json:"requests"`
	} `json:"resources"`
}

// reserved is what the scheduler reserves for the pod: its containers and sidecars together, or an init container
// with the sidecars started before it, whichever asks more, in each of CPU and memory.
func (p podRequests) reserved() Request {
	var sidecars, init Request
	for _, c := range p.InitContainers {
		asks := quantities(c.Resources.Requests)
		if c.RestartPolicy != nil && *c.RestartPolicy == "Always" {
			sidecars = sidecars.Plus(asks)
			init = init.max(sidecars)
			continue
		}
		init = init.max(asks.Plus(sidecars))
	}
	running := sidecars
	for _, c := range p.Containers {
		running = running.Plus(quantities(c.Resources.Requests))
	}
	return running.max(init)
}

// TemplatePod is the pod a pod template's spec makes, named name, with the placement it asks of a node and what the
// scheduler reserves for it.
func TemplatePod(name string, spec []byte) (WaitingPod, error) {
	var s struct {
		NodeSelector map[string]string `json:"nodeSelector"`
		Affinity     struct {
			NodeAffinity struct {
				Required *struct {
					NodeSelectorTerms []selectorTerm `json:"nodeSelectorTerms"`
				} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
			} `json:"nodeAffinity"`
		} `json:"affinity"`
		podRequests
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		return WaitingPod{}, fmt.Errorf("the pod template of %s cannot be read: %w", name, err)
	}
	p := &placement{NodeSelector: s.NodeSelector}
	if s.Affinity.NodeAffinity.Required != nil {
		p.NodeSelectorTerms = s.Affinity.NodeAffinity.Required.NodeSelectorTerms
	}
	return WaitingPod{Name: name, placement: p, request: s.podRequests.reserved()}, nil
}
