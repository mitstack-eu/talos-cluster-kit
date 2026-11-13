//go:build apiserver

// A component's handover Job gives up Talos's fields through an API server at the Kubernetes release the kit pins, so
// an object Talos created holds only what Argo CD applies.
package talosclusterkit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	argoCDUser = "system:serviceaccount:" + rollout.ArgoCDNamespace + ":" + rollout.ArgoCDController
	// talosInventoryAnnotation is the annotation Talos v1.14.4 gives each object it creates, naming its inventory.
	talosInventoryAnnotation = "config.k8s.io/owning-inventory"
	// argoCDTrackingAnnotation is what Argo CD v3.5.5 gives each object it applies but a CustomResourceDefinition.
	argoCDTrackingAnnotation = "argocd.argoproj.io/tracking-id"
	// droppedContainer is the privileged init container Cilium's agent ran at kit release v0.53.0.
	droppedContainer  = "mount-bpf-fs"
	ciliumApplication = rollout.ApplicationPrefix + "cilium"
)

type managedObject struct {
	Kind     string
	Metadata struct {
		Name            string
		Generation      int
		ResourceVersion string
		Annotations     map[string]string
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

// statedObjects are the documents of a carried file that Argo CD applies, which leaves out the hook it only runs.
func statedObjects(t *testing.T, file []byte) []map[string]any {
	t.Helper()
	var objects []map[string]any
	for _, doc := range testutil.Documents(t, file) {
		if !testutil.IsHook(doc) {
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
			if name == rollout.ArgoCDController {
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

// argoCDSyncs applies each object file states as Argo CD v3.5.5 syncs the Application app with ServerSideApply=true.
func (s *apiServer) argoCDSyncs(t *testing.T, app string, file []byte) {
	t.Helper()
	out, err := s.argoCDApply(t, app, statedObjects(t, file)).CombinedOutput()
	require.NoError(t, err, "Argo CD's apply: %s", out)
}

// argoCDApply is the command that applies objects as Argo CD v3.5.5 does for the Application app: a forced server-side
// apply as its application controller, of each object with the tracking annotation its repo-server gave it.
func (s *apiServer) argoCDApply(t *testing.T, app string, objects []map[string]any) *exec.Cmd {
	t.Helper()
	for _, doc := range objects {
		group, _, versioned := strings.Cut(doc["apiVersion"].(string), "/")
		if !versioned {
			group = ""
		}
		if doc["kind"] == "CustomResourceDefinition" && group == "apiextensions.k8s.io" {
			continue
		}
		metadata := doc["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
		}
		namespace, _ := metadata["namespace"].(string)
		annotations[argoCDTrackingAnnotation] = fmt.Sprintf("%s:%s/%s:%s/%s", app, group, doc["kind"], namespace, metadata["name"])
		metadata["annotations"] = annotations
	}
	return s.adminCommand(stream(t, objects...), "apply", "--server-side", "--force-conflicts",
		"--field-manager="+rollout.ArgoCDManager, "--as="+argoCDUser, "-f", "-")
}

// runHandover runs the handover Job of file as Argo CD's sync does: the API server takes the Job from the application
// controller, and the Job's container runs as its pod would, with its account's token and each of env set over the Job's.
func (s *apiServer) runHandover(t *testing.T, file []byte, env ...string) (string, error) {
	t.Helper()
	out, err := s.handover(t, file, env...).CombinedOutput()
	return string(out), err
}

// runHandoverApplying runs the handover as runHandover does and runs apply once the handover says its wait begins, as
// Argo CD applies a wave's later objects while the wave's Job runs.
func (s *apiServer) runHandoverApplying(t *testing.T, file []byte, apply *exec.Cmd) (string, error) {
	t.Helper()
	cmd := s.handover(t, file)
	printed, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = cmd.Stdout
	require.NoError(t, cmd.Start())
	var out strings.Builder
	for lines := bufio.NewScanner(printed); lines.Scan(); {
		out.WriteString(lines.Text() + "\n")
		if strings.HasPrefix(lines.Text(), "handover: waiting") {
			applied, err := apply.CombinedOutput()
			require.NoError(t, err, "Argo CD's apply of the objects the handover waits for: %s", applied)
		}
	}
	return out.String(), cmd.Wait()
}

// handover is the command that runs the handover Job of file, not yet started.
func (s *apiServer) handover(t *testing.T, file []byte, env ...string) *exec.Cmd {
	t.Helper()
	var hooks []map[string]any
	for _, doc := range testutil.Documents(t, file) {
		if testutil.IsHook(doc) {
			hooks = append(hooks, doc)
		}
	}
	require.Len(t, hooks, 1, "the hooks a component's file holds")
	job := hooks[0]
	s.admin(t, stream(t, job), "apply", "--server-side", "--dry-run=server", "--field-manager="+rollout.ArgoCDManager, "--as="+argoCDUser, "-f", "-")
	pod := testutil.Field(t, job, "spec", "template", "spec")
	container := testutil.Field(t, pod, "containers", "0")
	args := containerArgs(t, job, pod, container, nil)
	for _, variable := range env {
		args = append(args, "-e", variable)
	}
	return s.asInCommand(t, testutil.Field(t, job, "metadata", "namespace").(string), testutil.Field(t, pod, "serviceAccountName").(string),
		args, command(t, container)...)
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

// lateObjects are the documents of file that Argo CD applies, picked by kind and namespace/name.
func lateObjects(t *testing.T, file []byte, names ...string) []map[string]any {
	t.Helper()
	var picked []map[string]any
	for _, doc := range statedObjects(t, file) {
		name := fmt.Sprintf("%s %s/%s", doc["kind"], testutil.Field(t, doc, "metadata", "namespace"), testutil.Field(t, doc, "metadata", "name"))
		if slices.Contains(names, name) {
			picked = append(picked, doc)
		}
	}
	require.Len(t, picked, len(names), "the release's objects named %v", names)
	return picked
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

	t.Run("a handover before Argo CD applied the component waits, is refused and takes no field away", func(t *testing.T) {
		out, err := server.runHandover(t, carriedFile(t, before), "HANDOVER_ADOPTION_TIMEOUT=2s")

		require.Error(t, err, "the handover printed:\n%s", out)
		assert.Contains(t, out, "DaemonSet kube-system/cilium", "the refusal names an object Argo CD has not applied")
		assert.Contains(t, out, "within 2s", "the refusal names the wait for Argo CD's apply that ran out")
		assert.Contains(t, server.ciliumAgent(t).initContainers(), droppedContainer, "the agent's init containers")
	})

	server.argoCDSyncs(t, ciliumApplication, carriedFile(t, before))
	server.argoCDSyncs(t, ciliumApplication, release)
	synced := server.ciliumAgent(t)

	t.Run("Argo CD's sync of the release leaves the container Talos still owns, and the agent's pods never roll", func(t *testing.T) {
		assert.Contains(t, synced.initContainers(), droppedContainer, "the agent's init containers after the sync")
		assert.Equal(t, []string{droppedContainer}, synced.privileged(), "the agent's privileged containers after the sync")
		assert.Equal(t, 1, synced.Metadata.Generation, "the agent's generation, which a changed pod template raises")
		assert.True(t, synced.appliedBy(constants.KubernetesFieldManagerName) && synced.appliedBy(rollout.ArgoCDManager),
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
		server.argoCDSyncs(t, ciliumApplication, release)

		require.NoError(t, err, "the handover printed:\n%s", out)
		assert.Equal(t, handedOver.Metadata.ResourceVersion, server.ciliumAgent(t).Metadata.ResourceVersion, "the agent's resource version")
	})

	t.Run("a handover that starts before Argo CD creates two objects of its wave waits for them and succeeds", func(t *testing.T) {
		late := lateObjects(t, release, "ServiceAccount kube-system/cilium-envoy", "ConfigMap kube-system/cilium-envoy-config")
		server.admin(t, nil, "delete", "serviceaccount/cilium-envoy", "configmap/cilium-envoy-config", "-n", "kube-system")

		out, err := server.runHandoverApplying(t, release, server.argoCDApply(t, ciliumApplication, late))

		require.NoError(t, err, "the handover printed:\n%s", out)
		assert.Empty(t, namedByTalos(server.live(t, release)), "the component's objects that still name Talos as a field manager")
	})
}

func TestAPIServer_AComponentsHandoverGivesUpTalossFieldsOnACustomResourceDefinitionArgoCDAppliesWithoutItsTrackingAnnotation(t *testing.T) {
	policyCRD := testutil.Manifest(t, "cilium-policy-crd", components.Default().Served)
	file := carriedFile(t, policyCRD)
	application := rollout.ApplicationPrefix + policyCRD.Name
	server := startAPIServer(t)
	server.admin(t, argoCDIdentity(t), "apply", "-f", "-")
	server.talosCreates(t, policyCRD.Bytes)
	definition := func() managedObject {
		definitions := server.live(t, policyCRD.Bytes)
		require.Len(t, definitions, 1, "the definitions the component states")
		require.Equal(t, "CustomResourceDefinition", definitions[0].Kind)
		return definitions[0]
	}

	t.Run("a handover that starts before Argo CD applies the definition waits for that apply and leaves Talos no field of it", func(t *testing.T) {
		out, err := server.runHandoverApplying(t, file, server.argoCDApply(t, application, statedObjects(t, file)))

		require.NoError(t, err, "the handover printed:\n%s", out)
		handedOver := definition()
		assert.NotContains(t, handedOver.Metadata.Annotations, argoCDTrackingAnnotation, "the definition's annotations, as Argo CD applied it")
		assert.True(t, handedOver.appliedBy(rollout.ArgoCDManager) && !handedOver.appliedBy(constants.KubernetesFieldManagerName),
			"Argo CD alone applies the definition: %v", handedOver.Metadata.ManagedFields)
		assert.Equal(t, 1, handedOver.Metadata.Generation, "the definition's generation, which a changed spec raises")
	})

	t.Run("a second handover, of a definition Argo CD applied before it started, changes nothing", func(t *testing.T) {
		handedOver := definition()

		out, err := server.runHandover(t, file)

		require.NoError(t, err, "the handover printed:\n%s", out)
		assert.Equal(t, handedOver.Metadata.ResourceVersion, definition().Metadata.ResourceVersion, "the definition's resource version")
	})
}
