package components

import (
	"embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
)

const (
	// RunnersNamespace holds ARC's controller, the scale set's listener and every job's pod.
	RunnersNamespace = "mitstack-runners"
	// RunnerTokenSecret is the Secret the lease job fills with a GitHub token, which ARC reads as github_token.
	RunnerTokenSecret = "mitstack-runner-token"
	// RunnerTokenName names the control plane's manifest of the runner token's Secret and the CronJob that leases it.
	RunnerTokenName = "mitstack-runner-token"

	runnersName          = "mitstack-runners"
	runnerControllerName = "gha-runner-scale-set-controller"
	runnerScaleSetName   = "mitstack-runner-scale-set"
	runnerControllerPath = "gha-runner-scale-set-controller/gha-runner-scale-set-controller.yaml"
	runnerPoliciesDir    = "runners/policies/"
	githubURL            = "https://github.com/"
)

// runnerExemptPolicies are the pod policies a cluster with the runner services swaps for a variant that skips
// RunnersNamespace, which scripts/render-components.sh generates from each base policy.
var runnerExemptPolicies = []string{"mitstack-disallow-host-path", "mitstack-require-resource-limits", "mitstack-restrict-image-registries"}

var (
	//go:embed runners/runners.yaml runners/policies gha-runner-scale-set-controller
	runnerFiles embed.FS
	//go:embed runners/scale-set.yaml
	scaleSetTemplate string
)

var scaleSet = tmpl.Must("the runner scale set", scaleSetTemplate)

// RunnerInputs are what the org's one runner scale set takes from the operator that runs it.
type RunnerInputs struct {
	// Organization is the GitHub organisation login whose repositories the runners serve.
	Organization string
	// Group is the runner group the scale set registers into.
	Group string
	// ScaleSet names the scale set, which is also the label a job runs on.
	ScaleSet string
	// RunnerImage and DindImage are the digest-pinned actions-runner and Docker daemon images each job's pod runs.
	RunnerImage, DindImage string
	// MaxRunners is the most jobs that run at once, at least one.
	MaxRunners int
	// LeaseResource is the resource type whose GitHub token the lease job asks the platform for.
	LeaseResource string
}

// githubLogin matches a GitHub organisation login: letters, digits and single hyphens, at most 39 characters.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// Validate refuses inputs a scale set cannot run with, naming each field it refuses.
func (in RunnerInputs) Validate() error {
	var errs []error
	if !githubLogin.MatchString(in.Organization) {
		errs = append(errs, fmt.Errorf("the runner inputs' Organization %q is no GitHub organisation login", in.Organization))
	}
	for _, label := range []struct{ field, value string }{{"Group", in.Group}, {"ScaleSet", in.ScaleSet}} {
		if err := check.DNS1123Label(label.value); err != nil {
			errs = append(errs, fmt.Errorf("the runner inputs' %s: %w", label.field, err))
		}
	}
	for _, image := range []struct{ field, ref string }{{"RunnerImage", in.RunnerImage}, {"DindImage", in.DindImage}} {
		if _, err := ParseImage(image.ref); err != nil {
			errs = append(errs, fmt.Errorf("the runner inputs' %s: %w", image.field, err))
		}
	}
	if in.MaxRunners < 1 {
		errs = append(errs, fmt.Errorf("the runner inputs' MaxRunners is %d, want at least 1", in.MaxRunners))
	}
	if in.LeaseResource == "" {
		errs = append(errs, errors.New("the runner inputs' LeaseResource names no resource to lease the GitHub token of"))
	}
	return errors.Join(errs...)
}

// Images are the runner and dind images each job's pod starts, which the gateway's mirror holds.
func (in RunnerInputs) Images() ([]Image, error) {
	var images []Image
	for _, ref := range []string{in.RunnerImage, in.DindImage} {
		image, err := ParseImage(ref)
		if err != nil {
			return nil, fmt.Errorf("the runner images: %w", err)
		}
		images = append(images, image)
	}
	return images, nil
}

// WithRunnerServices adds ARC's controller and the runners' namespace to the served manifests and swaps each pod
// policy that skips local-path-storage for its variant that skips RunnersNamespace too, under the same name. A set
// that holds the services already is returned as it is.
func (s Set) WithRunnerServices() Set {
	if slices.ContainsFunc(s.Served, func(m Manifest) bool { return m.Name == runnersName }) {
		return s
	}
	s.Served = append(slices.Clone(s.Served),
		runnerManifest(runnersName, "runners/runners.yaml"),
		runnerManifest(runnerControllerName, runnerControllerPath),
	)
	s.Inline = slices.Clone(s.Inline)
	for i, m := range s.Inline {
		if slices.Contains(runnerExemptPolicies, m.Name) {
			s.Inline[i] = runnerManifest(m.Name, runnerPoliciesDir+m.Name+".yaml")
		}
	}
	return s
}

// WithRunners is the set with the runner services and the scale set in renders inline.
func (s Set) WithRunners(in RunnerInputs) (Set, error) {
	if err := in.Validate(); err != nil {
		return Set{}, fmt.Errorf("the runner scale set: %w", err)
	}
	controller, err := runnerControllerImage()
	if err != nil {
		return Set{}, err
	}
	out, err := tmpl.Execute(scaleSet, map[string]any{
		"Name":              in.ScaleSet,
		"Namespace":         RunnersNamespace,
		"ConfigURL":         githubURL + in.Organization,
		"TokenSecret":       RunnerTokenSecret,
		"Group":             in.Group,
		"MaxRunners":        in.MaxRunners,
		"RunnerImage":       in.RunnerImage,
		"DindImage":         in.DindImage,
		"ControllerImage":   controller.Ref(),
		"ControllerVersion": controller.Tag,
		"BurstLabel":        BurstLabel,
	})
	if err != nil {
		return Set{}, err
	}
	s = s.WithRunnerServices()
	s.Inline = append(slices.Clone(s.Inline), Manifest{Name: runnerScaleSetName, Path: "components/runners/scale-set.yaml", Bytes: out})
	return s, nil
}

// runnerControllerImage is the one image the rendered controller runs, which its listener runs too and whose tag is
// the release a scale set's version label must name.
func runnerControllerImage() (Image, error) {
	m := runnerManifest(runnerControllerName, runnerControllerPath)
	images, err := m.images()
	if err != nil {
		return Image{}, err
	}
	images = slices.CompactFunc(images, func(a, b Image) bool { return a == b })
	if len(images) != 1 {
		refs := make([]string, len(images))
		for i, image := range images {
			refs[i] = image.Ref()
		}
		return Image{}, fmt.Errorf("ARC's controller manifest names %d images, want one: %s", len(images), strings.Join(refs, ", "))
	}
	return images[0], nil
}

// runnerManifest panics on a file the embed directive leaves out, which any test of the runner services reaches first.
func runnerManifest(name, file string) Manifest {
	data, err := runnerFiles.ReadFile(file)
	if err != nil {
		panic(err)
	}
	return Manifest{Name: name, Path: "components/" + file, Bytes: data}
}
