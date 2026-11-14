package components

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

const TelemetryName = "mitstack-telemetry"

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

// TelemetryCollector renders the Collector pushing the cluster's events, metrics and app logs to e, tagged with subscription.
func TelemetryCollector(e TelemetryExport, subscription string) ([]Manifest, error) {
	if e.IsZero() {
		return nil, nil
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	out, err := tmpl.Execute(telemetryCollector, map[string]string{
		"Image":           logCollectorImage.Ref(),
		"SubscriptionID":  subscription,
		"MetricsEndpoint": e.MetricsEndpoint,
		"LogsEndpoint":    e.LogsEndpoint,
		"TokenHeader":     e.TokenHeader,
	})
	if err != nil {
		return nil, err
	}
	return []Manifest{{Name: TelemetryName, Path: "components/telemetry/collector.yaml", Bytes: out}}, nil
}
