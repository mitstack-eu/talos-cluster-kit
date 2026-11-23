package talosclusterkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

type renderedDoc struct {
	doc    map[string]any
	source string
}

func (d renderedDoc) String() string {
	metadata, _ := d.doc["metadata"].(map[string]any)
	return fmt.Sprintf("%s: %v %v", d.source, d.doc["kind"], metadata["name"])
}

func renderedDocs(t *testing.T) []renderedDoc {
	t.Helper()
	var docs []renderedDoc
	for source, manifest := range renderedManifests(t) {
		for _, doc := range testutil.Documents(t, manifest) {
			docs = append(docs, renderedDoc{doc, source})
		}
	}
	return docs
}

func TestManifests_NoWebhookConfigurationFailsOpen(t *testing.T) {
	var open []string
	for _, d := range renderedDocs(t) {
		if d.doc["kind"] != "ValidatingWebhookConfiguration" && d.doc["kind"] != "MutatingWebhookConfiguration" {
			continue
		}
		for _, w := range d.doc["webhooks"].([]any) {
			if policy := w.(map[string]any)["failurePolicy"]; policy != "Fail" {
				open = append(open, fmt.Sprintf("%s webhook %v has failurePolicy %v", d, w.(map[string]any)["name"], policy))
			}
		}
	}

	slices.Sort(open)
	assert.Empty(t, open, "Argo CD compares the rendered policy, and a controller rewrites a policy of Ignore to Fail once it is ready")
}

func TestManifests_AnAutoscalerNeedsTheMetricsAPIItReadsAndIstiodRunsOneReplicaWithout(t *testing.T) {
	var autoscalers []string
	servesMetrics := false
	for _, d := range renderedDocs(t) {
		switch d.doc["kind"] {
		case "HorizontalPodAutoscaler":
			autoscalers = append(autoscalers, d.String())
		case "APIService":
			servesMetrics = servesMetrics || testutil.Field(t, d.doc, "spec", "group") == "metrics.k8s.io"
		}
	}
	istiod := testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "istiod", components.Default().Served).Bytes), "Deployment")[0]

	assert.True(t, servesMetrics || len(autoscalers) == 0, "the set renders %v and registers no APIService for metrics.k8s.io, so each reads Degraded", autoscalers)
	if !servesMetrics {
		assert.EqualValues(t, 1, istiod["spec"].(map[string]any)["replicas"], "without a replica count the Deployment is left to an autoscaler the cluster cannot serve")
	}
}

func TestManifests_DeclareNoEmptyLabelsOrAnnotationsMap(t *testing.T) {
	var empty []string
	for _, d := range renderedDocs(t) {
		metadata, _ := d.doc["metadata"].(map[string]any)
		for _, key := range []string{"labels", "annotations"} {
			if m, ok := metadata[key].(map[string]any); ok && len(m) == 0 {
				empty = append(empty, fmt.Sprintf("%s declares empty %s", d, key))
			}
		}
	}

	slices.Sort(empty)
	assert.Empty(t, empty, "the API server stores no empty map, so the live object never matches the committed one")
}

