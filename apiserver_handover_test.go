//go:build apiserver

// A component's handover Job gives up Talos's fields through an API server at the Kubernetes release the kit pins, so
// an object Talos created holds only what Argo CD applies.
package talosclusterkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

const (
	argoCDController = "argocd-application-controller"
	argoCDUser       = "system:serviceaccount:argocd:" + argoCDController
	// talosInventoryAnnotation is the annotation Talos v1.14.4 gives each object it creates, naming its inventory.
	talosInventoryAnnotation = "config.k8s.io/owning-inventory"
	// droppedContainer is the privileged init container Cilium's agent ran at kit release v0.53.0.
	droppedContainer = "mount-bpf-fs"
	argoCDHook       = "argocd.argoproj.io/hook"
	// argoCDTracking is the annotation Argo CD v3.5.5 gives each object it applies, naming the Application and the object.
	argoCDTracking    = "argocd.argoproj.io/tracking-id"
	ciliumApplication = rollout.ApplicationPrefix + "cilium"
)

type managedObject struct {
	Kind     string
	Metadata struct {
		Name            string
		Generation      int
		ResourceVersion string
		ManagedFields   []struct{ Manager, Operation string }
	}
	Spec struct {
		Template struct {
			Spec struct{ InitContainers, Containers []podContainer }
		}
	}
}

type podContainer struct {
	Name            string
	SecurityContext struct{ Privileged bool }
}

func (o managedObject) appliedBy(manager string) bool {
	for _, entry := range o.Metadata.ManagedFields {
		if entry.Manager == manager && entry.Operation == "Apply" {
			return true
		}
	}
	return false
}

func (o managedObject) initContainers() []string {
	var names []string
	for _, c := range o.Spec.Template.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

func (o managedObject) privileged() []string {
	var names []string
	for _, c := range append(o.Spec.Template.Spec.InitContainers, o.Spec.Template.Spec.Containers...) {
		if c.SecurityContext.Privileged {
			names = append(names, c.Name)
		}
	}
	return names
}

// ciliumBefore is the Cilium component as kit release v0.53.0 shipped it.
func ciliumBefore(t *testing.T) components.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v0.53.0", "cilium.yaml"))
	require.NoError(t, err)
	return components.Manifest{Name: "cilium", Path: "cilium/cilium.yaml", Bytes: data}
}

// carriedFile is the file a block commits for the component m.
func carriedFile(t *testing.T, m components.Manifest) []byte {
	t.Helper()
	files, err := rollout.Release{Components: []components.Manifest{m}, ToolsImage: testutil.ToolsImage}.Files("acme/shop-cluster")
	require.NoError(t, err)
	require.Equal(t, "components/"+m.Name+"/manifest.yaml", files[0].Path)
	return files[0].Bytes
}

func isArgoCDHook(doc map[string]any) bool {
	annotations, _ := doc["metadata"].(map[string]any)["annotations"].(map[string]any)
	_, hook := annotations[argoCDHook]
	return hook
}

// statedObjects are the documents of a carried file that Argo CD applies, which leaves out the hook it only runs.
func statedObjects(t *testing.T, file []byte) []map[string]any {
	t.Helper()
	var objects []map[string]any
	for _, doc := range testutil.Documents(t, file) {
		if !isArgoCDHook(doc) {
			objects = append(objects, doc)
		}
	}
	return objects
}

func stream(t *testing.T, docs ...map[string]any) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, doc := range docs {
		data, err := yaml.Marshal(doc)
		require.NoError(t, err)
		out.WriteString("---\n")
		out.Write(data)
	}
	return out.Bytes()
}

