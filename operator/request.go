package operator

import (
	"context"
	"errors"
	"strings"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
)

// unreleasedToolsTag is the tag of the zero-digest placeholder tools.Image carries between the kit's releases.
const unreleasedToolsTag = ":unreleased@"

func HetznerToken(d contract.Desired) (string, error) {
	token := d.Secrets["hetzner"]["api_token"]
	if token == "" {
		return "", handler.Refuse("missing-hetzner-access",
			"the Hetzner connection is missing: secrets.hetzner.api_token is empty, so the cluster cannot be reached; connect a Hetzner Cloud project token for this app and retry")
	}
	return token, nil
}

// AppName refuses a request that names no app, since the cluster's resources and repository are named for it.
func AppName(d contract.Desired) (string, error) {
	name := d.Params["appName"]
	if name == "" {
		return "", handler.Refuse("missing-param", `the app name is required: params["appName"] is empty`)
	}
	return name, nil
}

// RefuseUnreleasedTools refuses the placeholder tools image an unreleased kit carries, which the node report runs from.
func RefuseUnreleasedTools(image string) error {
	if strings.Contains(image, unreleasedToolsTag) {
		return handler.Refuse("tools-image-unreleased",
			"the block is built against a talos-cluster-kit whose tools image %s is a placeholder, so the node report could not run and nothing was changed; build it against a released kit", image)
	}
	return nil
}

// RequestSecrets is every secret value the request carries, which a log line masks before it is written.
func RequestSecrets(d contract.Desired) []string {
	var out []string
	for _, group := range d.Secrets {
		for _, v := range group {
			out = append(out, v)
		}
	}
	for _, v := range d.SecretInputs {
		out = append(out, v)
	}
	for _, v := range d.OwnSecrets {
		out = append(out, v)
	}
	return out
}

func Unknown(detail string) contract.Observation {
	return contract.Observation{State: contract.Unknown, StatusDetail: detail}
}

// RefusalDetail drops the refused:<class> prefix, which a validate answer does not carry.
func RefusalDetail(err error) string {
	var refused *handler.Refused
	if errors.As(err, &refused) {
		return refused.Detail
	}
	return err.Error()
}

type ObserverFunc func(ctx context.Context, d contract.Desired) (contract.Observation, error)

func (f ObserverFunc) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	return f(ctx, r.Desired)
}