func TestDropEmptyMetadataMaps(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"drops a map written on two lines", "metadata:\n  labels:\n    {}\n  annotations:\n    {}\n  name: a\n", "metadata:\n  name: a\n"},
		{"drops a map written inline in each document", "---\nmetadata:\n  labels: {}\n  name: a\n---\nmetadata:\n  name: b\n  annotations: {}\nspec: {}\n", "---\nmetadata:\n  name: a\n---\nmetadata:\n  name: b\nspec: {}\n"},
		{"keeps a filled map", "metadata:\n  labels:\n    app: a\n  annotations:\n    {}\n  name: a\n", "metadata:\n  labels:\n    app: a\n  name: a\n"},
		{"keeps an empty map below the top-level metadata", "metadata:\n  name: a\nspec:\n  labels: {}\n  template:\n    metadata:\n      labels:\n        {}\n      annotations: {}\n  properties:\n    labels: {}\ndata:\n  script: |\n    metadata:\n      labels: {}\n", ""},
		{"keeps a null map before the next document", "metadata:\n  name: a\n  labels:\n---\nkind: B\n", ""},
		{"keeps a null map at the end of the file", "metadata:\n  name: a\n  labels:\n", ""},
		{"keeps a null map before another key", "metadata:\n  annotations:\n  name: a\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "rendered.yaml")
			require.NoError(t, os.WriteFile(file, []byte(tc.in), 0o644))

			got, err := testutil.RunBash(t, `. "$1" && drop_empty_metadata_maps "$2" && cat "$2"`, os.Environ(), "scripts/lib/empty-maps.sh", file)

			require.NoError(t, err, got)
			if tc.want == "" {
				tc.want = tc.in
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestKeepOffBurstNodes(t *testing.T) {
	const placed = "affinity:\n  nodeAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n      nodeSelectorTerms:\n" +
		"        - matchExpressions:\n            - key: mitstack.dev/burst\n              operator: DoesNotExist\n"
	at := func(indent int) string {
		pad := strings.Repeat(" ", indent)
		return pad + strings.ReplaceAll(strings.TrimSuffix(placed, "\n"), "\n", "\n"+pad) + "\n"
	}
	for _, tc := range []struct{ name, in, want string }{
		{"places the pod of a Deployment with no affinity",
			"kind: Deployment\nspec:\n  template:\n    metadata:\n      labels:\n        app: a\n    spec:\n      containers:\n        - name: a\n",
			"kind: Deployment\nspec:\n  template:\n    metadata:\n      labels:\n        app: a\n    spec:\n" + at(6) + "      containers:\n        - name: a\n"},
		{"adds the node affinity beside a pod anti-affinity",
			"kind: StatefulSet\nspec:\n  template:\n    spec:\n      affinity:\n        podAntiAffinity:\n          preferredDuringSchedulingIgnoredDuringExecution: []\n      containers: []\n" +
				"  volumeClaimTemplates:\n    - metadata:\n        name: data\n      spec:\n        accessModes: [ReadWriteOnce]\n",
			"kind: StatefulSet\nspec:\n  template:\n    spec:\n      affinity:\n" + strings.TrimPrefix(at(6), "      affinity:\n") +
				"        podAntiAffinity:\n          preferredDuringSchedulingIgnoredDuringExecution: []\n      containers: []\n" +
				"  volumeClaimTemplates:\n    - metadata:\n        name: data\n      spec:\n        accessModes: [ReadWriteOnce]\n"},
		{"places the pod of a CronJob and of a Job in the next document",
			"---\nkind: CronJob\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers: []\n---\nkind: Job\nspec:\n  template:\n    spec:\n      containers: []\n",
			"---\nkind: CronJob\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n" + at(10) + "          containers: []\n" +
				"---\nkind: Job\nspec:\n  template:\n    spec:\n" + at(6) + "      containers: []\n"},
		{"leaves a DaemonSet, which runs on every node, and a document holding no workload",
			"kind: DaemonSet\nspec:\n  template:\n    spec:\n      containers: []\n---\nkind: ConfigMap\ndata:\n  pod: |\n    spec:\n      template:\n        spec: {}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "rendered.yaml")
			require.NoError(t, os.WriteFile(file, []byte(tc.in), 0o644))

			got, err := testutil.RunBash(t, `. "$1" && keep_off_burst_nodes "$2" && cat "$2"`, os.Environ(), "scripts/lib/burst-placement.sh", file)

			require.NoError(t, err, got)
			if tc.want == "" {
				tc.want = tc.in
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestKeepOffBurstNodes_FailsOnAPodWhoseNodeAffinityItWouldHaveToMerge(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rendered.yaml")
	require.NoError(t, os.WriteFile(file, []byte("kind: Deployment\nmetadata:\n  name: placed\nspec:\n  template:\n    spec:\n      affinity:\n"+
		"        nodeAffinity:\n          preferredDuringSchedulingIgnoredDuringExecution: []\n      containers: []\n"), 0o644))

	got, err := testutil.RunBash(t, `. "$1" && keep_off_burst_nodes "$2"`, os.Environ(), "scripts/lib/burst-placement.sh", file)

	require.Error(t, err, got)
	assert.Contains(t, got, "Deployment placed")
}

// boundToItsVolumesNode is whether pod is the local path provisioner's helper, which the provisioner binds by name to
// the node holding the volume it makes or removes, a burst member's included.
func boundToItsVolumesNode(pod renderedPod) bool {
	return strings.HasSuffix(pod.source, "/local-path-provisioner") && pod.workload == pod.source+" helperPod.yaml"
}

// refusesBurstNodes is whether each required node selector term of spec holds a pod off a node with the burst label.
func refusesBurstNodes(spec map[string]any) bool {
	return refusesNodesLabelled(spec, components.BurstLabel)
}

// refusesNodesLabelled is whether each required node selector term of spec holds a pod off a node with label.
func refusesNodesLabelled(spec map[string]any, label string) bool {
	affinity, _ := spec["affinity"].(map[string]any)
	node, _ := affinity["nodeAffinity"].(map[string]any)
	required, _ := node["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms, _ := required["nodeSelectorTerms"].([]any)
	for _, raw := range terms {
		term, _ := raw.(map[string]any)
		expressions, _ := term["matchExpressions"].([]any)
		if !slices.ContainsFunc(expressions, func(e any) bool {
			m, _ := e.(map[string]any)
			return m["key"] == label && m["operator"] == "DoesNotExist"
		}) {
			return false
		}
	}
	return len(terms) > 0
}

// runsAJob is whether pod is the runner pod of a scale set, which runs a job and may run on a burst member.
func runsAJob(pod renderedPod) bool {
	return pod.kind == "AutoscalingRunnerSet" && strings.HasSuffix(pod.where, ".spec.template.spec")
}

func TestManifests_EveryPodButADaemonSetsStaysOffBurstMembersUnlessItRunsOnTheControlPlane(t *testing.T) {
	var free []string

	for _, pod := range renderedPods(t) {
		selector, _ := pod.spec["nodeSelector"].(map[string]any)
		_, onControlPlane := selector[controlPlaneRole]
		if pod.kind != "DaemonSet" && !onControlPlane && !boundToItsVolumesNode(pod) && !runsAJob(pod) && !refusesBurstNodes(pod.spec) {
			free = append(free, pod.where)
		}
	}

	slices.Sort(free)
	assert.Equal(t, "mitstack.dev/burst", components.BurstLabel, "the label each burst member's node carries")
	assert.Empty(t, free, "a pod free to run on a burst member holds that member out of its idle window, and moves when it goes")
}

func TestManifests_TheControlPlanesCredentialsRenderThePodsTheirInputsCallForEachKeptOffBurstMembers(t *testing.T) {
	bySource := map[string][]renderedPod{}
	for _, pod := range renderedPods(t) {
		bySource[pod.source] = append(bySource[pod.source], pod)
	}

	for _, tc := range []struct {
		name, source string
		want         []string
	}{
		{"a cluster reading its repository with the platform's token and leasing telemetry runs the enrolment, the token renewal and the lease",
			tokenAndLeaseCredentials, []string{"mitstack-enrolment", "mitstack-repository-token", "mitstack-telemetry-lease"}},
		{"a cluster reading its repository with a deploy key and naming no lease resource runs the enrolment alone",
			deployKeyCredentials, []string{"mitstack-enrolment"}},
		{"a cluster that runs runners runs the runner token's lease alone", runnerCredentials, []string{"mitstack-runner-lease"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got, free []string
			for _, pod := range bySource[tc.source] {
				got = append(got, pod.workload)
				if !refusesBurstNodes(pod.spec) {
					free = append(free, pod.where)
				}
			}

			slices.Sort(got)
			assert.Equal(t, tc.want, got, "the pods %s renders", tc.source)
			assert.Empty(t, free, "the pods of %s with no required node affinity %s DoesNotExist", tc.source, components.BurstLabel)
		})
	}
}

func TestManifests_TheRunnerServicesKeepARCsControllerAndTheListenerOffBurstMembersWhileOnlyTheRunnerPodMayRunOnOne(t *testing.T) {
	var controller, listeners, runners []renderedPod
	for _, pod := range renderedPods(t) {
		switch {
		case pod.source == "runners/gha-runner-scale-set-controller":
			controller = append(controller, pod)
		case pod.source == "runners/mitstack-runner-scale-set" && strings.HasSuffix(pod.where, ".spec.listenerTemplate.spec"):
			listeners = append(listeners, pod)
		case pod.source == "runners/mitstack-runner-scale-set" && runsAJob(pod):
			runners = append(runners, pod)
		}
	}

	require.NotEmpty(t, controller, "the controller's pods")
	require.Len(t, listeners, 1, "the listener's pod template")
	require.Len(t, runners, 1, "the runner's pod template")
	for _, pod := range append(controller, listeners...) {
		assert.True(t, refusesBurstNodes(pod.spec), "%s has no required node affinity %s DoesNotExist, and a burst member goes with the idle window", pod.where, components.BurstLabel)
	}
}

// runsOnTheControlPlane is whether spec selects the control-plane nodes and tolerates the NoSchedule taint they keep
// when apps run on workers alone.
func runsOnTheControlPlane(spec map[string]any) bool {
	selector, _ := spec["nodeSelector"].(map[string]any)
	_, selects := selector[controlPlaneRole]
	return selects && toleratesControlPlane(spec)
}

func TestManifests_TheRunnerServicesRunEveryPodThatHoldsTheRunnerTokenOnTheControlPlaneAndEachJobOffIt(t *testing.T) {
	var holders, jobs []renderedPod
	for _, pod := range renderedPods(t) {
		switch {
		case pod.source == "runners/gha-runner-scale-set-controller" && pod.kind == "Deployment",
			pod.source == "runners/mitstack-runner-scale-set" && strings.HasSuffix(pod.where, ".spec.listenerTemplate.spec"),
			pod.source == runnerCredentials && pod.workload == "mitstack-runner-lease":
			holders = append(holders, pod)
		case runsAJob(pod):
			jobs = append(jobs, pod)
		}
	}

	require.Len(t, holders, 3, "the pods of ARC's controller, the listener and the lease job")
	require.Len(t, jobs, 1, "the runner's pod template")
	for _, pod := range holders {
		assert.True(t, runsOnTheControlPlane(pod.spec), "%s holds the runner token, so it selects %s and tolerates its NoSchedule taint, where no job runs",
			pod.where, controlPlaneRole)
	}
	assert.True(t, refusesNodesLabelled(jobs[0].spec, controlPlaneRole), "%s has no required node affinity %s DoesNotExist, so a job may run beside the runner token",
		jobs[0].where, controlPlaneRole)
}

// secretReaders are the subjects, as namespace/name for a service account, group:name for a group and user:name for a
// user, that a rendered binding lets get, list or watch the Secret named secret in namespace.
func secretReaders(t *testing.T, namespace, secret string) []string {
	t.Helper()
	docs := renderedDocs(t)
	rules := map[string][]any{}
	for _, d := range docs {
		if d.doc["kind"] == "Role" || d.doc["kind"] == "ClusterRole" {
			ns, _ := testutil.Field(t, d.doc, "metadata", "namespace").(string)
			key := fmt.Sprintf("%v/%s/%v", d.doc["kind"], ns, testutil.Field(t, d.doc, "metadata", "name"))
			rules[key] = slices.Concat(rules[key], listOf(d.doc["rules"]))
		}
	}
	readers := map[string]bool{}
	for _, d := range docs {
		ns, _ := testutil.Field(t, d.doc, "metadata", "namespace").(string)
		bindingNamespace := ""
		switch {
		// Argo CD applies a Role or RoleBinding rendered without a namespace to its Application's, which may be namespace.
		case d.doc["kind"] == "RoleBinding" && (ns == namespace || ns == ""):
			bindingNamespace = namespace
		case d.doc["kind"] != "ClusterRoleBinding":
			continue
		}
		ref, _ := d.doc["roleRef"].(map[string]any)
		// A ClusterRole's key has no namespace, and a Role rendered without one counts as one in the binding's.
		granted := rules[fmt.Sprintf("%v//%v", ref["kind"], ref["name"])]
		if ref["kind"] == "Role" {
			granted = slices.Concat(rules[fmt.Sprintf("Role/%s/%v", bindingNamespace, ref["name"])], granted)
		}
		if !grantsSecretRead(granted, secret) {
			continue
		}
		for _, raw := range listOf(d.doc["subjects"]) {
			subject, _ := raw.(map[string]any)
			switch subject["kind"] {
			case "Group":
				readers[fmt.Sprintf("group:%v", subject["name"])] = true
			case "User":
				readers[fmt.Sprintf("user:%v", subject["name"])] = true
			default:
				// The API server reads a service account subject with no namespace as one in its binding's namespace.
				subjectNamespace, _ := subject["namespace"].(string)
				if subjectNamespace == "" {
					subjectNamespace = bindingNamespace
				}
				readers[fmt.Sprintf("%s/%v", subjectNamespace, subject["name"])] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(readers))
}

// grantsSecretRead is whether a rule of rules lets its holder get, list or watch the Secret named secret.
func grantsSecretRead(rules []any, secret string) bool {
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		groups, resources, verbs, names := listOf(rule["apiGroups"]), listOf(rule["resources"]), listOf(rule["verbs"]), listOf(rule["resourceNames"])
		core := slices.Contains(groups, any("")) || slices.Contains(groups, any("*"))
		onSecrets := slices.Contains(resources, any("secrets")) || slices.Contains(resources, any("*"))
		reads := slices.ContainsFunc(verbs, func(v any) bool { return v == "get" || v == "list" || v == "watch" || v == "*" })
		named := len(names) == 0 || slices.Contains(names, any(secret))
		if core && onSecrets && reads && named {
			return true
		}
	}
	return false
}

func TestManifests_NoRoleBindingLetsARunnerJobsServiceAccountReadTheRunnerToken(t *testing.T) {
	job := runnerPod(t)
	account := components.RunnersNamespace + "/" + testutil.Field(t, job.spec, "serviceAccountName").(string)

	readers := secretReaders(t, components.RunnersNamespace, components.RunnerTokenSecret)

	assert.Contains(t, readers, components.RunnersNamespace+"/arc-gha-rs-controller", "ARC's controller reads the token to register the scale set, which shows the search finds a reader")
	assert.NotContains(t, readers, account, "the account %s each job runs as", account)
	for _, group := range []string{"group:system:serviceaccounts", "group:system:serviceaccounts:" + components.RunnersNamespace, "group:system:authenticated"} {
		assert.NotContains(t, readers, group, "a group that holds the account %s each job runs as", account)
	}
}

// mountsSecret is whether spec mounts the Secret named secret, as a volume of its own or as a projected source.
func mountsSecret(spec map[string]any, secret string) bool {
	for _, raw := range listOf(spec["volumes"]) {
		volume, _ := raw.(map[string]any)
		if source, _ := volume["secret"].(map[string]any); source["secretName"] == secret {
			return true
		}
		projected, _ := volume["projected"].(map[string]any)
		for _, rawSource := range listOf(projected["sources"]) {
			source, _ := rawSource.(map[string]any)
			if projectedSecret, _ := source["secret"].(map[string]any); projectedSecret["name"] == secret {
				return true
			}
		}
	}
	return false
}

func TestManifests_EveryPodThatMountsTheRunnerSweepsCopyOfTheRunnerTokenRunsOnTheControlPlane(t *testing.T) {
	plane := tokenAndLeasePlane(t, time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC))
	in := runnerInputs()
	plane.Runners = &in
	release, err := plane.Release(controlplane.State{})
	require.NoError(t, err)

	var mounts []string
	for _, m := range release.Components {
		for _, doc := range testutil.Documents(t, m.Bytes) {
			found := map[string]map[string]any{}
			podSpecs(doc, "", found)
			for path, spec := range found {
				if !mountsSecret(spec, components.RunnerSweepTokenSecret) {
					continue
				}
				where := fmt.Sprintf("%s: %v %v%s", m.Name, doc["kind"], testutil.Field(t, doc, "metadata", "name"), path)
				mounts = append(mounts, where)
				assert.True(t, runsOnTheControlPlane(spec), "%s mounts the lease job's copy of the runner token, so it selects %s and tolerates its NoSchedule taint, where no job runs",
					where, controlPlaneRole)
			}
		}
	}
	assert.Len(t, mounts, 1, "the node report's pod alone mounts the copy, which shows the search finds a mount: %v", mounts)
}

// podRequest is the CPU in millicores and the memory in bytes a pod or a container asks the scheduler for.
type podRequest struct{ milliCPU, memory int64 }

func (r podRequest) plus(o podRequest) podRequest {
	return podRequest{r.milliCPU + o.milliCPU, r.memory + o.memory}
}

func (r podRequest) atLeast(o podRequest) podRequest {
	return podRequest{max(r.milliCPU, o.milliCPU), max(r.memory, o.memory)}
}

func (r podRequest) String() string { return fmt.Sprintf("%dm CPU and %dMi", r.milliCPU, r.memory>>20) }

func quantity(t *testing.T, value any) resource.Quantity {
	t.Helper()
	if value == nil {
		return resource.Quantity{}
	}
	q, err := resource.ParseQuantity(fmt.Sprint(value))
	require.NoError(t, err, "the quantity %v", value)
	return q
}

func containerRequest(t *testing.T, container any) podRequest {
	t.Helper()
	resources, _ := container.(map[string]any)["resources"].(map[string]any)
	requests, _ := resources["requests"].(map[string]any)
	cpu, memory := quantity(t, requests["cpu"]), quantity(t, requests["memory"])
	return podRequest{cpu.MilliValue(), memory.Value()}
}

// scheduledRequest is what the scheduler reserves for spec: its containers with its sidecars, or its costliest init
// container with the sidecars started before it, whichever is larger.
func scheduledRequest(t *testing.T, spec map[string]any) podRequest {
	t.Helper()
	var sidecars, initPeak, running podRequest
	for _, container := range listOf(spec["initContainers"]) {
		if container.(map[string]any)["restartPolicy"] == "Always" {
			sidecars = sidecars.plus(containerRequest(t, container))
			continue
		}
		initPeak = initPeak.atLeast(sidecars.plus(containerRequest(t, container)))
	}
	for _, container := range listOf(spec["containers"]) {
		running = running.plus(containerRequest(t, container))
	}
	return running.plus(sidecars).atLeast(initPeak)
}

// serverSizes are the sizes Hetzner publishes for the types a pool may list, which its API reports only at run time.
var serverSizes = map[string]podRequest{
	"cx23": {2000, 4 << 30}, "cpx22": {2000, 4 << 30}, "cx33": {4000, 8 << 30}, "cpx32": {4000, 8 << 30},
	"cx43": {8000, 16 << 30}, "cpx52": {12000, 24 << 30}, "cx53": {16000, 32 << 30}, "cpx62": {16000, 32 << 30},
}

// smallestPoolServer is the smallest size among controlplane.ServerTypes, failing on a type serverSizes does not hold.
func smallestPoolServer(t *testing.T) podRequest {
	t.Helper()
	require.NotEmpty(t, controlplane.ServerTypes)
	smallest := podRequest{milliCPU: math.MaxInt64, memory: math.MaxInt64}
	for _, name := range controlplane.ServerTypes {
		size, ok := serverSizes[name]
		require.True(t, ok, "controlplane.ServerTypes lists %s, whose size the fit test does not know", name)
		smallest = podRequest{min(size.milliCPU, smallest.milliCPU), min(size.memory, smallest.memory)}
	}
	return smallest
}

// kubeletHardEvictionMemory is the kubelet's default memory.available hard eviction threshold, which allocatable leaves out.
const kubeletHardEvictionMemory = 100 << 20

// workerAllocatable is what a Talos worker of size server offers its pods, after the system reservation and the hard eviction threshold.
func workerAllocatable(server podRequest) podRequest {
	cpu, memory := resource.MustParse(constants.KubeletSystemReservedCPU), resource.MustParse(constants.KubeletSystemReservedMemoryWorker)
	return podRequest{milliCPU: server.milliCPU - cpu.MilliValue(), memory: server.memory - memory.Value() - kubeletHardEvictionMemory}
}

func runnerPod(t *testing.T) renderedPod {
	t.Helper()
	var runners []renderedPod
	for _, pod := range renderedPods(t) {
		if runsAJob(pod) {
			runners = append(runners, pod)
		}
	}
	require.Len(t, runners, 1, "the runner's pod template")
	return runners[0]
}

func TestManifests_TheRunnerPodRequestsCPUAndMemoryOnItsRunnerAndItsDockerDaemonSoAQueuedJobWaitsForRoom(t *testing.T) {
	pod := runnerPod(t)

	containers := podContainers(t, pod)

	for _, name := range []string{"runner", "dind"} {
		container, ok := containers[pod.workload+"/"+name]
		require.True(t, ok, "the runner pod's container %s, of %v", name, slices.Sorted(maps.Keys(containers)))
		got := containerRequest(t, container)
		assert.Positive(t, got.milliCPU, "%s requests %s; a container that reserves no CPU always fits, so a queued job never grows the pool", name, got)
		assert.Positive(t, got.memory, "%s requests %s; a container that reserves no memory always fits, so a queued job never grows the pool", name, got)
	}
}

func TestManifests_OneRunnerJobFitsOnTheSmallestPoolServerBesideEveryDaemonSetThatRunsOnABurstMember(t *testing.T) {
	daemons := map[string]podRequest{}
	for _, pod := range renderedPods(t) {
		selector, _ := pod.spec["nodeSelector"].(map[string]any)
		_, onControlPlane := selector[controlPlaneRole]
		if pod.kind == "DaemonSet" && !onControlPlane && !refusesBurstNodes(pod.spec) {
			// The default and hosting sets each render some DaemonSets, and a cluster runs one of them.
			key := pod.namespace + "/" + pod.workload
			daemons[key] = daemons[key].atLeast(scheduledRequest(t, pod.spec))
		}
	}
	require.Contains(t, daemons, "istio-system/ztunnel", "the per-node pods the fit sums, %v", slices.Sorted(maps.Keys(daemons)))
	job := scheduledRequest(t, runnerPod(t).spec)
	require.Positive(t, job.milliCPU, "the runner pod requests %s, and a pod that reserves nothing fits anywhere", job)
	require.Positive(t, job.memory, "the runner pod requests %s, and a pod that reserves nothing fits anywhere", job)

	var sum podRequest
	var each []string
	for _, key := range slices.Sorted(maps.Keys(daemons)) {
		sum = sum.plus(daemons[key])
		each = append(each, fmt.Sprintf("%s %s", key, daemons[key]))
	}
	need := job.plus(sum)
	room := workerAllocatable(smallestPoolServer(t))
	t.Logf("the runner pod asks %s, the DaemonSets %v ask %s, together %s, and the smallest pool server offers %s", job, each, sum, need, room)

	assert.LessOrEqual(t, need.milliCPU, room.milliCPU, "the runner pod's %s with the DaemonSets' %v needs %s, and the smallest pool server offers %s", job, each, need, room)
	assert.LessOrEqual(t, need.memory, room.memory, "the runner pod's %s with the DaemonSets' %v needs %s, and the smallest pool server offers %s", job, each, need, room)
}

func TestManifests_TheRolloutRendersAHandoverPerComponentAndOpenBaosMemberMoveOnlyWhereItCarriesOpenBaoEachKeptOffBurstMembers(t *testing.T) {
	manifests := renderedManifests(t)
	pods := renderedPods(t)

	for _, tc := range []struct {
		name, plane string
		// amongCarried are components the rollout must carry, each of which then gets a handover.
		amongCarried []string
		// besideHandovers are the Jobs the rollout renders that no carried component's handover accounts for.
		besideHandovers []string
	}{
		{"an unhosted plane with no OpenBao renders a handover for each component it carries and no member move",
			unhostedRollout, []string{"mitstack-node-topology"}, nil},
		{"a hosting plane carrying OpenBao renders the member move beside OpenBao's handover",
			hostingRollout, []string{"mitstack-node-topology", components.OpenBaoName}, []string{"mitstack-openbao-rollout"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var carried []string
			for source := range manifests {
				if dir, ok := strings.CutPrefix(source, tc.plane+"/"+rollout.ComponentsDir+"/"); ok {
					carried = append(carried, strings.TrimSuffix(dir, "/"+componentFile))
				}
			}
			want := slices.Clone(tc.besideHandovers)
			for _, name := range carried {
				want = append(want, rollout.ApplicationPrefix+name+"-handover")
			}

			var got, free []string
			for _, pod := range pods {
				if !strings.HasPrefix(pod.source, tc.plane+"/") {
					continue
				}
				got = append(got, pod.workload)
				if !refusesBurstNodes(pod.spec) {
					free = append(free, pod.where)
				}
			}

			assert.Subset(t, carried, tc.amongCarried, "the components %s carries", tc.plane)
			assert.ElementsMatch(t, want, got, "the pods %s renders", tc.plane)
			slices.Sort(free)
			assert.Empty(t, free, "the pods of %s with no required node affinity %s DoesNotExist", tc.plane, components.BurstLabel)
		})
	}
}

func TestFetch(t *testing.T) {
	const archive = "the chart archive's bytes"
	sum := sha256.Sum256([]byte(archive))
	pin := hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refused.tgz" {
			http.Error(w, "Access denied.", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, archive)
	}))
	t.Cleanup(srv.Close)
	caBundle := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caBundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644))

	for _, tc := range []struct{ name, path, pin, want string }{
		{"an archive matching its pin lands at its destination and passes the check there", "/chart.tgz", pin, "exit=0"},
		{"a URL that answers 403 stops with curl's exit status and names the URL", "/refused.tgz", pin, srv.URL + "/refused.tgz (curl exit 22)\nexit=22"},
		{"an archive whose sha256 differs from its pin stops at the check", "/chart.tgz", strings.Repeat("0", 64), "did NOT match\nexit=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "chart.tgz")

			out, err := testutil.RunBash(t, `. "$1"; fetch "$2" "$3" "$4"; echo "exit=$?"`, append(os.Environ(), "CURL_CA_BUNDLE="+caBundle), "scripts/lib/fetch.sh", srv.URL+tc.path, tc.pin, dest)

			require.NoError(t, err, out)
			assert.Contains(t, out, tc.want, "fetch %s%s with pin %s", srv.URL, tc.path, tc.pin)
		})
	}
}

var podRefusingPolicies = []string{"mitstack-disallow-host-path", "mitstack-require-resource-limits", "mitstack-restrict-image-registries"}

// renderedPod is a pod spec the kit renders in a document, or in a pod template a ConfigMap of the kit holds, with the
// manifest and the object that hold it and the namespace it starts in.
type renderedPod struct {
	where     string
	source    string
	kind      any
	workload  string
	namespace string
	spec      map[string]any
}

func renderedPods(t *testing.T) []renderedPod {
	t.Helper()
	var pods []renderedPod
	add := func(source string, doc map[string]any, namespace string) {
		found := map[string]map[string]any{}
		podSpecs(doc, "", found)
		for path, spec := range found {
			if _, ok := spec["containers"].([]any); !ok {
				continue
			}
			name := fmt.Sprint(testutil.Field(t, doc, "metadata", "name"))
			pods = append(pods, renderedPod{where: fmt.Sprintf("%s: %v %s%s", source, doc["kind"], name, path), source: source, kind: doc["kind"],
				workload: name, namespace: namespace, spec: spec})
		}
	}
	for _, d := range renderedDocs(t) {
		if d.doc["kind"] == "CustomResourceDefinition" {
			continue
		}
		namespace, _ := testutil.Field(t, d.doc, "metadata", "namespace").(string)
		add(d.source, d.doc, namespace)
		if data, ok := d.doc["data"].(map[string]any); ok {
			for key, value := range data {
				if text, ok := value.(string); ok && strings.Contains(text, "containers:") {
					for _, embedded := range testutil.Documents(t, []byte(text)) {
						embedded["metadata"] = map[string]any{"name": d.source + " " + key}
						add(d.source, embedded, namespace)
					}
				}
			}
		}
	}
	return pods
}

func podPolicy(t *testing.T, name string) map[string]any {
	t.Helper()
	return testutil.Documents(t, testutil.Manifest(t, name, components.Default().Inline).Bytes)[0]
}

// runnerPodPolicy is a pod policy as a cluster that runs runners holds it, which also skips the runners' namespace.
func runnerPodPolicy(t *testing.T, name string) map[string]any {
	t.Helper()
	set := components.Default().WithRunnerServices()
	return testutil.Documents(t, testutil.Manifest(t, name, set.Served, set.Inline).Bytes)[0]
}

func policySkips(t *testing.T, policy map[string]any) []string {
	t.Helper()
	var skipped []string
	for _, v := range testutil.Field(t, policy, "spec", "matchConstraints", "namespaceSelector", "matchExpressions", "0", "values").([]any) {
		skipped = append(skipped, v.(string))
	}
	return skipped
}

func refusals(t *testing.T, pod renderedPod) []string {
	t.Helper()
	var refused []string
	for _, name := range podRefusingPolicies {
		if !testutil.Admits(t, podPolicy(t, name), map[string]any{"spec": pod.spec}, map[string]any{"metadata": map[string]any{"name": pod.namespace}}) {
			refused = append(refused, name)
		}
	}
	return refused
}

func TestManifests_NoPodOutsideTheSkippedNamespacesIsRefusedByTheHostPathLimitsOrRegistryPolicies(t *testing.T) {
	skipped := policySkips(t, runnerPodPolicy(t, podRefusingPolicies[0]))
	var refused []string

	for _, pod := range renderedPods(t) {
		if slices.Contains(skipped, pod.namespace) {
			continue
		}
		for _, policy := range refusals(t, pod) {
			refused = append(refused, fmt.Sprintf("%s in %q is refused by %s", pod.where, pod.namespace, policy))
		}
	}

	slices.Sort(refused)
	assert.Empty(t, refused, "a pod the kit renders would stop at its next restart")
}

const digestPolicy = "mitstack-require-image-digest"

func TestManifests_NoPodOutsideTheNamespacesTheLabelPolicySkipsNamesAnImageWithoutItsDigest(t *testing.T) {
	policy := podPolicy(t, digestPolicy)
	skipped := policySkips(t, policy)
	var refused []string

	for _, pod := range renderedPods(t) {
		if slices.Contains(skipped, pod.namespace) {
			continue
		}
		if !testutil.Admits(t, policy, map[string]any{"spec": pod.spec}, map[string]any{"metadata": map[string]any{"name": pod.namespace}}) {
			refused = append(refused, fmt.Sprintf("%s in %q", pod.where, pod.namespace))
		}
	}

	slices.Sort(refused)
	assert.Empty(t, refused, "a pod the kit renders would stop at its next restart")
	assert.Equal(t, policySkips(t, podPolicy(t, "mitstack-require-app-label")), skipped, "the digest policy skips other namespaces than the label policy and Kyverno's webhooks")
}

func TestManifests_EveryNamespaceTheNewPolicySkipsBeyondTheLabelPolicysHoldsAPodTheyWouldRefuse(t *testing.T) {
	label := policySkips(t, podPolicy(t, "mitstack-require-app-label"))
	refusedIn := map[string][]string{}
	for _, pod := range renderedPods(t) {
		if refused := refusals(t, pod); len(refused) > 0 {
			refusedIn[pod.namespace] = append(refusedIn[pod.namespace], pod.where)
		}
	}

	for _, namespace := range policySkips(t, runnerPodPolicy(t, podRefusingPolicies[0])) {
		if slices.Contains(label, namespace) {
			continue
		}
		assert.NotEmpty(t, refusedIn[namespace], "the policies skip %s, but no pod the kit renders there would be refused", namespace)
	}
}

func TestManifests_TheSecurityScanRunsInKubeSystemWhichEveryPodPolicySkips(t *testing.T) {
	var scan []renderedPod
	for _, pod := range renderedPods(t) {
		if strings.HasPrefix(pod.where, "report: CronJob mitstack-security-scan.") {
			scan = append(scan, pod)
		}
	}
	require.Len(t, scan, 1, "the security scan's pod spec")

	for _, name := range podRefusingPolicies {
		assert.Contains(t, policySkips(t, podPolicy(t, name)), scan[0].namespace, "%s judges the security scan's pod in %s", name, scan[0].namespace)
	}
	assert.Equal(t, "kube-system", scan[0].namespace)
}

// quoted lists the single-quoted strings of a policy variable's CEL list, in order.
func quoted(t *testing.T, policy map[string]any, variable string) []string {
	t.Helper()
	for _, v := range testutil.Field(t, policy, "spec", "variables").([]any) {
		if testutil.Field(t, v, "name") != variable {
			continue
		}
		var found []string
		for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(testutil.Field(t, v, "expression").(string), -1) {
			found = append(found, m[1])
		}
		return found
	}
	require.Failf(t, "no variable", "policy %v declares no variable %s", testutil.Field(t, policy, "metadata", "name"), variable)
	return nil
}

func TestManifests_ThePodPoliciesSkipTheSameNamespacesAndNoneThatHoldsAnApp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy func(*testing.T, string) map[string]any
	}{
		{"a cluster that runs no runners", podPolicy},
		{"a cluster that runs runners", runnerPodPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label := policySkips(t, tc.policy(t, "mitstack-require-app-label"))
			var first []string
			for _, name := range podRefusingPolicies {
				skipped := policySkips(t, tc.policy(t, name))
				if first == nil {
					first = skipped
				}

				assert.Equal(t, first, skipped, "%s skips other namespaces than %s", name, podRefusingPolicies[0])
				assert.Subset(t, skipped, label, "%s must leave alone every namespace the label policy does", name)
				for _, namespace := range skipped {
					assert.False(t, strings.HasPrefix(namespace, "app-"), "%s skips %s, which holds an app", name, namespace)
					assert.NotEqual(t, "mitstack-admission-probe", namespace, "%s skips the namespace its own fixtures are tried in", name)
				}
			}
		})
	}
}

