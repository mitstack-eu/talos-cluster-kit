package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// OrderableTypes picks the first of a list of server types Hetzner can order in a location. It reads each location
// once, so every server one call creates there takes its type from the same read.
type OrderableTypes struct {
	API     API
	offered map[string][]string
}

// First is the first of types Hetzner reads as orderable in loc. It refuses with server-unavailable, naming what the
// server is, the list and loc, when Hetzner can order none of them there.
func (o *OrderableTypes) First(ctx context.Context, what string, types []string, loc string) (string, error) {
	if len(types) == 0 {
		return "", fmt.Errorf("the %s names no server type", what)
	}
	offered, ok := o.offered[loc]
	if !ok {
		var err error
		if offered, err = o.API.AvailableServerTypes(ctx, loc); err != nil {
			return "", fmt.Errorf("reading the server types Hetzner can order in %s: %w", loc, err)
		}
		if o.offered == nil {
			o.offered = map[string][]string{}
		}
		o.offered[loc] = offered
	}
	for _, t := range types {
		if slices.Contains(offered, t) {
			return t, nil
		}
	}
	return "", refuse("server-unavailable",
		"Hetzner can order none of the %s's server types %s in %s now; retry once it sells one of them there again",
		what, strings.Join(types, ", "), loc)
}
