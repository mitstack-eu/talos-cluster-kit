package components

import (
	_ "embed"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

const coreDNSName = "coredns"

//go:embed coredns/coredns.yaml
var coreDNSTemplate string

var coreDNSDeployment = tmpl.Must("the CoreDNS Deployment", coreDNSTemplate)

// CoreDNS renders the Deployment Talos creates for CoreDNS with one replica in each distinct location of locations,
// or nothing for a cluster in one location, which keeps the Deployment as Talos created it.
func CoreDNS(locations []string) ([]Manifest, error) {
	distinct := distinctZones(locations)
	if len(distinct) < 2 {
		return nil, nil
	}
	data, err := tmpl.Execute(coreDNSDeployment, map[string]any{"Replicas": len(distinct), "Image": coreDNSImage.Ref()})
	if err != nil {
		return nil, err
	}
	return []Manifest{{Name: coreDNSName, Path: "components/coredns/coredns.yaml", Bytes: data}}, nil
}
