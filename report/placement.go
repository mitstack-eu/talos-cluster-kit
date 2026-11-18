package report

import (
	"slices"
	"strconv"
)

// WaitingPod is a pod the scheduler found no node for, named as namespace/name, with the placement it asks of a node.
type WaitingPod struct {
	Name      string
	placement *placement
}

// placement is a pod's node selector and the terms of its required node affinity, as the node report copies them.
type placement struct {
	NodeSelector      map[string]string `json:"nodeSelector"`
	NodeSelectorTerms []selectorTerm    `json:"nodeSelectorTerms"`
}

type selectorTerm struct {
	MatchExpressions []requirement `json:"matchExpressions"`
	MatchFields      []requirement `json:"matchFields"`
}

type requirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// Admits reports whether the scheduler's node selection lets the pod run on a node carrying labels: every label of
// its node selector, and at least one of its terms, each of whose expressions the labels meet. A term naming a field
// admits no node, since the only field the scheduler matches is a node's name, which no node yet to be created has.
func (w WaitingPod) Admits(labels map[string]string) bool {
	p := w.placement
	if p == nil {
		return true
	}
	for key, value := range p.NodeSelector {
		if got, ok := labels[key]; !ok || got != value {
			return false
		}
	}
	if p.NodeSelectorTerms == nil {
		return true
	}
	return slices.ContainsFunc(p.NodeSelectorTerms, func(t selectorTerm) bool { return t.admits(labels) })
}

func (t selectorTerm) admits(labels map[string]string) bool {
	if len(t.MatchFields) > 0 || len(t.MatchExpressions) == 0 {
		return false
	}
	return !slices.ContainsFunc(t.MatchExpressions, func(r requirement) bool { return !r.metBy(labels) })
}

// metBy follows the node selector operators; an operator the scheduler does not know meets no node.
func (r requirement) metBy(labels map[string]string) bool {
	value, ok := labels[r.Key]
	switch r.Operator {
	case "In":
		return ok && slices.Contains(r.Values, value)
	case "NotIn":
		return !ok || !slices.Contains(r.Values, value)
	case "Exists":
		return ok
	case "DoesNotExist":
		return !ok
	case "Gt", "Lt":
		return ok && len(r.Values) == 1 && compares(value, r.Values[0], r.Operator)
	}
	return false
}

func compares(value, bound, operator string) bool {
	v, err := strconv.ParseInt(value, 10, 64)
	b, berr := strconv.ParseInt(bound, 10, 64)
	if err != nil || berr != nil {
		return false
	}
	if operator == "Gt" {
		return v > b
	}
	return v < b
}