func readmeTable(t *testing.T, header string) [][]string {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	var rows [][]string
	inTable := false
	for _, line := range strings.Split(string(readme), "\n") {
		switch {
		case strings.HasPrefix(line, header):
			inTable = true
		case inTable && !strings.HasPrefix(line, "|"):
			return rows
		case inTable && !strings.HasPrefix(line, "|---"):
			rows = append(rows, strings.Split(strings.Trim(line, "| "), " | "))
		}
	}
	return rows
}

func TestManifests_TheReadmeNamesEachSkippedNamespaceWithWhatRunsThereAndWhy(t *testing.T) {
	rows := map[string][]string{}
	for _, cells := range readmeTable(t, "| Namespace | What runs there | Why the pod policies skip it |") {
		rows[strings.Trim(cells[0], "`")] = cells[1:]
	}

	skipped := policySkips(t, runnerPodPolicy(t, podRefusingPolicies[0]))

	var documented []string
	for namespace, cells := range rows {
		documented = append(documented, namespace)
		require.Len(t, cells, 2, "the row for %s", namespace)
		assert.NotEmpty(t, strings.TrimSpace(cells[0]), "%s: what runs there", namespace)
		assert.NotEmpty(t, strings.TrimSpace(cells[1]), "%s: why the policies skip it", namespace)
	}
	assert.ElementsMatch(t, skipped, documented, "the README's table of skipped namespaces differs from the policies' list")
}

