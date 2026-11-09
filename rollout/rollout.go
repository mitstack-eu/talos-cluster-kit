// Package rollout gives a running cluster a release's component set through its cluster repository, where one Argo CD
// Application per component applies, changes and prunes it, and reads each one back from the cluster's node report.
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
	"path"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
)

const (
	// ApplicationPrefix starts the name of each component's Application and of the file under cluster/ that holds it.
	ApplicationPrefix = "component-"
	ComponentsDir     = "components"
	manifestFile      = "manifest.yaml"
	clusterDir        = "cluster"
	markerNamespace   = "argocd"
	markerDigestLen   = 12
	synced            = "Synced"
	healthy           = "Healthy"
)

// LegacyFiles are what releases before the whole set's carriage committed for the hosting policies alone, which the
// carriage takes out in the commit that gives the policies their own Application.
var LegacyFiles = []string{"cluster/mitstack-hosting.yaml", "hosting/policies.yaml"}

//go:embed application.yaml
var applicationTemplate string

var application = tmpl.Must("a component's Application", applicationTemplate)

// Release is the component set a kit release carries to a running cluster.
type Release struct {
	Components []components.Manifest
}

type File struct {
	Path  string
	Bytes []byte
}

// carried is one component as the repository holds it: the documents it carries, then the ConfigMap named for their
// digest, since Argo CD's list of what it applied names objects and no revision.
type carried struct {
	name, app string
	bytes     []byte
}

func (c carried) dir() string  { return ComponentsDir + "/" + c.name }
func (c carried) file() string { return c.dir() + "/" + manifestFile }

func (c carried) applicationFile() string { return clusterDir + "/" + c.app + ".yaml" }

// Files are what a block commits to the cluster repository: each component's manifest, then the Applications that
// apply them, so Argo CD never syncs an Application whose directory is empty.
func (r Release) Files(repository string) ([]File, error) {
	if err := openbaoinit.CheckRepository(repository); err != nil {
		return nil, fmt.Errorf("the component rollout: %w", err)
	}
	set, err := r.carried()
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, 2*len(set))
	for _, c := range set {
		files = append(files, File{Path: c.file(), Bytes: c.bytes})
	}
	for _, c := range set {
		app, err := tmpl.Execute(application, map[string]string{
			"Name": c.app, "RepoURL": components.RepositorySSHURL(repository), "Path": c.dir(),
		})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: c.applicationFile(), Bytes: app})
	}
	return files, nil
}

// Retired is each path of held that the carriage committed for an earlier release and this one does not state, so the
// commit that lands this release takes it out; no other file of the repository is ever named.
func (r Release) Retired(held []string) ([]string, error) {
	set, err := r.carried()
	if err != nil {
		return nil, err
	}
	var stated []string
	for _, c := range set {
		stated = append(stated, c.file(), c.applicationFile())
	}
	var retired []string
	for _, p := range held {
		if ownedByCarriage(p) && !slices.Contains(stated, p) {
			retired = append(retired, p)
		}
	}
	return retired, nil
}

func ownedByCarriage(p string) bool {
	dir, base := path.Split(p)
	return strings.HasPrefix(p, ComponentsDir+"/") || slices.Contains(LegacyFiles, p) ||
		dir == clusterDir+"/" && strings.HasPrefix(base, ApplicationPrefix) && strings.HasSuffix(base, ".yaml")
}

// carriedCache keeps each component's carried bytes by its digest, since splitting the set's documents decodes every
// one of them and a block reads the set on every call.
var carriedCache sync.Map

func (r Release) carried() ([]carried, error) {
	var set []carried
	for _, m := range r.Components {
		if err := check.DNS1123Label(ApplicationPrefix + m.Name); err != nil {
			return nil, fmt.Errorf("the component %s cannot name an Application: %w", m.Name, err)
		}
		docs, err := carriedDocuments(m)
		if err != nil {
			return nil, err
		}
		if len(docs) == 0 {
			continue
		}
		set = append(set, withMarker(m.Name, docs))
	}
	return set, nil
}

func withMarker(name string, docs []byte) carried {
	c := carried{name: name, app: ApplicationPrefix + name}
	sum := sha256.Sum256(docs)
	marker := fmt.Sprintf("# Argo CD lists this ConfigMap only once it compared the documents above, whose digest it is named for.\n"+
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s-%s\n  namespace: %s\n",
		c.app, hex.EncodeToString(sum[:])[:markerDigestLen], markerNamespace)
	c.bytes = append(slices.Clip(docs), marker...)
	return c
}

func carriedDocuments(m components.Manifest) ([]byte, error) {
	key := m.Name + "\x00" + m.SHA256()
	if docs, ok := carriedCache.Load(key); ok {
		return docs.([]byte), nil
	}
	docs, err := keepCarried(m)
	if err != nil {
		return nil, err
	}
	carriedCache.Store(key, docs)
	return docs, nil
}