// argoCDIdentity is Argo CD's namespace and its application controller's account, ClusterRole and binding, from the
// kit's own Argo CD manifest.
func argoCDIdentity(t *testing.T) []byte {
	t.Helper()
	var identity []map[string]any
	for _, doc := range testutil.Documents(t, testutil.Manifest(t, "argocd", components.Default().Served).Bytes) {
		name := testutil.Field(t, doc, "metadata", "name")
		switch doc["kind"] {
		case "Namespace":
			identity = append(identity, doc)
		case "ServiceAccount", "ClusterRole", "ClusterRoleBinding":
			if name == argoCDController {
				identity = append(identity, doc)
			}
		}
	}
	require.Len(t, identity, 4, "Argo CD's namespace and its application controller's account, ClusterRole and binding")
	return stream(t, identity...)
}

// talosCreates creates each object of manifest as Talos v1.14.4's ManifestApplyController does: in Talos's order, by a
// server-side apply without force under Talos's field manager, of the object with Talos's inventory annotation.
func (s *apiServer) talosCreates(t *testing.T, manifest []byte) {
	t.Helper()
	var docs []map[string]any
	for _, o := range testutil.TalosOrder(t, map[string][]byte{"manifest": manifest}) {
		metadata := o.Doc["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
		}
		annotations[talosInventoryAnnotation] = constants.KubernetesBootstrapManifestsInventoryName
		metadata["annotations"] = annotations
		docs = append(docs, o.Doc)
	}
	s.admin(t, stream(t, docs...), "apply", "--server-side", "--field-manager="+constants.KubernetesFieldManagerName, "-f", "-")
}

// argoCDSyncs applies each object file states as Argo CD v3.5.5 syncs an Application that sets ServerSideApply=true: a
// forced server-side apply under Argo CD's field manager, as its application controller, of the object with the
// annotation Argo CD tracks it by.
func (s *apiServer) argoCDSyncs(t *testing.T, file []byte) {
	t.Helper()
	objects := statedObjects(t, file)
	for _, doc := range objects {
		group, _, versioned := strings.Cut(doc["apiVersion"].(string), "/")
		if !versioned {
			group = ""
		}
		metadata := doc["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
		}
		namespace, _ := metadata["namespace"].(string)
		annotations[argoCDTracking] = fmt.Sprintf("%s:%s/%s:%s/%s", ciliumApplication, group, doc["kind"], namespace, metadata["name"])
		metadata["annotations"] = annotations
	}
	s.admin(t, stream(t, objects...), "apply", "--server-side", "--force-conflicts",
		"--field-manager=argocd-controller", "--as="+argoCDUser, "-f", "-")
}

// runHandover runs the handover Job of file as Argo CD's sync does once the objects are applied: the API server takes
// the Job from the application controller, and the Job's container runs as its pod would, with its account's token.
func (s *apiServer) runHandover(t *testing.T, file []byte) (string, error) {
	t.Helper()
	var hooks []map[string]any
	for _, doc := range testutil.Documents(t, file) {
		if isArgoCDHook(doc) {
			hooks = append(hooks, doc)
		}
	}
	require.Len(t, hooks, 1, "the hooks a component's file holds")
	job := hooks[0]
	s.admin(t, stream(t, job), "apply", "--server-side", "--dry-run=server", "--field-manager=argocd-controller", "--as="+argoCDUser, "-f", "-")
	pod := testutil.Field(t, job, "spec", "template", "spec")
	container := testutil.Field(t, pod, "containers", "0")
	return s.asIn(t, testutil.Field(t, job, "metadata", "namespace").(string), testutil.Field(t, pod, "serviceAccountName").(string),
		containerArgs(t, job, pod, container, nil), command(t, container)...)
}

// live reads each object file states, with its managed fields.
func (s *apiServer) live(t *testing.T, file []byte) []managedObject {
	t.Helper()
	var list struct{ Items []managedObject }
	out := s.admin(t, stream(t, statedObjects(t, file)...), "get", "-f", "-", "--show-managed-fields", "-o", "json")
	require.NoError(t, json.Unmarshal(out, &list))
	require.NotEmpty(t, list.Items)
	return list.Items
}