func podContainers(t *testing.T, pod renderedPod) map[string]map[string]any {
	t.Helper()
	containers := map[string]map[string]any{}
	for _, list := range []string{"initContainers", "containers"} {
		for _, container := range listOf(pod.spec[list]) {
			containers[fmt.Sprintf("%s/%v", pod.workload, testutil.Field(t, container, "name"))] = container.(map[string]any)
		}
	}
	return containers
}

func containerFaults(name string, container map[string]any, want []string) []string {
	context, _ := container["securityContext"].(map[string]any)
	capabilities, _ := context["capabilities"].(map[string]any)
	var added, faults []string
	for _, capability := range listOf(capabilities["add"]) {
		added = append(added, capability.(string))
	}
	slices.Sort(added)
	if context["privileged"] == true {
		faults = append(faults, name+" runs privileged")
	}
	if !slices.Contains(listOf(capabilities["drop"]), any("ALL")) {
		faults = append(faults, name+" keeps the container runtime's default capabilities, since it does not drop ALL")
	}
	if !slices.Equal(added, want) {
		faults = append(faults, fmt.Sprintf("%s adds %v, and the README records %v", name, added, want))
	}
	return faults
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

// A component set that makes a Cilium container privileged fails this test, which names the container, and a cluster
// built from either set runs Cilium with only the capabilities the README records.
func TestManifests_NoCiliumContainerRunsPrivilegedOrAddsACapabilityBeyondTheReadmesList(t *testing.T) {
	documented := map[string][]string{}
	for _, cells := range readmeTable(t, "| Workload | Container | Capabilities it adds |") {
		var capabilities []string
		for _, m := range regexp.MustCompile("`([A-Z_]+)`").FindAllStringSubmatch(cells[2], -1) {
			capabilities = append(capabilities, m[1])
		}
		slices.Sort(capabilities)
		documented[strings.Trim(cells[0], "`")+"/"+strings.Trim(cells[1], "`")] = capabilities
	}
	rendered := map[string][]string{}
	var faults []string

	for _, pod := range renderedPods(t) {
		if !strings.HasSuffix(pod.source, "/cilium") {
			continue
		}
		for name, container := range podContainers(t, pod) {
			rendered[pod.source] = append(rendered[pod.source], name)
			for _, fault := range containerFaults(name, container, documented[name]) {
				faults = append(faults, pod.source+": "+fault)
			}
		}
	}

	slices.Sort(faults)
	assert.Empty(t, strings.Join(faults, "\n"), "Cilium holds more of the node than the README accounts for")
	for _, source := range []string{"default/cilium", "hosting/cilium"} {
		assert.ElementsMatch(t, slices.Collect(maps.Keys(documented)), rendered[source], "the README's table lists other Cilium containers than %s renders", source)
	}
}

// The scan's privileged-container control, C-0057, fails a container that adds SYS_ADMIN.
func TestManifests_CiliumEnvoyAddsNetAdminAndBPFInPlaceOfSysAdmin(t *testing.T) {
	added := map[string]any{}
	for _, pod := range renderedPods(t) {
		if envoy, ok := podContainers(t, pod)["cilium-envoy/cilium-envoy"]; ok {
			added[pod.source] = testutil.Field(t, envoy, "securityContext", "capabilities", "add")
		}
	}

	want := []any{"NET_ADMIN", "BPF"}
	assert.Equal(t, map[string]any{"default/cilium": want, "hosting/cilium": want}, added, "the capabilities each set's cilium-envoy container adds")
}

func TestManifests_EveryRefusalOpensWithTheNameOfTheRuleThePlatformLooksFor(t *testing.T) {
	for _, name := range append(slices.Clone(podRefusingPolicies), digestPolicy) {
		for _, v := range testutil.Field(t, podPolicy(t, name), "spec", "validations").([]any) {
			assert.True(t, strings.HasPrefix(testutil.Field(t, v, "message").(string), name+": "), "%s refuses with a message that does not open with its name", name)
		}
	}
}

// judgesAsItsTwin holds policy to the one pods rule Kyverno's CanAutoGen needs to extend it to the controllers that
// make pods, and twin to the same checks of the update of pods/ephemeralcontainers that kubectl debug sends.
func judgesAsItsTwin(t *testing.T, policy, twin map[string]any, checks ...string) {
	t.Helper()
	rule := func(resource string, operations ...any) []any {
		return []any{map[string]any{"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": operations, "resources": []any{resource}}}
	}
	assert.Equal(t, rule("pods", "CREATE", "UPDATE"), testutil.Field(t, policy, "spec", "matchConstraints", "resourceRules"), "the policy's resource rules")
	assert.Equal(t, rule("pods/ephemeralcontainers", "UPDATE"), testutil.Field(t, twin, "spec", "matchConstraints", "resourceRules"), "the twin's resource rules")
	assert.Equal(t, policy["kind"], twin["kind"])
	assert.Equal(t, false, testutil.Field(t, twin, "spec", "evaluation", "background", "enabled"), "a background scan leaves the twin not ready")
	assert.Equal(t, policySkips(t, policy), policySkips(t, twin), "the namespaces the twin skips")
	for _, field := range checks {
		assert.Equal(t, testutil.Field(t, policy, "spec", field), testutil.Field(t, twin, "spec", field), "the twin's %s", field)
	}
}

func TestManifests_APodPolicyThatReadsEphemeralContainersJudgesTheUpdateKubectlDebugAddsThemThrough(t *testing.T) {
	var judged []string
	for _, m := range components.Default().Inline {
		docs := testutil.Documents(t, m.Bytes)
		if len(docs) == 0 || docs[0]["kind"] != "ValidatingPolicy" || !strings.Contains(string(m.Bytes), "ephemeralContainers") {
			continue
		}
		judged = append(judged, m.Name)
		t.Run(m.Name, func(t *testing.T) {
			require.Len(t, docs, 2, "the policy and its twin")
			judgesAsItsTwin(t, docs[0], docs[1], "validationActions", "variables", "validations")
		})
	}

	assert.ElementsMatch(t, []string{digestPolicy, "mitstack-restrict-image-registries"}, judged, "the pod policies that read ephemeral containers")
}

// pinnedBusybox is the one busybox reference the local path provisioner and the mesh probe both pin.
func pinnedBusybox(t *testing.T) string {
	t.Helper()
	set := components.Default()
	var refs []string
	for _, name := range []string{"local-path-provisioner", "mitstack-mesh-probe"} {
		found := regexp.MustCompile(`docker\.io/library/busybox[^"\s]*`).FindAllString(string(testutil.Manifest(t, name, set.Served, set.Inline).Bytes), -1)
		require.NotEmpty(t, found, "the %s manifest pins no busybox image", name)
		refs = append(refs, found...)
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(refs)))
	require.Len(t, distinct, 1, "the kit's manifests pin more than one busybox reference")
	return distinct[0]
}

