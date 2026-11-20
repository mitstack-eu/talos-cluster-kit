package testutil

import (
	"crypto/sha256"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ObjectRef names a Kubernetes object as Argo CD lists it, with no namespace for a cluster-wide one.
type ObjectRef struct {
	Group, Kind, Namespace, Name string
}

func (r ObjectRef) String() string {
	if r.Namespace == "" {
		return r.Kind + " " + r.Name
	}
	return r.Kind + " " + r.Namespace + "/" + r.Name
}

func sortedRefs(objects map[ObjectRef]map[string]any) []ObjectRef {
	return slices.SortedFunc(maps.Keys(objects), func(a, b ObjectRef) int { return strings.Compare(a.String(), b.String()) })
}

func refOf(t *testing.T, doc map[string]any) ObjectRef {
	t.Helper()
	group, _, versioned := strings.Cut(Field(t, doc, "apiVersion").(string), "/")
	if !versioned {
		group = ""
	}
	namespace, _ := Field(t, doc, "metadata", "namespace").(string)
	return ObjectRef{Group: group, Kind: doc["kind"].(string), Namespace: namespace, Name: Field(t, doc, "metadata", "name").(string)}
}

// Cluster is a running cluster as far as a cluster repository reaches it: the objects its API serves, the ones Talos
// created once at the build among them, and Argo CD, whose Applications apply, change and prune what the repository holds.
// It tracks no field manager, so an object reads as it does once its Application's handover gave up Talos's fields.
type Cluster struct {
	t          *testing.T
	objects    map[ObjectRef]map[string]any
	repository map[string][]byte
	// applied is what each Application applied at its last sync, which it prunes once the repository drops it.
	applied map[string]map[ObjectRef]map[string]any
	// unread are the Applications whose address the cluster holds no repository credential for, so their last sync
	// compared and applied nothing.
	unread map[string]bool
}

// rootApplication is the cluster's own Application, whose address alone the cluster's repository credential opens.
var rootApplication = ObjectRef{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "cluster"}

// BuildCluster creates each object of manifests once, as Talos does at a cluster's first build.
func BuildCluster(t *testing.T, manifests ...[]byte) *Cluster {
	t.Helper()
	c := &Cluster{t: t, objects: map[ObjectRef]map[string]any{}, repository: map[string][]byte{}, applied: map[string]map[ObjectRef]map[string]any{},
		unread: map[string]bool{}}
	for _, m := range manifests {
		for _, doc := range Documents(t, m) {
			c.objects[refOf(t, doc)] = doc
		}
	}
	c.applied["cluster"] = map[ObjectRef]map[string]any{}
	return c
}

// Commit writes each file to the cluster repository, as a block's commit does, and takes out each file whose content is nil.
func (c *Cluster) Commit(files map[string][]byte) {
	for path, data := range files {
		if data == nil {
			delete(c.repository, path)
			continue
		}
		c.repository[path] = data
	}
}

// Files are the paths the cluster repository holds, sorted.
func (c *Cluster) Files() []string { return slices.Sorted(maps.Keys(c.repository)) }

// Object is the object the cluster's API serves under ref, or nil.
func (c *Cluster) Object(ref ObjectRef) map[string]any { return c.objects[ref] }

// Sync runs each Application once, the cluster Application first, so one it applies syncs in the same pass. On a cluster
// that holds the root Application, an Application at any other address applies nothing and reads sync "Unknown".
func (c *Cluster) Sync() {
	c.sync("cluster", "cluster")
	for _, app := range sortedRefs(c.objects) {
		if app.Kind != "Application" || app.Name == "cluster" {
			continue
		}
		if !c.readable(c.objects[app]) {
			c.unread[app.Name] = true
			continue
		}
		delete(c.unread, app.Name)
		c.sync(app.Name, Field(c.t, c.objects[app], "spec", "source", "path").(string))
	}
}

// readable is whether the cluster's repository credential opens app's address: any address on a cluster built without
// the root Application, and otherwise the root Application's alone.
func (c *Cluster) readable(app map[string]any) bool {
	root, rooted := c.objects[rootApplication]
	return !rooted || Field(c.t, app, "spec", "source", "repoURL") == Field(c.t, root, "spec", "source", "repoURL")
}

func (c *Cluster) sync(app, path string) {
	desired := c.desired(path)
	for ref := range c.applied[app] {
		if _, kept := desired[ref]; !kept {
			c.delete(ref)
		}
	}
	maps.Copy(c.objects, desired)
	c.applied[app] = desired
}

// resourcesFinalizer on an Application has Argo CD delete what it applied before the Application itself goes.
const resourcesFinalizer = "resources-finalizer.argocd.argoproj.io"

// delete takes ref out of the cluster, and once it is an Application, its list of what it applied, with each object on
// that list when the Application carries Argo CD's resources finalizer.
func (c *Cluster) delete(ref ObjectRef) {
	obj := c.objects[ref]
	delete(c.objects, ref)
	if ref.Kind != "Application" || ref.Group != "argoproj.io" {
		return
	}
	applied := c.applied[ref.Name]
	delete(c.applied, ref.Name)
	delete(c.unread, ref.Name)
	metadata, _ := obj["metadata"].(map[string]any)
	finalizers, _ := metadata["finalizers"].([]any)
	if !slices.Contains(finalizers, any(resourcesFinalizer)) {
		return
	}
	for owned := range applied {
		c.delete(owned)
	}
}

func (c *Cluster) desired(path string) map[ObjectRef]map[string]any {
	out := map[ObjectRef]map[string]any{}
	for file, data := range c.repository {
		if strings.HasPrefix(file, path+"/") && strings.HasSuffix(file, ".yaml") {
			for _, doc := range c.parsed(data) {
				if !IsHook(doc) {
					out[refOf(c.t, doc)] = doc
				}
			}
		}
	}
	return out
}

// hookAnnotation marks a document Argo CD runs during a sync as a hook, which is no object the Application applies.
const hookAnnotation = "argocd.argoproj.io/hook"

// IsHook is whether Argo CD runs doc during a sync as a hook rather than applying it.
func IsHook(doc map[string]any) bool {
	metadata, _ := doc["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	_, hook := annotations[hookAnnotation]
	return hook
}

// parsedFiles keeps each file's documents by its digest, since a component set runs to megabytes of YAML and a test
// syncs it many times. No caller changes a document it is given.
var parsedFiles sync.Map

func (c *Cluster) parsed(data []byte) []map[string]any {
	key := sha256.Sum256(data)
	if docs, ok := parsedFiles.Load(key); ok {
		return docs.([]map[string]any)
	}
	docs := Documents(c.t, data)
	parsedFiles.Store(key, docs)
	return docs
}

// Report is a node report body holding Argo CD's Applications as report/scripts/report.sh reads them, each object
// Synced while the API serves it as the Application last applied it, and each unread Application at sync "Unknown" with
// no sync run and nothing listed.
func (c *Cluster) Report() []byte {
	type resource struct {
		Group     string  `json:"group"`
		Kind      string  `json:"kind"`
		Namespace *string `json:"namespace"`
		Name      string  `json:"name"`
		Sync      string  `json:"sync"`
	}
	type application struct {
		Name      string         `json:"name"`
		Health    string         `json:"health"`
		Sync      string         `json:"sync"`
		Operation map[string]any `json:"operation"`
		Resources []resource     `json:"resources"`
	}
	names := slices.Collect(maps.Keys(c.applied))
	for name := range c.unread {
		if _, applied := c.applied[name]; !applied {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var apps []application
	for _, name := range names {
		if c.unread[name] {
			apps = append(apps, application{Name: name, Health: "Healthy", Sync: "Unknown", Resources: []resource{}})
			continue
		}
		app := application{Name: name, Health: "Healthy", Sync: "Synced", Operation: map[string]any{"phase": "Succeeded", "message": nil}, Resources: []resource{}}
		for _, ref := range sortedRefs(c.applied[name]) {
			r := resource{Group: ref.Group, Kind: ref.Kind, Name: ref.Name, Sync: "Synced"}
			if ref.Namespace != "" {
				r.Namespace = &ref.Namespace
			}
			if !reflect.DeepEqual(c.objects[ref], c.applied[name][ref]) {
				r.Sync, app.Sync = "OutOfSync", "OutOfSync"
			}
			app.Resources = append(app.Resources, r)
		}
		apps = append(apps, app)
	}
	body, err := json.Marshal(map[string]any{"argocd": map[string]any{"applications": apps}})
	require.NoError(c.t, err)
	return body
}
