// Package rollout gives a running cluster a release's component set through its cluster repository, where one Argo CD
// Application per component applies, changes and prunes it, and reads each one back from the cluster's node report.
package rollout

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	// ComponentsDir holds one directory per component, which its Application applies.
	ComponentsDir = "components"
	manifestFile  = "manifest.yaml"
	clusterDir    = "cluster"
	// argoCDNamespace holds Argo CD, each component's marker and each component's handover Job.
	argoCDNamespace = "argocd"
	markerDigestLen = 12
	synced          = "Synced"
	healthy         = "Healthy"
)

// LegacyFiles apply the hosting policies apart from the carriage, which takes them out so that one Application alone
// applies each policy.
var LegacyFiles = []string{"cluster/mitstack-hosting.yaml", "hosting/policies.yaml"}

//go:embed application.yaml
var applicationTemplate string

var application = tmpl.Must("a component's Application", applicationTemplate)

// Release is the component set a kit release carries to a running cluster.
type Release struct {
	Components []components.Manifest
	// ToolsImage is the tools image the kit release published, by digest, which each component's handover Job runs.
	ToolsImage string
}

// File is one file of the release at its path in the cluster repository.
type File struct {
	Path  string
	Bytes []byte
}

// carried is one component as the repository holds it: the documents it carries, the Job that hands them over from
// Talos, then the ConfigMap named for the digest of both, since Argo CD's list of what it applied names objects and no
// revision.
type carried struct {
	name, app string
	bytes     []byte
	// objects are what Argo CD lists once it applied the file: each carried document and the marker.
	objects []object
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

// ownedByCarriage is whether p is a path the carriage writes for some component, or one of LegacyFiles, so a file an app
// keeps beside them is never taken out.
func ownedByCarriage(p string) bool {
	if slices.Contains(LegacyFiles, p) {
		return true
	}
	if name, ok := strings.CutPrefix(p, ComponentsDir+"/"); ok {
		name, ok = strings.CutSuffix(name, "/"+manifestFile)
		return ok && check.DNS1123Label(ApplicationPrefix+name) == nil
	}
	app, inCluster := strings.CutPrefix(p, clusterDir+"/")
	app, isYAML := strings.CutSuffix(app, ".yaml")
	return inCluster && isYAML && strings.HasPrefix(app, ApplicationPrefix) && check.DNS1123Label(app) == nil
}

// carriedCache keeps each component as the repository holds it, by its digest and the tools image, since splitting the
// set's documents decodes every one of them and a block reads the set on every call.
var carriedCache sync.Map

func (r Release) carried() ([]carried, error) {
	if err := check.DigestRef(r.ToolsImage); err != nil {
		return nil, fmt.Errorf("the release's tools image, which each handover Job runs: %w", err)
	}
	var set []carried
	for _, m := range r.Components {
		c, err := r.carry(m)
		if err != nil {
			return nil, err
		}
		if len(c.objects) > 0 {
			set = append(set, c)
		}
	}
	return set, nil
}

// carry is m as the repository holds it, with no object when each of its documents stays with the build.
func (r Release) carry(m components.Manifest) (carried, error) {
	c := carried{name: m.Name, app: ApplicationPrefix + m.Name}
	if err := check.DNS1123Label(c.handoverJob().Name); err != nil {
		return carried{}, fmt.Errorf("the component %s cannot name an Application and its handover Job: %w", m.Name, err)
	}
	key := m.Name + "\x00" + m.SHA256() + "\x00" + r.ToolsImage
	if kept, ok := carriedCache.Load(key); ok {
		return kept.(carried), nil
	}
	docs, heads, err := keepCarried(m)
	if err != nil {
		return carried{}, err
	}
	if len(heads) > 0 {
		job, err := c.renderHandover(heads, r.ToolsImage)
		if err != nil {
			return carried{}, err
		}
		c = c.withMarker(append(docs, job...), heads)
	}
	carriedCache.Store(key, c)
	return c, nil
}

// withMarker closes stated, the component's documents and its handover Job, with the marker named for their digest.
func (c carried) withMarker(stated []byte, heads []documentHead) carried {
	stated = append(stated, "---\n"...)
	sum := sha256.Sum256(stated)
	marker := object{Kind: "ConfigMap", Namespace: argoCDNamespace, Name: c.app + "-" + hex.EncodeToString(sum[:])[:markerDigestLen]}
	c.bytes = fmt.Appendf(stated, "# Argo CD lists this ConfigMap only once it compared the documents above, whose digest it is named for.\n"+
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n", marker.Name, marker.Namespace)
	for _, head := range heads {
		c.objects = append(c.objects, head.object())
	}
	c.objects = append(c.objects, marker)
	return c
}

// keepCarried keeps each document of m, with its head, but those that stay with the build: a Secret holding data, which
// carries a key the repository must never hold; a Job, which runs once and whose pod template no later sync can change;
// and a Helm or Argo CD hook, which Argo CD would run again on every sync. Each kept document ends with a separator.
func keepCarried(m components.Manifest) ([]byte, []documentHead, error) {
	var out bytes.Buffer
	var heads []documentHead
	for _, doc := range splitDocuments(m.Bytes) {
		var head documentHead
		if err := yaml.Unmarshal(doc, &head); err != nil {
			return nil, nil, fmt.Errorf("the component %s: %w", m.Name, err)
		}
		if head.Kind == "" || head.staysWithBuild() {
			continue
		}
		heads = append(heads, head)
		out.Write(doc)
		if !bytes.HasSuffix(doc, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString("---\n")
	}
	return out.Bytes(), heads, nil
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

// object is the document as Argo CD lists it.
func (h documentHead) object() object {
	group, _, versioned := strings.Cut(h.APIVersion, "/")
	if !versioned {
		group = ""
	}
	return object{Group: group, Kind: h.Kind, Namespace: h.Metadata.Namespace, Name: h.Metadata.Name}
}

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
		objects, missing := c.readApplication(body.ArgoCD.Applications)
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

// readApplication is each object the report reads back of the component's Application, or why it does not.
func (c carried) readApplication(apps []reportedApplication) (applied, problems []string) {
	i := slices.IndexFunc(apps, func(a reportedApplication) bool { return a.Name == c.app })
	switch {
	case i < 0:
		return nil, []string{"it lists no Argo CD Application " + c.app}
	case apps[i].Resources == nil:
		return nil, []string{"it carries no list of the objects Argo CD's Application " + c.app + " applied"}
	case apps[i].Sync != synced || apps[i].Health != healthy:
		return nil, []string{fmt.Sprintf("Argo CD's Application %s reads sync %q and health %q", c.app, apps[i].Sync, apps[i].Health)}
	}
	for _, l := range *apps[i].Resources {
		switch {
		case l.object == c.handoverJob():
			problems = append(problems, fmt.Sprintf("%s's handover %s has not succeeded, so its objects may still hold a field Talos owns", c.app, l.object))
		case !slices.Contains(c.objects, l.object):
			problems = append(problems, fmt.Sprintf("%s lists %s, which the release does not state", c.app, l.object))
		case l.Sync != synced:
			problems = append(problems, fmt.Sprintf("%s lists %s %s", c.app, l.object, l.Sync))
		default:
			applied = append(applied, l.object.String())
		}
	}
	for _, o := range c.objects {
		if !slices.ContainsFunc(*apps[i].Resources, func(l listedObject) bool { return l.object == o }) {
			problems = append(problems, fmt.Sprintf("%s does not list %s", c.app, o))
		}
	}
	return applied, problems
}