func TestManifests_RestrictImageRegistriesNamesExactlyTheImagesTheKitPinsForPodsInAnAppNamespace(t *testing.T) {
	cnpg := string(testutil.Manifest(t, "cloudnative-pg", components.Default().Served, components.Default().Inline).Bytes)
	hosting, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	certManager := string(testutil.Manifest(t, "cert-manager", hosting.Served, hosting.Inline).Bytes)
	var want []string
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*POSTGRES_IMAGE_NAME: (\S+)$`),
		regexp.MustCompile(`(?m)^\s*PGBOUNCER_IMAGE_NAME: (\S+)$`),
		regexp.MustCompile(`(?m)^\s*- name: OPERATOR_IMAGE_NAME\n\s+value: "?([^"\s]+)"?$`),
	} {
		found := re.FindStringSubmatch(cnpg)
		require.NotNil(t, found, "the CloudNativePG manifest sets no image for %s", re)
		want = append(want, found[1])
	}
	solver := regexp.MustCompile(`--acme-http01-solver-image=(\S+)`).FindStringSubmatch(certManager)
	require.NotNil(t, solver, "cert-manager sets no solver image")
	sidecar := regexp.MustCompile(`(?m)^\s*SIDECAR_IMAGE: "?([^"\s]+)"?$`).FindStringSubmatch(string(testutil.Manifest(t, "plugin-barman-cloud", hosting.Served).Bytes))
	require.NotNil(t, sidecar, "the Barman Cloud plugin sets no sidecar image")
	want = append(want, solver[1], sidecar[1], pinnedBusybox(t))

	got := quoted(t, podPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")

	assert.ElementsMatch(t, want, got, "the images the kit pins for pods in an app namespace differ from the policy's exact list")
}

