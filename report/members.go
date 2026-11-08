package report

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/redundant"
)

// Member is one member of a redundant set with what a node report read of it, as the redundancy guard judges it.
type Member = redundant.Member

// Read is the node report a removal judges the rest of a set by: its body, and when its node began the read, which is
// the body's madeAt, else when the platform received it.
type Read struct {
	Body []byte
	At   time.Time
}

// GatewayReadAt is when a report's reading of the gateway at address was taken: its probedAt, when the outside probe
// read it, else the body's madeAt, else received, when the platform received the report.
func GatewayReadAt(body []byte, address netip.Addr, received time.Time) time.Time {
	var b struct {
		MadeAt   *time.Time `json:"madeAt"`
		Gateways []struct {
			Address  string     `json:"address"`
			ProbedAt *time.Time `json:"probedAt"`
		} `json:"gateways"`
	}
	if json.Unmarshal(body, &b) != nil {
		return received
	}
	for _, g := range b.Gateways {
		if g.Address == address.String() && g.ProbedAt != nil {
			return g.ProbedAt.UTC()
		}
	}
	if b.MadeAt != nil {
		return b.MadeAt.UTC()
	}
	return received
}

// openBaoNamespace is where the OpenBao members' pods run, each named for its member.
const openBaoNamespace = "openbao"

type members struct {
	Nodes []struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
	} `json:"nodes"`
	Etcd struct {
		Members []struct {
			Hostname string `json:"hostname"`
			Healthy  bool   `json:"healthy"`
		} `json:"members"`
	} `json:"etcd"`
	Pods []struct {
		Namespace string  `json:"namespace"`
		Name      string  `json:"name"`
		Node      *string `json:"node"`
	} `json:"pods"`
	OpenBao struct {
		Members []struct {
			Name        string `json:"name"`
			Initialized *bool  `json:"initialized"`
			Sealed      *bool  `json:"sealed"`
		} `json:"members"`
	} `json:"openbao"`
}

func (r Read) members() (members, error) {
	var m members
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return members{}, fmt.Errorf("the node report read at %s cannot be read: %w", r.At.UTC().Format(time.RFC3339), err)
	}
	return m, nil
}

func (r Read) reading(serving bool) *redundant.Reading {
	return &redundant.Reading{At: r.At, Serving: serving}
}

// EtcdMembers reads each etcd member by its node's hostname, serving when that node answered its status with no error.
func (r Read) EtcdMembers() ([]Member, error) {
	m, err := r.members()
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(m.Etcd.Members))
	for _, e := range m.Etcd.Members {
		out = append(out, Member{Name: e.Hostname, Reading: r.reading(e.Healthy)})
	}
	return out, nil
}

// Nodes reads each named node, serving when the report reads it Ready; a node the report does not list has no reading.
func (r Read) Nodes(names []string) ([]Member, error) {
	m, err := r.members()
	if err != nil {
		return nil, err
	}
	ready := map[string]bool{}
	for _, n := range m.Nodes {
		ready[n.Name] = n.Ready
	}
	out := make([]Member, 0, len(names))
	for _, name := range names {
		member := Member{Name: name}
		if isReady, listed := ready[name]; listed {
			member.Reading = r.reading(isReady)
		}
		out = append(out, member)
	}
	return out, nil
}

// OpenBaoMembers reads each OpenBao member, serving when it is initialized and unsealed, with the node its pod runs on.
func (r Read) OpenBaoMembers() ([]Member, map[string]string, error) {
	m, err := r.members()
	if err != nil {
		return nil, nil, err
	}
	out := make([]Member, 0, len(m.OpenBao.Members))
	for _, b := range m.OpenBao.Members {
		unsealed := b.Initialized != nil && *b.Initialized && b.Sealed != nil && !*b.Sealed
		out = append(out, Member{Name: b.Name, Reading: r.reading(unsealed)})
	}
	nodes := map[string]string{}
	for _, p := range m.Pods {
		if p.Namespace == openBaoNamespace && p.Node != nil {
			nodes[p.Name] = *p.Node
		}
	}
	return out, nodes, nil
}
