package components

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
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
	// RunnerSweepTokenSecret is the Secret in the report's namespace the lease job also fills with the GitHub token, which
	// the node report mounts to remove the scale set's dead runners, since the report holds no right on any Secret.
	RunnerSweepTokenSecret = "mitstack-runner-sweep-token"
	// RunnerTokenName names the control plane's manifest of the runner token's Secret and the CronJob that leases it.
	RunnerTokenName = "mitstack-runner-token"

	runnersName          = "mitstack-runners"
	runnerControllerName = "gha-runner-scale-set-controller"
	runnerScaleSetName   = "mitstack-runner-scale-set"
	runnerControllerPath = "gha-runner-scale-set-controller/gha-runner-scale-set-controller.yaml"
	runnerPoliciesDir    = "runners/policies"
	githubURL            = "https://github.com/"
)

var (
	//go:embed runners/runners.yaml runners/policies gha-runner-scale-set-controller
	runnerFiles embed.FS
	//go:embed runners/scale-set.yaml
	scaleSetTemplate string
)

var scaleSet = tmpl.Must("the runner scale set", scaleSetTemplate)

// containerSize is the CPU and memory a container requests and is limited to, equal so its pod is Guaranteed.
type containerSize struct{ CPU, Memory string }

// The sizes of a job pod's containers, the same for every block, give the runner a whole core the static CPU manager
// keeps for it alone and fit one job on a cx23, the smallest server a Cloud pool may list.
var (
	jobRunnerSize = containerSize{CPU: "1000m", Memory: "1536Mi"}
	jobDindSize   = containerSize{CPU: "450m", Memory: "1024Mi"}
	jobInitSize   = containerSize{CPU: "100m", Memory: "128Mi"}
)

// dockerBridges are the interfaces Istio reroutes out of a job's pod, since its ambient redirect would otherwise catch
// their traffic as inbound: Docker's default bridge, and the pattern matching each custom network's bridge.
var dockerBridges = []string{"docker0", "br-+"}

// controllerAccountRe matches a pod's serviceAccountName with its value, which a CRD schema's bare key never has.
var controllerAccountRe = regexp.MustCompile(`(?m)^[ \t]+serviceAccountName:[ \t]+(\S+)[ \t]*$`)

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
	// IsolateJobs runs each job's pod in its own user namespace with an unprivileged Docker daemon; false runs the
	// daemon privileged, sharing its node's users.
	IsolateJobs bool
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

// WithRunnerServices adds ARC's controller and the runners' namespace to the served manifests and swaps in the pod
// policies that skip RunnersNamespace, so a gateway can serve them without the scale set.
func (s Set) WithRunnerServices() Set {
	if slices.ContainsFunc(s.Served, func(m Manifest) bool { return m.Name == runnersName }) {
		return s
	}
	s.Served = append(slices.Clone(s.Served),
		runnerManifest(runnersName, "runners/runners.yaml"),
		runnerManifest(runnerControllerName, runnerControllerPath),
	)
	variants := runnerPolicyVariants()
	s.Inline = slices.Clone(s.Inline)
	for i, m := range s.Inline {
		if slices.Contains(variants, m.Name) {
			s.Inline[i] = runnerManifest(m.Name, runnerPoliciesDir+"/"+m.Name+".yaml")
		}
	}
	return s
}

// runnerPolicyVariants names each pod policy whose variant skipping RunnersNamespace scripts/render-components.sh
// writes, so the script's list is the one source.
func runnerPolicyVariants() []string {
	entries, err := fs.ReadDir(runnerFiles, runnerPoliciesDir)
	if err != nil {
		panic(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSuffix(entry.Name(), ".yaml"))
	}
	return names
}

// WithRunners is the set with the runner services and the scale set rendered inline from the inputs.
func (s Set) WithRunners(in RunnerInputs) (Set, error) {
	if err := in.Validate(); err != nil {
		return Set{}, fmt.Errorf("the runner scale set: %w", err)
	}
	controller, err := runnerControllerImage()
	if err != nil {
		return Set{}, err
	}
	account, err := runnerControllerAccount()
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
		"ControllerAccount": account,
		"BurstLabel":        BurstLabel,
		"ControlPlaneLabel": ControlPlaneLabel,
		"RunnerSize":        jobRunnerSize,
		"DindSize":          jobDindSize,
		"InitSize":          jobInitSize,
		"IsolateJobs":       in.IsolateJobs,
		"DockerBridges":     strings.Join(dockerBridges, ","),
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
	slices.SortFunc(images, func(a, b Image) int { return strings.Compare(a.Ref(), b.Ref()) })
	images = slices.Compact(images)
	if len(images) != 1 {
		refs := make([]string, len(images))
		for i, image := range images {
			refs[i] = image.Ref()
		}
		return Image{}, fmt.Errorf("ARC's controller manifest names %d images, want one: %s", len(images), strings.Join(refs, ", "))
	}
	return images[0], nil
}

// runnerControllerAccount is the ServiceAccount the rendered controller runs as, which the scale set's manager Role is
// bound to.
func runnerControllerAccount() (string, error) {
	m := runnerManifest(runnerControllerName, runnerControllerPath)
	matches := controllerAccountRe.FindAllSubmatch(m.Bytes, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("ARC's controller manifest %s names %d pod service accounts, want one", m.Path, len(matches))
	}
	return string(matches[0][1]), nil
}

// runnerManifest panics on a file the embed directive leaves out, which any test of the runner services reaches first.
func runnerManifest(name, file string) Manifest {
	data, err := runnerFiles.ReadFile(file)
	if err != nil {
		panic(err)
	}
	return Manifest{Name: name, Path: "components/" + file, Bytes: data}
}