func barmanCloudPlugin(t *testing.T) (operator, service, deployment map[string]any) {
	t.Helper()
	hosting, err := components.Hosting(testutil.HostingInputs(t))
	require.NoError(t, err)
	plugin := testutil.Documents(t, testutil.Manifest(t, "plugin-barman-cloud", hosting.Served).Bytes)
	operator = testutil.Kind(testutil.Documents(t, testutil.Manifest(t, "cloudnative-pg", hosting.Served).Bytes), "Deployment")[0]
	return operator, testutil.Kind(plugin, "Service")[0], testutil.Kind(plugin, "Deployment")[0]
}

func TestManifests_TheBarmanCloudPluginServesInTheOperatorsNamespaceUnderTheNameAClusterAsksForItBy(t *testing.T) {
	operator, service, _ := barmanCloudPlugin(t)

	assert.Equal(t, testutil.Field(t, operator, "metadata", "namespace"), testutil.Field(t, service, "metadata", "namespace"), "CloudNativePG reads a plugin's Service only in the namespace its operator runs in")
	assert.Equal(t, "barman-cloud.cloudnative-pg.io", testutil.Field(t, service, "metadata", "labels", "cnpg.io/pluginName"), "the name a Cluster asks for the plugin by, which the node report lists")
}

func TestManifests_TheBarmanCloudPluginsPodSetsTheLimitsThePolicyAsksOfAnAppsPodThoughItsNamespaceIsSkipped(t *testing.T) {
	_, _, deployment := barmanCloudPlugin(t)

	pod := map[string]any{"spec": testutil.Field(t, deployment, "spec", "template", "spec")}

	assert.True(t, testutil.Admits(t, podPolicy(t, "mitstack-require-resource-limits"), pod, map[string]any{"metadata": map[string]any{"name": "app-shop"}}), "the plugin's container sets no CPU or no memory limit")
}

