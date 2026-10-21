package components

import (
	_ "embed"
	"fmt"
	"net"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

// The loopback ports the node-local Collector listens on, for Talos's service logs and its kernel log.
const (
	ServiceLogPort = 5170
	KernelLogPort  = 5171
)

// LogExport names the OTLP receiver every node's logs go to, and its zero value names none.
type LogExport struct {
	// Endpoint is the receiver's host:port for OTLP over gRPC.
	Endpoint string
	// CA is the PEM certificate the Collector verifies the receiver against.
	CA []byte
}

// IsZero reports whether e names no receiver, so the cluster keeps its logs on its nodes.
func (e LogExport) IsZero() bool {
	return e.Endpoint == "" && len(e.CA) == 0
}

// logCollectorImage is the pinned opentelemetry-collector-contrib release, by digest.
var logCollectorImage = Image{
	Name:   "docker.io/otel/opentelemetry-collector-contrib",
	Tag:    "0.163.1",
	Digest: "sha256:1175b03003053962012f5a231f5567f66a3e8fdb960c7835c0e48f7aa1555806",
}

//go:embed logs/collector.yaml
var collectorTemplate string

var collector = tmpl.Must("the node-local Collector", collectorTemplate)

// LogCollector renders the node-local Collector that exports the node's logs to e.
func LogCollector(e LogExport) (Manifest, error) {
	if err := e.validate(); err != nil {
		return Manifest{}, err
	}
	out, err := tmpl.Execute(collector, map[string]any{
		"CA":          string(e.CA),
		"Endpoint":    e.Endpoint,
		"Image":       logCollectorImage.Ref(),
		"ServicePort": ServiceLogPort,
		"KernelPort":  KernelLogPort,
	})
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Name: "mitstack-node-logs", Path: "components/logs/collector.yaml", Bytes: out}, nil
}

func (e LogExport) validate() error {
	host, port, err := net.SplitHostPort(e.Endpoint)
	if err != nil || host == "" || port == "" || strings.ContainsAny(e.Endpoint, " \n\"'") {
		return fmt.Errorf("log export endpoint %q is not host:port", e.Endpoint)
	}
	if err := check.PEMCertificate(e.CA); err != nil {
		return fmt.Errorf("log export CA for %s: %w", e.Endpoint, err)
	}
	return nil
}
