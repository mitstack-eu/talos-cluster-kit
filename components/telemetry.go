package components

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

const (
	TelemetryName        = "mitstack-telemetry"
	telemetryOTLPService = TelemetryName + "-otlp"
	// telemetryOTLPPort is the Collector's default port for OTLP over HTTP.
	telemetryOTLPPort = "4318"
	// TelemetryOTLPEndpoint is the base URL a pod pushes OTLP to, through a Service that keeps each push on the pod's node.
	TelemetryOTLPEndpoint = "http://" + telemetryOTLPService + ".kube-system.svc.cluster.local:" + telemetryOTLPPort
)

// TelemetryExport is the OTLP backend the block requires, as that resource outputs it; the zero export runs no Collector.
type TelemetryExport struct {
	// Resource is the required resource's type, whose push token the lease job leases.
	Resource        string
	MetricsEndpoint string
	LogsEndpoint    string
	// TokenHeader is the header that carries the bare push token.
	TokenHeader string
}

func (e TelemetryExport) IsZero() bool {
	return e == TelemetryExport{}
}

var headerFieldName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (e TelemetryExport) validate() error {
	var errs []error
	for _, endpoint := range []struct{ signal, address string }{{"metrics", e.MetricsEndpoint}, {"logs", e.LogsEndpoint}} {
		if u, err := url.Parse(endpoint.address); err != nil || u.Scheme != "https" || u.Hostname() == "" {
			errs = append(errs, fmt.Errorf("the telemetry export's %s endpoint %q is no https address", endpoint.signal, endpoint.address))
		}
	}
	if e.TokenHeader == "" {
		errs = append(errs, errors.New("the telemetry export names no header for the push token"))
	} else if !headerFieldName.MatchString(e.TokenHeader) {
		errs = append(errs, fmt.Errorf("the telemetry export's push token header %q is no header name", e.TokenHeader))
	}
	if e.Resource == "" {
		errs = append(errs, errors.New("the telemetry export names no resource to lease the push token of"))
	}
	return errors.Join(errs...)
}

//go:embed telemetry/collector.yaml
var telemetryTemplate string

var telemetryCollector = tmpl.Must("the telemetry Collector", telemetryTemplate)

// TelemetryCollector renders the Collector that pushes the cluster's events, metrics and pod logs, and what its pods
// push to it, to e, tagged with subscription, and scrapes Argo CD's application controller and each OpenBao member the
// set runs.
func (s Set) TelemetryCollector(e TelemetryExport, subscription string) ([]Manifest, error) {
	if e.IsZero() {
		return nil, nil
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	data := map[string]any{
		"Image":           logCollectorImage.Ref(),
		"SubscriptionID":  subscription,
		"MetricsEndpoint": e.MetricsEndpoint,
		"LogsEndpoint":    e.LogsEndpoint,
		"TokenHeader":     e.TokenHeader,
		"OpenBaoMembers":  s.openBaoMembers(),
		"OTLPService":     telemetryOTLPService,
		"OTLPPort":        telemetryOTLPPort,
	}
	sums := map[string]string{}
	for _, which := range []string{"node", "cluster"} {
		config, err := tmpl.Execute(telemetryCollector.Lookup(which+"Config"), data)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(config)
		sums[which] = hex.EncodeToString(sum[:])
	}
	data["ConfigSHA256"] = sums
	out, err := tmpl.Execute(telemetryCollector, data)
	if err != nil {
		return nil, err
	}
	return []Manifest{{Name: TelemetryName, Path: "components/telemetry/collector.yaml", Bytes: out}}, nil
}

// openBaoMembers is the pod and host of each OpenBao member the set runs, or none for a set without OpenBao.
func (s Set) openBaoMembers() []map[string]string {
	if !slices.ContainsFunc(s.Served, func(m Manifest) bool { return m.Name == OpenBaoName }) {
		return nil
	}
	members := make([]map[string]string, OpenBaoMembers)
	for i := range members {
		pod, host := OpenBaoMember(i)
		members[i] = map[string]string{"Pod": pod, "Host": host}
	}
	return members
}