func TestManifests_PodPoliciesJudgeWhatAPodMountsSetsAndPulls(t *testing.T) {
	const (
		mitstack = "ghcr.io/mitstack-eu/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		ecr      = "839001574838.dkr.ecr.eu-central-1.amazonaws.com/mitstack-plain@sha256:1602f9e6f55dc68469e142ae6069499a597e74d0ed5425adea9ab9400e9061ff"
		zero     = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	)
	pinned := quoted(t, podPolicy(t, "mitstack-restrict-image-registries"), "pinnedImages")
	postgres := pinned[slices.IndexFunc(pinned, func(s string) bool { return strings.Contains(s, "postgresql:") })]
	sidecarAt := slices.IndexFunc(pinned, func(s string) bool { return strings.Contains(s, "plugin-barman-cloud-sidecar:") })
	require.GreaterOrEqual(t, sidecarAt, 0, "the policy pins no Barman Cloud sidecar image")
	sidecar := pinned[sidecarAt]
	sidecarTag, _, _ := strings.Cut(sidecar, "@")
	busybox := pinnedBusybox(t)
	busyboxTag, busyboxDigest, _ := strings.Cut(busybox, "@")
	limits := map[string]any{"limits": map[string]any{"cpu": "100m", "memory": "64Mi"}}
	container := func(image string, resources map[string]any) map[string]any {
		return map[string]any{"name": "c", "image": image, "resources": resources}
	}
	pod := func(containers ...any) map[string]any {
		return map[string]any{"spec": map[string]any{"containers": containers}}
	}
	with := func(p map[string]any, field string, value any) map[string]any {
		p["spec"].(map[string]any)[field] = value
		return p
	}
	namespace := map[string]any{"metadata": map[string]any{"name": "app-shop"}}

	for _, tc := range []struct {
		name   string
		policy string
		pod    map[string]any
		want   bool
	}{
		{"a pod that mounts an emptyDir is admitted", "mitstack-disallow-host-path", with(pod(container(mitstack, limits)), "volumes", []any{map[string]any{"name": "v", "emptyDir": map[string]any{}}}), true},
		{"a pod that mounts a host path is refused", "mitstack-disallow-host-path", with(pod(container(mitstack, limits)), "volumes", []any{map[string]any{"name": "v", "hostPath": map[string]any{"path": "/var"}}}), false},
		{"a pod with no volumes is admitted by the host path rule", "mitstack-disallow-host-path", pod(container(mitstack, limits)), true},
		{"a container with a CPU and a memory limit is admitted", "mitstack-require-resource-limits", pod(container(mitstack, limits)), true},
		{"a container with no resources is refused", "mitstack-require-resource-limits", pod(container(mitstack, map[string]any{})), false},
		{"a container with a CPU limit and no memory limit is refused", "mitstack-require-resource-limits", pod(container(mitstack, map[string]any{"limits": map[string]any{"cpu": "1"}})), false},
		{"a container with requests alone is refused, since a LimitRange fills a Pod and not its Deployment", "mitstack-require-resource-limits", pod(container(mitstack, map[string]any{"requests": map[string]any{"cpu": "1", "memory": "1Gi"}})), false},
		{"an init container with no limits is refused", "mitstack-require-resource-limits", with(pod(container(mitstack, limits)), "initContainers", []any{container(mitstack, map[string]any{})}), false},
		{"an image of mitstack's ghcr.io organisation is admitted", "mitstack-restrict-image-registries", pod(container(mitstack, limits)), true},
		{"an image of mitstack's ECR registry is admitted", "mitstack-restrict-image-registries", pod(container(ecr, limits)), true},
		{"an image from Docker Hub is refused", "mitstack-restrict-image-registries", pod(container("docker.io/library/busybox"+zero, limits)), false},
		{"an image of a ghcr.io organisation that only begins with mitstack-eu is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/mitstack-eu-fork/app"+zero, limits)), false},
		{"an init container from another registry is refused", "mitstack-restrict-image-registries", with(pod(container(mitstack, limits)), "initContainers", []any{container("quay.io/x/y:1", limits)}), false},
		{"an ephemeral container from another registry is refused", "mitstack-restrict-image-registries", with(pod(container(mitstack, limits)), "ephemeralContainers", []any{container("quay.io/x/y:1", limits)}), false},
		{"the Postgres image CloudNativePG starts, by its pinned digest, is admitted", "mitstack-restrict-image-registries", pod(container(postgres, limits)), true},
		{"the same Postgres image by tag alone is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie", limits)), false},
		{"the busybox image the kit pins, by its tag and digest, is admitted", "mitstack-restrict-image-registries", pod(container(busybox, limits)), true},
		{"the same busybox image by tag alone is refused", "mitstack-restrict-image-registries", pod(container(busyboxTag, limits)), false},
		{"the same busybox tag at another digest is refused", "mitstack-restrict-image-registries", pod(container(busyboxTag+zero, limits)), false},
		{"the same busybox digest under another tag is refused", "mitstack-restrict-image-registries", pod(container("docker.io/library/busybox:1.36.1@"+busyboxDigest, limits)), false},
		{"another Docker Hub image at the busybox digest is refused", "mitstack-restrict-image-registries", pod(container("docker.io/library/alpine:3.22.0@"+busyboxDigest, limits)), false},
		{"the pinned busybox image in an init container is admitted", "mitstack-restrict-image-registries", with(pod(container(mitstack, limits)), "initContainers", []any{container(busybox, limits)}), true},
		{"the same Postgres image at another digest is refused", "mitstack-restrict-image-registries", pod(container("ghcr.io/cloudnative-pg/postgresql:16.15-system-trixie"+zero, limits)), false},
		{"the Barman Cloud sidecar the plugin adds to a Postgres pod as an init container, by its pinned digest, is admitted", "mitstack-restrict-image-registries", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecar, limits)}), true},
		{"the same sidecar by tag alone is refused", "mitstack-restrict-image-registries", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecarTag, limits)}), false},
		{"the same sidecar at another digest is refused", "mitstack-restrict-image-registries", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecarTag+zero, limits)}), false},
		{"the plugin's own image, which runs only in the operator's namespace, is refused in an app namespace", "mitstack-restrict-image-registries", pod(container(strings.Replace(sidecar, "-sidecar:", ":", 1), limits)), false},
		{"a Postgres pod whose sidecar sets no limits is refused", "mitstack-require-resource-limits", with(pod(container(postgres, limits)), "initContainers", []any{container(sidecar, map[string]any{})}), false},
		{"an image named by its digest alone is admitted by the digest rule", digestPolicy, pod(container(mitstack, limits)), true},
		{"an image named by its tag and its digest is admitted", digestPolicy, pod(container(busybox, limits)), true},
		{"the tools image this release pins is admitted", digestPolicy, pod(container(tools.Image, limits)), true},
		{"an image named by its tag alone is refused", digestPolicy, pod(container(busyboxTag, limits)), false},
		{"an image named with neither tag nor digest is refused", digestPolicy, pod(container("ghcr.io/mitstack-eu/app", limits)), false},
		{"an image whose digest is cut short is refused", digestPolicy, pod(container("ghcr.io/mitstack-eu/app@sha256:00000000", limits)), false},
		{"an image whose tag only reads like a digest is refused", digestPolicy, pod(container("ghcr.io/mitstack-eu/app:sha256-0000000000000000000000000000000000000000000000000000000000000000", limits)), false},
		{"an init container named by its tag is refused by the digest rule", digestPolicy, with(pod(container(mitstack, limits)), "initContainers", []any{container(busyboxTag, limits)}), false},
		{"an ephemeral container named by its tag is refused by the digest rule", digestPolicy, with(pod(container(mitstack, limits)), "ephemeralContainers", []any{container(busyboxTag, limits)}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, testutil.Admits(t, podPolicy(t, tc.policy), tc.pod, namespace))
		})
	}
}

