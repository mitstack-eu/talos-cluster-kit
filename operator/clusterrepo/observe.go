package clusterrepo

import (
	"context"
	"strings"

	"github.com/mitstack-eu/block-kit/contract"

	"github.com/mitstack-eu/talos-cluster-kit/operator"
)

// State is what reading the cluster repository found.
type State int

const (
	Unknown State = iota
	Present
	Unmarked
	Gone
)

// Observation is what reading the cluster repository found; Detail says why its state is unknown, and Access is the
// access it read with, nil when it could not dial.
type Observation struct {
	State     State
	Name, URL string
	Detail    string
	Access    *Access
}

func (o Observation) Describe() string {
	switch o.State {
	case Present:
		return "the cluster repository " + o.Name + " exists"
	case Unmarked:
		return "the repository " + o.Name + " does not carry this subscription's mark, so it is not the cluster's"
	case Gone:
		return "the cluster repository " + o.Name + " does not exist"
	default:
		return o.Detail
	}
}

// Absent is whether no repository of this subscription's is there, since an unmarked one is not the cluster's.
func (o Observation) Absent() bool { return o.State == Gone || o.State == Unmarked }

// Observe leaves the repository unknown, with no GitHub call, when the request cannot name it or reach GitHub.
func Observe(ctx context.Context, d contract.Desired, dial DialFunc) (Observation, error) {
	appName := d.Params["appName"]
	if appName == "" {
		return Observation{Name: "(unnamed)", Detail: `the cluster repository cannot be named: params["appName"] is empty`}, nil
	}
	obs := Observation{Name: For(d.Secrets["github"]["owner"], appName).FullName()}
	a, err := dial(d, appName)
	if err != nil {
		obs.Detail = "the cluster repository cannot be read: " + operator.RefusalDetail(err)
		return obs, nil
	}
	obs.Name, obs.URL, obs.Access = a.Repo.FullName(), a.Repo.URL(), a
	read, err := a.Read(ctx)
	if err != nil {
		return obs, err
	}
	switch {
	case !read.Exists:
		obs.State = Gone
	case !read.Marked:
		obs.State = Unmarked
	default:
		obs.State = Present
	}
	return obs, nil
}

// RepositoryObserver reads the cluster repository from GitHub alone; one without the subscription's mark is not the
// cluster's. Check, when set, names each way a marked repository still falls short, which reads it absent.
type RepositoryObserver struct {
	Dial  DialFunc
	Check func(ctx context.Context, d contract.Desired, a *Access) ([]string, error)
}

func (o RepositoryObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	repo, err := Observe(ctx, r.Desired, o.Dial)
	if err != nil {
		return contract.Observation{}, err
	}
	switch {
	case repo.State == Present:
		return o.present(ctx, r.Desired, repo)
	case repo.Absent():
		return contract.Observation{State: contract.Absent, StatusDetail: repo.Describe()}, nil
	default:
		return operator.Unknown(repo.Describe()), nil
	}
}

func (o RepositoryObserver) present(ctx context.Context, d contract.Desired, repo Observation) (contract.Observation, error) {
	if o.Check != nil {
		problems, err := o.Check(ctx, d, repo.Access)
		if err != nil {
			return contract.Observation{}, err
		}
		if len(problems) > 0 {
			return contract.Observation{State: contract.Absent, StatusDetail: strings.Join(problems, "; ")}, nil
		}
	}
	return contract.Observation{State: contract.Present, Outputs: map[string]string{"repo": repo.Name}, StatusDetail: repo.Describe()}, nil
}

// SeedObserver reads the seed present while the marked cluster repository holds it.
type SeedObserver struct {
	Dial DialFunc
}

func (o SeedObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	repo, err := Observe(ctx, r.Desired, o.Dial)
	if err != nil {
		return contract.Observation{}, err
	}
	switch repo.State {
	case Gone, Unmarked:
		return contract.Observation{State: contract.Absent, StatusDetail: repo.Describe()}, nil
	case Unknown:
		return operator.Unknown(repo.Describe()), nil
	}
	held, err := repo.Access.HoldsSeed(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	if !held {
		return contract.Observation{State: contract.Absent, StatusDetail: "the cluster repository " + repo.Name + " holds no " + SeedMarkerFile}, nil
	}
	return contract.Observation{State: contract.Present, StatusDetail: "the cluster repository " + repo.Name + " holds the seed"}, nil
}
