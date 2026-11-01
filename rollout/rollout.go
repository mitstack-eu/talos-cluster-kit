// Package rollout gives a running hosting cluster a release's hosting policies through its cluster repository, where
// Argo CD applies, changes and prunes them, and reads each one back from the cluster's node report.
package rollout

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
)

const (
	// Application is the Argo CD Application that applies the policies, and the cluster's own Application applies it.
	Application     = "mitstack-hosting"
	ApplicationPath = "cluster/" + Application + ".yaml"
	PoliciesPath    = policiesDir + "/policies.yaml"
	policiesDir     = "hosting"
	markerNamespace = "argocd"
	markerDigestLen = 12
	synced          = "Synced"
	healthy         = "Healthy"
)

//go:embed application.yaml
var applicationTemplate string

var application = tmpl.Must("the hosting Application", applicationTemplate)

// Release is the hosting policies a kit release states.
type Release struct {
	Policies []components.Manifest
}

// Current is this release's hosting policies.
func Current() Release { return Release{Policies: components.HostingPolicies()} }

type File struct {
	Path  string
	Bytes []byte
}

// Files are what a block commits to the cluster repository, in order, the policies before the Application that
// applies them, so Argo CD never syncs an Application whose directory is empty.
func (r Release) Files(repository string) ([]File, error) {
	if err := openbaoinit.CheckRepository(repository); err != nil {
		return nil, fmt.Errorf("the hosting rollout: %w", err)
	}
	app, err := tmpl.Execute(application, map[string]string{
		"Name": Application, "RepoURL": components.RepositorySSHURL(repository), "Path": policiesDir,
	})
	if err != nil {
		return nil, err
	}
	return []File{{Path: PoliciesPath, Bytes: r.policies()}, {Path: ApplicationPath, Bytes: app}}, nil
}

// policies joins the release's manifests and a ConfigMap named for their digest, since Argo CD's list of what it
// applied names objects and no revision.
func (r Release) policies() []byte {
	var out bytes.Buffer
	for _, m := range r.Policies {
		out.Write(m.Bytes)
		if !bytes.HasSuffix(m.Bytes, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString("---\n")
	}
	sum := sha256.Sum256(out.Bytes())
	fmt.Fprintf(&out, "# Argo CD lists this ConfigMap only once it compared the policies above, whose digest it is named for.\n"+
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s-%s\n  namespace: %s\n",
		Application, hex.EncodeToString(sum[:])[:markerDigestLen], markerNamespace)
	return out.Bytes()
}

// Reading is what a node report reads back of a release.
type Reading struct {
	// Applied names each object read back, as its kind and its namespace/name.
	Applied []string
	// Problem says why the report does not read the release back, and is empty when it does.
	Problem string
}

type object struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (o object) String() string {
	if o.Namespace == "" {
		return o.Kind + " " + o.Name
	}
	return o.Kind + " " + o.Namespace + "/" + o.Name
}

type listedObject struct {
	object
	Sync string `json:"sync"`
}

type reportedApplication struct {
	Name, Sync, Health string
	// Resources is nil when the report left out what each Application applied, to stay within the platform's limit.
	Resources *[]listedObject
}

// ReadBack reads the release back from a node report: Argo CD's Application reads Synced and Healthy, and lists
// exactly the objects the release states, each Synced, so an object the release changed or dropped stands as stated.
func (r Release) ReadBack(report []byte) (Reading, error) {
	want, err := stated(r.policies())
	if err != nil {
		return Reading{}, err
	}
	var body struct {
		ArgoCD struct {
			Applications []reportedApplication `json:"applications"`
		} `json:"argocd"`
	}
	if json.Unmarshal(report, &body) != nil {
		return problem("the newest node report is not a node report's JSON"), nil
	}
	apps := body.ArgoCD.Applications
	i := slices.IndexFunc(apps, func(a reportedApplication) bool { return a.Name == Application })
	switch {
	case i < 0:
		return problem("the newest node report lists no Argo CD Application %s", Application), nil
	case apps[i].Resources == nil:
		return problem("the newest node report carries no list of the objects Argo CD's Application %s applied", Application), nil
	case apps[i].Sync != synced || apps[i].Health != healthy:
		return problem("Argo CD's Application %s reads sync %q and health %q", Application, apps[i].Sync, apps[i].Health), nil
	}
	return judge(want, *apps[i].Resources), nil
}

func problem(format string, args ...any) Reading {
	return Reading{Problem: fmt.Sprintf(format, args...)}
}

func judge(want []object, listed []listedObject) Reading {
	var applied, problems []string
	for _, l := range listed {
		switch {
		case !slices.Contains(want, l.object):
			problems = append(problems, l.object.String()+", which the release does not state, is listed")
		case l.Sync != synced:
			problems = append(problems, fmt.Sprintf("%s reads %s", l.object, l.Sync))
		default:
			applied = append(applied, l.object.String())
		}
	}
	for _, o := range want {
		if !slices.ContainsFunc(listed, func(l listedObject) bool { return l.object == o }) {
			problems = append(problems, o.String()+" is not listed")
		}
	}
	if len(problems) > 0 {
		return problem("Argo CD's Application %s does not hold the release: %s", Application, strings.Join(problems, "; "))
	}
	slices.Sort(applied)
	return Reading{Applied: applied}
}

// stated is each object the policies file states, as Argo CD lists it.
func stated(policies []byte) ([]object, error) {
	var out []object
	decoder := yaml.NewDecoder(bytes.NewReader(policies))
	for {
		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name, Namespace string
			} `yaml:"metadata"`
		}
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the hosting policies: %w", err)
		}
		if doc.Kind == "" {
			continue
		}
		group, _, versioned := strings.Cut(doc.APIVersion, "/")
		if !versioned {
			group = ""
		}
		out = append(out, object{Group: group, Kind: doc.Kind, Namespace: doc.Metadata.Namespace, Name: doc.Metadata.Name})
	}
}