// keepCarried keeps each document of m but those that stay with the build: a Secret holding data, which carries a key
// the repository must never hold; a Job, which runs once and whose pod template no later sync can change; and a Helm
// or Argo CD hook, which Argo CD would run again on every sync. Each kept document ends with a separator.
func keepCarried(m components.Manifest) ([]byte, error) {
	var out bytes.Buffer
	for _, doc := range splitDocuments(m.Bytes) {
		var head documentHead
		if err := yaml.Unmarshal(doc, &head); err != nil {
			return nil, fmt.Errorf("the component %s: %w", m.Name, err)
		}
		if head.Kind == "" || head.staysWithBuild() {
			continue
		}
		out.Write(doc)
		if !bytes.HasSuffix(doc, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString("---\n")
	}
	return out.Bytes(), nil
}

type documentHead struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Data       map[string]any `yaml:"data"`
	StringData map[string]any `yaml:"stringData"`
}

var hookAnnotations = []string{"helm.sh/hook", "argocd.argoproj.io/hook"}

func (h documentHead) staysWithBuild() bool {
	switch {
	case h.Kind == "Secret" && len(h.Data)+len(h.StringData) > 0:
		return true
	case h.Kind == "Job" && strings.HasPrefix(h.APIVersion, "batch/"):
		return true
	}
	for _, a := range hookAnnotations {
		if _, ok := h.Metadata.Annotations[a]; ok {
			return true
		}
	}
	return false
}

// splitDocuments cuts a YAML stream at each line that is only a document marker, which a block scalar cannot hold at
// the start of a line.
func splitDocuments(stream []byte) [][]byte {
	var docs [][]byte
	var current []byte
	for line := range bytes.Lines(stream) {
		if marker := bytes.TrimRight(line, " \t\r\n"); bytes.Equal(marker, []byte("---")) {
			docs, current = appendDocument(docs, current), nil
			continue
		}
		current = append(current, line...)
	}
	return appendDocument(docs, current)
}

func appendDocument(docs [][]byte, doc []byte) [][]byte {
	if len(bytes.TrimSpace(doc)) == 0 {
		return docs
	}
	return append(docs, doc)
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

// ReadBack reads the release back from a node report: each component's Application reads Synced and Healthy, and lists
// exactly the objects the release states for it, each Synced, so an object the release changed or dropped stands as
// stated.
func (r Release) ReadBack(report []byte) (Reading, error) {
	set, err := r.carried()
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
	var applied, problems []string
	for _, c := range set {
		want, err := stated(c)
		if err != nil {
			return Reading{}, err
		}
		objects, missing := readApplication(body.ArgoCD.Applications, c.app, want)
		applied, problems = append(applied, objects...), append(problems, missing...)
	}
	if len(problems) > 0 {
		return problem("the newest node report does not read the release back: %s", strings.Join(problems, "; ")), nil
	}
	slices.Sort(applied)
	return Reading{Applied: applied}, nil
}

func problem(format string, args ...any) Reading {
	return Reading{Problem: fmt.Sprintf(format, args...)}
}

// readApplication is each object the report reads back of the Application named app, or why it does not.
func readApplication(apps []reportedApplication, app string, want []object) (applied, problems []string) {
	i := slices.IndexFunc(apps, func(a reportedApplication) bool { return a.Name == app })
	switch {
	case i < 0:
		return nil, []string{"it lists no Argo CD Application " + app}
	case apps[i].Resources == nil:
		return nil, []string{"it carries no list of the objects Argo CD's Application " + app + " applied"}
	case apps[i].Sync != synced || apps[i].Health != healthy:
		return nil, []string{fmt.Sprintf("Argo CD's Application %s reads sync %q and health %q", app, apps[i].Sync, apps[i].Health)}
	}
	for _, l := range *apps[i].Resources {
		switch {
		case !slices.Contains(want, l.object):
			problems = append(problems, fmt.Sprintf("%s lists %s, which the release does not state", app, l.object))
		case l.Sync != synced:
			problems = append(problems, fmt.Sprintf("%s lists %s %s", app, l.object, l.Sync))
		default:
			applied = append(applied, l.object.String())
		}
	}
	for _, o := range want {
		if !slices.ContainsFunc(*apps[i].Resources, func(l listedObject) bool { return l.object == o }) {
			problems = append(problems, fmt.Sprintf("%s does not list %s", app, o))
		}
	}
	return applied, problems
}

// statedCache keeps each component's objects by its carried digest, for the reason carriedCache does.
var statedCache sync.Map

// stated is each object a component's file states, as Argo CD lists it.
func stated(c carried) ([]object, error) {
	sum := sha256.Sum256(c.bytes)
	key := hex.EncodeToString(sum[:])
	if objects, ok := statedCache.Load(key); ok {
		return objects.([]object), nil
	}
	var out []object
	decoder := yaml.NewDecoder(bytes.NewReader(c.bytes))
	for {
		var head documentHead
		err := decoder.Decode(&head)
		if errors.Is(err, io.EOF) {
			statedCache.Store(key, out)
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the component %s: %w", c.name, err)
		}
		if head.Kind == "" {
			continue
		}
		group, _, versioned := strings.Cut(head.APIVersion, "/")
		if !versioned {
			group = ""
		}
		out = append(out, object{Group: group, Kind: head.Kind, Namespace: head.Metadata.Namespace, Name: head.Metadata.Name})
	}
}
