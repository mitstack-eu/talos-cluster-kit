// Package pools reads a Talos cluster block's node pools as the catalogue passes them, plans a Cloud pool's servers
// against what Hetzner lists, and renders a dedicated pool's workers. It holds no Robot API code.
package pools

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

// Pools is a cluster's two node pools; a pool with no entry and no input is empty.
type Pools struct {
	Dedicated []Dedicated
	Cloud     CloudPool
}

// Dedicated is a Robot server the org rents under RobotAccount, installed as a worker.
type Dedicated struct {
	ServerNumber int64
	RobotAccount string
}

// CloudPool is a Cloud pool of between Min and Max servers, spread over Locations in turn.
type CloudPool struct {
	// ServerTypes are in order of preference.
	ServerTypes []string
	Locations   []string
	Min, Max    int
}

const (
	inputDedicated = "dedicatedPool"
	inputTypes     = "cloudPoolServerTypes"
	inputLocations = "cloudPoolLocations"
	inputMin       = "cloudPoolMin"
	inputMax       = "cloudPoolMax"
)

// Parse reads and checks the pool inputs, answering every one that breaks its rule in one error.
func Parse(params map[string]string) (Pools, error) {
	var p Pools
	var errs []error
	var err error
	if p.Dedicated, err = parseDedicated(params[inputDedicated]); err != nil {
		errs = append(errs, err)
	}
	if p.Cloud, err = parseCloud(params); err != nil {
		errs = append(errs, err)
	}
	if errs != nil {
		return Pools{}, errors.Join(errs...)
	}
	return p, nil
}

func parseDedicated(value string) ([]Dedicated, error) {
	if value == "" {
		return nil, nil
	}
	var raw []map[string]string
	if err := json.Unmarshal([]byte(value), &raw); err != nil || raw == nil {
		return nil, fmt.Errorf("%s: want a JSON list of entries with a serverNumber and a robotAccount", inputDedicated)
	}
	var out []Dedicated
	var errs []error
	seen := map[int64]bool{}
	for i, entry := range raw {
		d, err := dedicatedEntry(entry)
		if err == nil && seen[d.ServerNumber] {
			err = fmt.Errorf("server %d is listed twice", d.ServerNumber)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s entry %d: %w", inputDedicated, i+1, err))
			continue
		}
		seen[d.ServerNumber] = true
		out = append(out, d)
	}
	return out, errors.Join(errs...)
}

func dedicatedEntry(entry map[string]string) (Dedicated, error) {
	var errs []error
	number := entry["serverNumber"]
	n, err := strconv.ParseInt(number, 10, 64)
	switch {
	case number == "":
		errs = append(errs, errors.New("serverNumber is required"))
	case err != nil || n < 1:
		errs = append(errs, fmt.Errorf("serverNumber %q is no Robot server number", number))
	}
	if entry["robotAccount"] == "" {
		errs = append(errs, errors.New("robotAccount is required"))
	}
	return Dedicated{ServerNumber: n, RobotAccount: entry["robotAccount"]}, errors.Join(errs...)
}

func parseCloud(params map[string]string) (CloudPool, error) {
	if !slices.ContainsFunc([]string{inputTypes, inputLocations, inputMin, inputMax}, func(in string) bool { return params[in] != "" }) {
		return CloudPool{}, nil
	}
	var pool CloudPool
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	pool.ServerTypes, err = oneOf(inputTypes, "server type", params[inputTypes], controlplane.ServerTypes)
	add(err)
	pool.Locations, err = oneOf(inputLocations, "location", params[inputLocations], controlplane.Locations)
	add(err)
	minSize, minErr := size(inputMin, "minimum", params[inputMin])
	maxSize, maxErr := size(inputMax, "maximum", params[inputMax])
	add(minErr)
	add(maxErr)
	pool.Min, pool.Max = minSize, maxSize
	if minErr == nil && maxErr == nil {
		add(checkBounds(pool))
	}
	return pool, errors.Join(errs...)
}

// oneOf reads a comma-separated list whose items each sit in allowed, once.
func oneOf(input, what, value string, allowed []string) ([]string, error) {
	var items []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s: a Cloud pool needs at least one %s", input, what)
	}
	var errs []error
	for i, item := range items {
		switch {
		case !slices.Contains(allowed, item):
			errs = append(errs, fmt.Errorf("%s: %q is no %s of %s", input, item, what, strings.Join(allowed, ", ")))
		case slices.Contains(items[:i], item):
			errs = append(errs, fmt.Errorf("%s: %q is listed twice", input, item))
		}
	}
	return items, errors.Join(errs...)
}

func size(input, what, value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("%s: a Cloud pool needs a %s", input, what)
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %q is no whole number of servers, 0 or more", input, value)
	}
	return n, nil
}

// checkBounds refuses unequal bounds, since only a pool of a set size can be kept until the scaling ships.
func checkBounds(pool CloudPool) error {
	switch {
	case pool.Max < pool.Min:
		return fmt.Errorf("%s %d is below %s %d", inputMax, pool.Max, inputMin, pool.Min)
	case pool.Max > pool.Min:
		return fmt.Errorf("%s %d differs from %s %d, and a pool of unequal bounds waits for the scaling MIT-5702 ships", inputMax, pool.Max, inputMin, pool.Min)
	}
	return nil
}