func (s *apiServer) ciliumAgent(t *testing.T) managedObject {
	t.Helper()
	var agent managedObject
	out := s.admin(t, nil, "get", "daemonset", "cilium", "-n", "kube-system", "--show-managed-fields", "-o", "json")
	require.NoError(t, json.Unmarshal(out, &agent))
	return agent
}

func namedByTalos(objects []managedObject) []string {
	var named []string
	for _, o := range objects {
		if o.appliedBy(constants.KubernetesFieldManagerName) {
			named = append(named, o.Kind+" "+o.Metadata.Name)
		}
	}
	return named
}

func TestAPIServer_AnObjectTalosCreatedKeepsAFieldAReleaseDropsUntilItsComponentsHandoverGivesUpTalossFields(t *testing.T) {
	before := ciliumBefore(t)
	release := carriedFile(t, testutil.Manifest(t, "cilium", components.Default().Served))
	var stated managedObject
	for _, doc := range statedObjects(t, release) {
		if doc["kind"] == "DaemonSet" && testutil.Field(t, doc, "metadata", "name") == "cilium" {
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &stated))
		}
	}
	require.NotEmpty(t, stated.initContainers(), "the release states Cilium's agent")
	require.NotContains(t, stated.initContainers(), droppedContainer, "the release drops the container")
	server := startAPIServer(t)
	server.admin(t, argoCDIdentity(t), "apply", "-f", "-")
	server.talosCreates(t, before.Bytes)

	t.Run("a handover before Argo CD applied the component is refused and takes no field away", func(t *testing.T) {
		out, err := server.runHandover(t, carriedFile(t, before))

		require.Error(t, err, "the handover printed:\n%s", out)
		assert.Contains(t, out, "DaemonSet kube-system/cilium", "the refusal names an object Argo CD has not applied")
		assert.Contains(t, server.ciliumAgent(t).initContainers(), droppedContainer, "the agent's init containers")
	})

	server.argoCDSyncs(t, carriedFile(t, before))
	server.argoCDSyncs(t, release)
	synced := server.ciliumAgent(t)

	t.Run("Argo CD's sync of the release leaves the container Talos still owns, and the agent's pods never roll", func(t *testing.T) {
		assert.Contains(t, synced.initContainers(), droppedContainer, "the agent's init containers after the sync")
		assert.Equal(t, []string{droppedContainer}, synced.privileged(), "the agent's privileged containers after the sync")
		assert.Equal(t, 1, synced.Metadata.Generation, "the agent's generation, which a changed pod template raises")
		assert.True(t, synced.appliedBy(constants.KubernetesFieldManagerName) && synced.appliedBy("argocd-controller"),
			"Talos and Argo CD both own fields of the agent: %v", synced.Metadata.ManagedFields)
	})

	t.Run("the handover drops the container, rolls the agent and leaves Talos no field of the component", func(t *testing.T) {
		out, err := server.runHandover(t, release)

		require.NoError(t, err, "the handover printed:\n%s", out)
		agent := server.ciliumAgent(t)
		assert.Equal(t, stated.initContainers(), agent.initContainers(), "the agent's init containers, as the release states them")
		assert.Empty(t, agent.privileged(), "the agent's privileged containers")
		assert.Equal(t, 2, agent.Metadata.Generation, "the agent's generation, so its pods roll once")
		assert.Empty(t, namedByTalos(server.live(t, release)), "the component's objects that still name Talos as a field manager")
	})

	t.Run("a second handover and a second sync change nothing", func(t *testing.T) {
		handedOver := server.ciliumAgent(t)

		out, err := server.runHandover(t, release)
		server.argoCDSyncs(t, release)

		require.NoError(t, err, "the handover printed:\n%s", out)
		assert.Equal(t, handedOver.Metadata.ResourceVersion, server.ciliumAgent(t).Metadata.ResourceVersion, "the agent's resource version")
	})
}
