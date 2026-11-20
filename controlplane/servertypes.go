package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// OrderableTypes picks the first of a list of server types Hetzner can order in a location, reading each location once.
type OrderableTypes struct {
	API     API
	offered map[string][]ServerType
}

// First is the first of types Hetzner reads as orderable in loc, refusing with server-unavailable when it reads none.
func (o *OrderableTypes) First(ctx context.Context, what string, types []string, loc string) (string, error) {
	offered, err := o.listed(ctx, what, types, loc)
	if err != nil {
		return "", err
	}
	if t := FirstFitting(offered, types, 0); t != "" {
		return t, nil
	}
	return "", refuse("server-unavailable",
		"Hetzner can order none of the %s's server types %s in %s now; retry once it sells one of them there again",
		what, strings.Join(types, ", "), loc)
}

// FirstHolding is the first of types Hetzner reads as orderable in loc whose disk holds image, since Hetzner creates no
// server from an image larger than its type's disk, refusing with server-unavailable, naming each type's disk, when
// none does.
func (o *OrderableTypes) FirstHolding(ctx context.Context, what string, types []string, loc string, image Snapshot) (string, error) {
	offered, err := o.listed(ctx, what, types, loc)
	if err != nil {
		return "", err
	}
	if t := FirstFitting(offered, types, image.DiskGB); t != "" {
		return t, nil
	}
	return "", refuse("server-unavailable",
		"Hetzner can order none of the %s's server types in %s now whose disk holds its image's %g GB (%s); retry once it sells one there again",
		what, loc, image.DiskGB, DescribeOffer(offered, types))
}

func (o *OrderableTypes) listed(ctx context.Context, what string, types []string, loc string) ([]ServerType, error) {
	if len(types) == 0 {
		return nil, fmt.Errorf("the %s names no server type", what)
	}
	return o.Offered(ctx, loc)
}

// Offered is the server types Hetzner can order in loc now, as read the first time this asks for loc.
func (o *OrderableTypes) Offered(ctx context.Context, loc string) ([]ServerType, error) {
	if offered, ok := o.offered[loc]; ok {
		return offered, nil
	}
	offered, err := o.API.AvailableServerTypes(ctx, loc)
	if err != nil {
		return nil, fmt.Errorf("reading the server types Hetzner can order in %s: %w", loc, err)
	}
	if o.offered == nil {
		o.offered = map[string][]ServerType{}
	}
	o.offered[loc] = offered
	return offered, nil
}

// FirstFitting is the first of types offered with a disk of at least diskGB, empty when none is.
func FirstFitting(offered []ServerType, types []string, diskGB float64) string {
	for _, name := range types {
		if t, ok := typeNamed(offered, name); ok && t.DiskGB >= diskGB {
			return name
		}
	}
	return ""
}

// DescribeOffer names the disk of each of types offered, and each other one as not on sale.
func DescribeOffer(offered []ServerType, types []string) string {
	described := make([]string, 0, len(types))
	for _, name := range types {
		if t, ok := typeNamed(offered, name); ok {
			described = append(described, fmt.Sprintf("%s has %g GB", name, t.DiskGB))
		} else {
			described = append(described, name+" is not on sale")
		}
	}
	return strings.Join(described, ", ")
}

func typeNamed(offered []ServerType, name string) (ServerType, bool) {
	i := slices.IndexFunc(offered, func(t ServerType) bool { return t.Name == name })
	if i < 0 {
		return ServerType{}, false
	}
	return offered[i], true
}