func TestManifests_TheRunnerServicesRenderNoGitHubAppKeyAndNoTokenValueAndTheTokenSecretEmpty(t *testing.T) {
	key := regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	token := regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)
	manifests := renderedManifests(t)
	require.Contains(t, manifests, "runners/mitstack-runner-scale-set", "the scale set")
	require.Contains(t, manifests, runnerCredentials, "the runner token's manifest")
	var credentialed []string

	for source, manifest := range manifests {
		if !strings.HasPrefix(source, "runners/") && source != runnerCredentials {
			continue
		}
		if key.Match(manifest) {
			credentialed = append(credentialed, source+" holds a private key")
		}
		if token.Match(manifest) {
			credentialed = append(credentialed, source+" holds a GitHub token")
		}
	}
	for _, doc := range testutil.Kind(testutil.Documents(t, manifests[runnerCredentials]), "Secret") {
		assert.Nil(t, doc["data"], "the Secret %v holds a value", testutil.Field(t, doc, "metadata", "name"))
		assert.Nil(t, doc["stringData"], "the Secret %v holds a value", testutil.Field(t, doc, "metadata", "name"))
	}

	slices.Sort(credentialed)
	assert.Empty(t, credentialed, "the lease Job fills the token at run time, and nothing the kit commits holds a credential")
}
