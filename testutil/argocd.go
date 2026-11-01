package testutil

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
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
type Cluster struct {
	t          *testing.T
	objects    map[ObjectRef]map[string]any
	repository map[string][]byte
	// applied is what each Application applied at its last sync, which it prunes once the repository drops it.
	applied map[string]map[ObjectRef]map[string]any
}

// BuildCluster creates each object of manifests once, as Talos does at a cluster's first build.
func BuildCluster(t *testing.T, manifests ...[]byte) *Cluster {
	t.Helper()
	c := &Cluster{t: t, objects: map[ObjectRef]map[string]any{}, repository: map[string][]byte{}, applied: map[string]map[ObjectRef]map[string]any{}}
	for _, m := range manifests {
		for _, doc := range Documents(t, m) {
			c.objects[refOf(t, doc)] = doc
		}
	}
	c.applied["cluster"] = map[ObjectRef]map[string]any{}
	return c
}

// Commit writes each file to the cluster repository, as a block's commit does.
func (c *Cluster) Commit(files map[string][]byte) {
	maps.Copy(c.repository, files)
}

// Object is the object the cluster's API serves under ref, or nil.
func (c *Cluster) Object(ref ObjectRef) map[string]any { return c.objects[ref] }

// Sync runs each Application once, the cluster Application first, so one it applies syncs in the same pass.
func (c *Cluster) Sync() {
	c.sync("cluster", "cluster")
	for _, app := range sortedRefs(c.objects) {
		if app.Kind == "Application" && app.Name != "cluster" {
			c.sync(app.Name, Field(c.t, c.objects[app], "spec", "source", "path").(string))
		}
	}
}

func (c *Cluster) sync(app, path string) {
	desired := c.desired(path)
	for ref := range c.applied[app] {
		if _, kept := desired[ref]; !kept {
			delete(c.objects, ref)
		}
	}
	maps.Copy(c.objects, desired)
	c.applied[app] = desired
}

func (c *Cluster) desired(path string) map[ObjectRef]map[string]any {
	out := map[ObjectRef]map[string]any{}
	for file, data := range c.repository {
		if strings.HasPrefix(file, path+"/") && strings.HasSuffix(file, ".yaml") {
			for _, doc := range Documents(c.t, data) {
				out[refOf(c.t, doc)] = doc
			}
		}
	}
	return out
}

// Report is a node report body holding Argo CD's Applications as report/scripts/report.sh reads them, each object
// Synced while the API serves it as the Application last applied it.
func (c *Cluster) Report() []byte {
	type resource struct {
		Group     string  `json:"group"`
		Kind      string  `json:"kind"`
		Namespace *string `json:"namespace"`
		Name      string  `json:"name"`
		Sync      string  `json:"sync"`
	}
	type application struct {
		Name      string     `json:"name"`
		Health    string     `json:"health"`
		Sync      string     `json:"sync"`
		Resources []resource `json:"resources"`
	}
	var apps []application
	for _, name := range slices.Sorted(maps.Keys(c.applied)) {
		app := application{Name: name, Health: "Healthy", Sync: "Synced", Resources: []resource{}}
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
