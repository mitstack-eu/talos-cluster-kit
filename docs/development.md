# Developing the Talos cluster kit

This page is for anyone changing talos-cluster-kit: where the component set
comes from, where each script lives, how its tests run it, and what CI proves
before a release is tagged. Every path below is relative to the repository
root.

Back to [the README](../README.md).

A block, a board and the other workspace terms this page uses are defined in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md).

## Where the component set comes from

The component set is the manifests every Talos cluster starts with.
The table lists its files. `scripts/render-components.sh` writes the first
six rows from pinned sources, each checked against a pinned sha256, and refuses
any image that is not named by digest. The CloudNativePG files are copied from
a release, and the rest are written by hand. The hosting set's files follow in
a second table. [The README's mesh section](../README.md#how-every-call-between-pods-proves-who-makes-it)
explains the mesh's terms used here: the CNI agent, ztunnel, port 15008, the
exceptions for callers with no identity, and a Postgres instance's port 8000.

| File | Source | What it runs |
|---|---|---|
| `components/cilium/cilium.yaml` | The cilium 1.19.9 chart with `components/render/cilium-values.yaml` | Cilium as the network plugin and in place of kube-proxy, with Hubble off so the chart mints no key, and with no privileged container, as [the README](../README.md#what-cilium-holds-of-each-node) explains |
| `components/cilium/policy-crd.yaml` | Cilium v1.19.9's `CiliumClusterwideNetworkPolicy` CRD | The CRD the metadata policy needs before Cilium's operator starts |
| `components/argocd/argocd.yaml` | The argo-cd 10.10.5 chart with `components/render/argocd-values.yaml`, Dex and the local `admin` account off | Argo CD v3.5.5 in namespace `argocd`, which no password signs in to and which keeps each object's health on its Application, with a health check that reads a CloudNativePG Cluster passing through a switchover, failover, upgrade or configuration change as Progressing |
| `components/kyverno/kyverno.yaml` | The kyverno 3.9.1 chart with `components/render/kyverno-values.yaml`, its tests and Helm hooks off | Kyverno v1.19.1 in namespace `kyverno`, its images from `ghcr.io` and its webhooks skipping the system namespaces |
| `components/local-path-provisioner/local-path-provisioner.yaml` | The local-path-provisioner 0.0.37 chart with `components/render/local-path-provisioner-values.yaml`, taken from the source archive of the commit the project's v0.0.37 tag names, since the project publishes no chart repository | Rancher's local path provisioner in namespace `local-path-storage`, behind the default StorageClass `local-path`, keeping each claim on the user volume `local-path-provisioner` |
| `components/istio/istio-base.yaml`, `istiod.yaml`, `istio-cni.yaml`, `ztunnel.yaml` | Istio's base, istiod, cni and ztunnel 1.30.7 charts, each with its values file in `components/render/` | Istio in ambient mode in namespace `istio-system`: its CRDs, istiod with no sidecar injector, the CNI agent that puts every pod outside `kube-system`, `istio-system` and `kyverno` in the mesh, and ztunnel on every node |
| `components/cloudnative-pg/cloudnative-pg.yaml`, `components/cloudnative-pg/namespace.yaml` | Copied from mitstack-eu/cluster-bases v0.4.0, with the operator's header and `POSTGRES_IMAGE_NAME` changed, and the closing comment line the render script gives a document that ends in a block scalar, which the paragraph below the table shows | The CloudNativePG 1.30.1 operator in namespace `cnpg-system`, with its PgBouncer image and its default Postgres image by digest. The default is Postgres 16.15, the image postgres-db-kube pins, so the gateway mirrors it |
| `components/admission/policies/` | Written by hand | The Kyverno policies that require the `mitstack.dev/app` label, give each app namespace `mitstack-default-deny`, refuse a pod the mesh leaves out, refuse a PeerAuthentication that takes a call with no workload identity, and refuse a pod outside the system namespaces that mounts a host path, sets no CPU or memory limit or names an image outside mitstack's registries. The three pod policies skip the system namespaces and five more, which [the README's table of skipped namespaces](../README.md#which-namespaces-the-pod-policies-skip) lists |
| `components/mesh/strict-mtls.yaml`, `probes.yaml`, `cloudnative-pg.yaml`, `argocd.yaml` | Written by hand | The mesh-wide `STRICT` PeerAuthentication, the Cilium policy that admits the kubelet's probes from `169.254.7.127`, the CloudNativePG operator's webhook exception and the ports a Postgres instance takes, and the network policies that admit the mesh's port 15008 between Argo CD's pods |
| `components/mesh/probe.yaml` | Written by hand | The mesh probe's target in namespace `mitstack-mesh-probe`, on the local path provisioner's busybox image, and the Cilium policy that admits only the node report and its outside probe in `kube-system` to it, as [the README](../README.md#how-the-node-report-shows-the-mesh-refusing-a-call) explains |
| `components/network-policies/mitstack-database-slices.yaml` | Written by hand | The Cilium policy that opens a database slice's Postgres instances to each other, the operator and the app slices |
| `components/local-path-provisioner/namespace.yaml`, `components/local-path-provisioner/flows.yaml` | Written by hand | The provisioner's namespace at the privileged Pod Security level, and the Cilium policy that lets its pods reach the API server |
| `components/metadata/metadata-block.yaml` | Written by hand | The policy that drops pod traffic to `169.254.169.254` and isolates nothing else |
| `components/logs/collector.yaml` | Written by hand, as a template `components.LogCollector` fills | The node-local OpenTelemetry Collector on the host network |

`components.Hosting` adds these files for a cluster that hosts the platform.
The script renders each chart with its CRDs and without its tests, and pins
each chart with the sha256 its repository's `index.yaml` publishes.

| File | Source | What it runs |
|---|---|---|
| `components/openbao/openbao.yaml` | The openbao 0.30.2 chart with `components/render/openbao-values.yaml` | OpenBao 2.7.1 in namespace `openbao`: three Raft members on TLS with the static seal |
| `components/cert-manager/cert-manager.yaml` | The cert-manager v1.21.2 chart with `components/render/cert-manager-values.yaml`, its startup check off | cert-manager in namespace `cert-manager` |
| `components/trust-manager/trust-manager.yaml` | The trust-manager v0.25.0 chart with `components/render/trust-manager-values.yaml` | trust-manager in namespace `cert-manager`, reading Secrets only in `openbao` |
| `components/external-secrets/external-secrets.yaml` | The external-secrets 2.11.0 chart with `components/render/external-secrets-values.yaml` | External Secrets in namespace `external-secrets`, its webhook certificate from cert-manager |
| `components/sealed-secrets/sealed-secrets.yaml` | The sealed-secrets 2.20.0 chart with `components/render/sealed-secrets-values.yaml` | The Sealed Secrets controller in `kube-system`, with key renewal off |
| `components/traefik/traefik.yaml` | The traefik 41.6.1 chart with `components/render/traefik-values.yaml` | Traefik on every node in namespace `traefik`, on NodePorts 30080 and 30081. `Set.PublishingIngress` in `components/hosting.go` replaces the chart's published Service argument with the ingress load balancer's address for the files a block commits, and the gateways serve the file as rendered |
| `components/hosting/tls.yaml`, `components/hosting/flows.yaml`, `components/hosting/mesh.yaml` | Written by hand | OpenBao's authority and certificate, its trust bundle, the flows the hosting namespaces allow, the ports OpenBao takes, and the webhook and Traefik ports that take a call with no workload identity |
| `components/hosting/seal-key.yaml`, `sealed-secrets-key.yaml`, `deploy-key.yaml`, `cluster-repository.yaml`, `storage.yaml` | Written by hand, as templates `components.Hosting` fills | The inline manifests that carry the install's secrets, the deploy keys, Argo CD's credential for the cluster repository and OpenBao's volumes |
| `openbaoinit/openbao-init.yaml` | Written by hand, as a template `openbaoinit.Manifests` fills | OpenBao's init Job in `mitstack-node`, and the ConfigMap with the public key the Job encrypts the root token and recovery key to, `openbaoSealKeyRecipient` |

mitstack-eu/cluster-bases is the repository of pinned third-party manifests
the k3s blocks copy from. It holds CloudNativePG, which the kit copies, and
none of the charts the script renders. This module renders those the way
cluster-bases renders its own, with a pinned chart, a values file and a check.
To change one, edit its values file or the script's pin, then render again:

```bash
scripts/render-components.sh
```

CI then runs the same script with `--check`, which fails when a committed file
differs from a fresh render.

The script also drops an empty `labels` or `annotations` map from the top-level
`metadata` of every document it renders, with `drop_empty_metadata_maps` in
`scripts/lib/empty-maps.sh`. The API server stores no empty map, so a document
that declares one never matches its live object, and Argo CD reads its
Application as OutOfSync, which means the cluster differs from the repository.
The step names no chart and no kind, and leaves a map anywhere
below the top-level `metadata` as it is. `rendered_set_test.go` runs
the step on documents that hold each kind of map and checks every rendered
manifest for a webhook that fails open, an autoscaler without a metrics API and
an empty map, as [the README](../README.md#what-the-rendered-components-must-look-like-for-argo-cd-to-read-them-synced)
describes.

The same file checks what Cilium holds of a node.
`TestManifests_NoCiliumContainerRunsPrivilegedOrAddsACapabilityBeyondTheReadmesList`
reads [the README's table of Cilium's capabilities](../README.md#what-cilium-holds-of-each-node)
and compares it with every Cilium container of the default and the hosting
set. After a change to `components/render/cilium-values.yaml` or to the chart's
version, render again and bring that table in step with the render.

Talos trims the blank space at both ends of each document of a manifest before
it decodes it, in `SetYAML` in Talos v1.14.4, so a document whose last value is
a block scalar loses that value's final line break. A block scalar is a value
written as `|` or `>` with its text on the indented lines below. The script ends
each such document with this comment line:

```yaml
# Talos trims each document before it decodes it, so this line keeps the value above whole.
```

The trim then removes the blank space after the comment, the value's final
line break stays inside the document, and the comment changes no object. The
CloudNativePG copy and the templates written by hand, such as
`components/logs/collector.yaml`, end such a document with the same line, and
`components/mesh/probe.yaml` writes its one-line answer as a double-quoted
string instead. `testutil.TalosObjects` in `testutil/talos.go` decodes a
manifest the way `SetYAML` does, and a test checks every manifest a node
applies with it, so a document a later change leaves ending in a block scalar
fails it.

Talos applies each manifest of the set once, creating every object that does
not exist yet and leaving an existing one as it is. A change to a component or
a policy therefore reaches a running cluster only through the files the
`rollout` package renders. A block commits those files to the cluster
repository at each upgrade, the block's operation that moves a running cluster
to a newer kit release, and Argo CD applies them, as
[the README's section on how a running cluster takes a release's components](../README.md#how-a-running-cluster-takes-a-releases-components)
explains. A field that a release drops leaves an object Talos created only
once that component's handover Job has had Talos give up its fields. On a new cluster, the first pass skips each object whose CRD is not yet
registered, such as the Kyverno policies, and Talos applies it on a later pass.

cluster-bases is private, so CI cannot fetch from it. A newer CloudNativePG
comes in by copying both files from a newer cluster-bases release and setting
the blob ids that `git rev-parse <tag>:cloudnative-pg/<file>` answers there in
`TestCloudNativePG_IsClusterBasesV040sComponentWithPostgres16AsItsDefault`.
That test undoes the operator's two changes, its header and its default
Postgres image, before it compares the blob.

Each admission policy has a `kyverno test` case under
`components/admission/tests/`, which CI runs with the Kyverno CLI v1.19.1:

```bash
kyverno test components/admission/tests
```

The `report-admission-fixtures` case runs the five policies that refuse a pod
or a Deployment, `mitstack-require-app-label`, `mitstack-require-mesh`,
`mitstack-disallow-host-path`, `mitstack-require-resource-limits` and
`mitstack-restrict-image-registries`, against the six fixtures in
`report/admission/` that the node report tries. The fixtures for the app
label, the host path, the limits and the registries each break one rule, and
the matching policy refuses that fixture alone. `mitstack-require-mesh` admits
all six. Two fixtures are refused by none of these policies: the digest
fixture, whose policy the kit does not hold yet, and `pod-security-restricted`,
which Pod Security refuses. The
`kube-app-deploy` case feeds the three pod policies a Deployment shaped like
the ones the kube-app-deploy block deploys for an app. It also feeds the
registry policy a `secrets-import` Job shaped like the one that block runs,
once with the busybox image the kit pins, which the policy admits, and four
times with a variant of it, which the policy refuses: another tag, another
digest, the tag with no digest and another Docker Hub image. The
`cloudnative-pg` and `acme-http01-solver` cases feed the policies the pods that
CloudNativePG and cert-manager start in an app namespace.

The policy holds each of its five exact images as a literal, since Kyverno reads
YAML. `TestManifests_RestrictImageRegistriesNamesExactlyTheImagesTheKitPinsForPodsInAnAppNamespace`
reads the same five from the manifests that pin them, and fails when the policy
differs from them. It reads the three CloudNativePG images from the operator's
manifest, the ACME solver image from cert-manager's arguments, and the busybox
image from the local path provisioner and the mesh probe, which must pin the
same reference.

The Talos images, such as the kubelet and the API server, are not in any
manifest. `pinnedTalosImages` in `components/image.go` lists them with the
digests their registries served for Talos v1.14.4 and Kubernetes 1.37.1.

## Where the scripts live

Each script is a file you can read and lint as the shell it is. The Go code
embeds most of them with `go:embed`. The node report's, the labeller's and the
cluster tokens' scripts are copied into the tools image at `tools.ScriptDir`
instead, since the builder's user data has no room for them inside a node's
configuration. The handover script sits there too, since the Job that runs it
reaches a cluster through its repository and never through a node's
configuration.

| Script | Where it runs | What it does | Test |
|---|---|---|---|
| `etcd/scripts/remove-member.sh` | The member removal Job, inline in its command | Looks up the lost node's etcd member, removes it, then deletes the Node | `etcd/etcd_test.go` |
| `etcd/scripts/bootstrap.sh` | The gateway, as `mitstack-talos-bootstrap` | Bootstraps etcd, or restores it and deletes the old Node objects, then deletes the admin talosconfig | `etcd/etcd_test.go` |
| `gateway/scripts/prepare.sh` | The gateway, once at first boot | Copies each image into the local registry by digest, then copies talosctl, kubectl and each served manifest out of the tools image that registry holds, refusing a file whose sha256 differs from its pin | `gateway/gateway_test.go`, and `toolsimage_test.go` against a built tools image |
| `report/scripts/report.sh` | The node report's CronJob, from the tools image at `/usr/local/lib/mitstack/report.sh` | Reads the cluster and posts the node report, whose members, checks and failure rules [the README's section on the report](../README.md#what-the-node-report-carries) gives. The reads that every report needs run one after another, each under `REPORT_READ_SECONDS`, 21 by default. The labelled-object reads, one per object type in `subscription_kinds`, share one such limit. The mesh, pod, workload, Argo CD, default-deny, OpenBao, etcd status, gateway and egress reads run beside them. The script runs the proof script under a 5-second limit, and a proof script that prints nothing fails like a read that exits non-zero. A cluster whose claims and Services, at about 60 to 100 bytes each, pods with their containers, at about 90 to 210 bytes each and about 450 for one whose container waits with a long message, and images, at about 130 to 200 bytes each, take the body past 256 KiB gets HTTP 413 from the platform, and the Job fails | `report/report_test.go` |
| `report/scripts/mesh-probe.sh` | The outside probe's CronJob in `kube-system`, from the tools image at `/usr/local/lib/mitstack/mesh-probe.sh` | Calls the mesh probe's target from outside the mesh, so the node report can show the mesh refusing a call with no workload identity. It makes a plain call to the target and a TLS handshake to its port 15008, and patches both outcomes into `kube-system/mitstack-mesh-probe`, with the certificate's SPIFFE identities, serial and validity | `report/meshprobe_test.go`, and `apiserver_test.go` against a real API server |
| `topology/scripts/label-nodes.sh` | The topology labeller's CronJob, from the tools image at `/usr/local/lib/mitstack/label-nodes.sh` | Labels each node `TOPOLOGY_NODES` names with its zone and `TOPOLOGY_REGION` under the field manager `mitstack-node-topology`, leaves a node that has not joined to a later run, and fails on a node it cannot read or label | `topology/topology_test.go` |
| `rollout/scripts/handover.sh` | Each component's handover Job, which Argo CD runs during a sync in the wave of the component's last object, from the tools image at `/usr/local/lib/mitstack/handover.sh` | Waits until each object `HANDOVER_OBJECTS` names exists and carries Argo CD's annotation `HANDOVER_ARGOCD_ANNOTATION`, with one `kubectl wait` for each object, run again a second later while kubectl finds the object absent, and at most `HANDOVER_ADOPTION_TIMEOUT`, a number of seconds such as `120s`, for them all. It then reads each object with its field managers. It then applies each one that Talos and Argo CD both applied under Talos's field manager with no field stated, so the API server drops every field Talos alone owned. It refuses an object that is absent or that Argo CD has not annotated when the wait ends, an object Argo CD has not applied and one that names Talos for a write that was no server-side apply, and fails when an object still names Talos afterwards. [The README](../README.md#how-an-object-talos-created-comes-to-hold-only-what-a-release-states) explains why | `rollout/rollout_test.go`, and `apiserver_handover_test.go` against a real API server |
| `openbaoinit/scripts/openbao-init.sh` | OpenBao's init Job, inline in its command | Initialises OpenBao once, commits `openbao-init.asc`, then enables kv and Kubernetes auth and writes each slice's policies and roles, `app-secrets-reader` and `app-secrets-writer` | `openbaoinit/openbaoinit_test.go` |
| `controlplane/scripts/build-image.sh` | A Cloud control plane's builder server, from its user data | Builds the cluster's Talos disk image with imager, writes it over the builder's own disk from a tmpfs and powers the builder off, so the block can snapshot that disk | `controlplane/scripts_test.go` |
| `controlplane/scripts/bootstrap-first-node.sh` | The install's first gateway, in `fsn1`, once at first boot | Finds the first node among the addresses the gateway has seen, proves it is the cluster's by its Talos API's certificate, bootstraps etcd on it and deletes the admin talosconfig | `controlplane/scripts_test.go` |
| `controlplane/scripts/cluster-tokens.sh` | A Job and a CronJob in the Cloud control plane's cluster, from the tools image at `/usr/local/lib/mitstack/cluster-tokens.sh` | Spends the enrolment token for the report token, Argo CD's notification token with the platform's URL and the cluster repository's token, refusing an answer missing one before it writes any, and renews the repository tokens with each app slice's | `controlplane/scripts_test.go` |
| `scripts/render-components.sh` | A workstation and CI | Renders the component set, drops an empty `labels` or `annotations` map from each document's metadata, and ends each document whose last value is a block scalar with a comment line | The `components` CI job |

The member removal Job, the node report and its outside probe, the topology
labeller, the cluster tokens' Job and CronJob, OpenBao's init Job and each
component's handover Job run in the
tools image that `tools/Dockerfile` builds from the repository root: bash,
curl, jq, gpg, openssl, talosctl and kubectl on Alpine 3.22, with the report's,
the outside probe's, the labeller's, the cluster tokens' and the handover's scripts. The image
also carries every manifest a gateway serves, at
its repository path under `tools.ScriptDir`, as `tools.ManifestPath` gives it.
`tools/Dockerfile.dockerignore` keeps every other file out of the build. `tools/tools.env` pins both binaries with
their sha256. The Dockerfile checks the binaries it installs against it, and
the gateway's `prepare.sh` checks the ones it copies out of the image, so the
image and the gateway run the same binaries. `tools/jq.env`
pins the image's jq the same way, and the report's tests fetch that jq, since
jq-1.8 parses a number more strictly than the jq a runner may have. Each release
pushes and signs that image and names it in `tools.Image`, which a block
passes to `etcd.RemoveMemberJob`, `report.Manifests`, `openbaoinit.Inputs` and
`gateway.NewList`, and which the control plane passes to
`topology.Manifests` and as `gateway.Inputs.Tools`.
[The README](../README.md#how-a-release-is-versioned) says how the release
pins it.

## How the tests run the scripts

The script tests run the script a rendered manifest carries, so they test what
a node would run. The report's, the labeller's, the cluster tokens' and the
handover's tests
check that each Job's or CronJob's command names the script's path in the tools
image, then run the repository's file. The `tools image` CI job checks that the
image holds those same bytes. `testutil.NewTools` writes fake `talosctl`, `kubectl`,
`curl`, `skopeo`, `docker` and `systemctl` commands that log each call, and
each test reads that log to check the order of the calls.

One check needs Docker and the network, so the default `go test ./...` leaves
it out and CI runs it as its own `mirror check` step. It starts a registry from
the image the gateway runs its mirror from, copies trust-manager's image index
from quay.io into it with the skopeo image the gateway pins, then reads the
digest the registry serves for the tag:

```bash
go test -tags mirror -count=1 -run '^TestPrepareScript_Mirrors' ./gateway/
```

A second check needs Docker and a built tools image, so the `tools image` CI
job runs it once it has built one. It starts etcd and the kube-apiserver from
the images `components/image.go` pins for Talos, with RBAC on, and applies
every object `report.Manifests` renders in a built-in API group. It then runs
the outside probe's and the security scan's containers in that image as their
CronJobs run them: as their own service account and user, with the
capabilities their container drops, on a read-only root with a tmpfs at each
`emptyDir`. A Role that lacks a verb the real kubectl needs therefore fails
here, as the test's fake kubectl cannot show.

The same check runs the scan's kube-bench step with the real kube-bench, which
the script tests replace with a fake. A second container holds one process
named as each control-plane component, and the step shares its process table
as `hostPID` shares a node's. `/var/lib/etcd` is a tmpfs with mode 700 and
user 60, as Talos creates it. An image whose `ps` cannot find a process by
name, a variable kube-bench takes for a flag, or a lookup inside the etcd
directory therefore fails here.

`apiserver_handover_test.go` runs a component's handover against the same API
server, which merges each server-side apply as a cluster's does. It creates
Cilium as kit release v0.53.0 shipped it, the way Talos does, then applies it
and this release's Cilium the way Argo CD does. The privileged init container
that this release drops is still there, and the handover Job then removes it.
The Job's container runs as Argo CD's application controller, with the
ClusterRole the kit's own Argo CD manifest gives it.

To run both on a workstation, build the image first:

```bash
docker build -t talos-tools:ci -f tools/Dockerfile .
TOOLS_IMAGE=talos-tools:ci go test -tags apiserver -count=1 -run '^TestAPIServer_' .
```

A third check runs a gateway's preparation against that built image, so an
image the Dockerfile left a file out of fails before a release publishes it.
It starts the registry the gateway runs on `127.0.0.1:5000`, pushes the image
into it and removes the local tag. The preparation's Docker therefore reads the
image from that registry by its digest, as a gateway does. The test runs the
rendered `prepare.sh` with the hosting set's `tools.env` and `manifests.txt`,
and expects talosctl, kubectl and every served manifest with its pinned sha256:

```bash
TOOLS_IMAGE=talos-tools:ci go test -tags toolsimage -count=1 -run '^TestToolsImage_' .
```

| Test | What it proves | Against what |
|---|---|---|
| `TestRender_MatchesTheGoldenConfiguration` | The configuration for one and three control-plane nodes on the `hcloud` and the `metal` profile matches its recording | `nodeconfig/testdata/golden/` |
| `TestRenderWorker_IsAWorkerTalosAcceptsThatJoinsThroughThePrivateAPIEndpointAndAppliesNoManifest` | A dedicated worker's configuration passes Talos's own loader and schema, names machine type `worker` and the first private API load balancer, carries no manifest and no Talos API access, and matches its recording | `nodeconfig/testdata/golden/metal-3-worker-wk-fsn1.yaml` |
| `TestRenderWorker_HoldsTheGoldenListOfSecretsWithNeitherTheEtcdNorTheServiceAccountSigningKey` | The worker holds exactly the fixture secrets its recording lists, and neither the etcd authority's key nor the service-account signing key, which the same search finds in a control-plane node's configuration | `nodeconfig/testdata/golden/metal-3-worker-secrets.txt` |
| `TestRender_PullsEveryImageOfTheComponentSetFromEveryGatewaysMirror` | Every image the set names, and every image the configuration names, comes from a mirror with no fallback | The mirror list |
| `TestRender_UsesHetznersResolversAndTimeServersInPlaceOfTalosDefaults` | Hetzner's resolvers and time servers replace Talos's | The rendered documents |
| `TestRender_GivesACloudNodeADefaultRouteThroughHetznersRouterAndKeepsDHCPOnItsLink` | A Cloud node routes through `10.0.0.1` and keeps its DHCP address, and a dedicated node gets neither document | The rendered documents |
| `TestRender_SignsServiceAccountTokensForTheIssuerItIsGiven` | The configuration names the cluster's `ServiceAccountIssuer` as the `issuerURL` Talos passes the API server as `--service-account-issuer` | The rendered `KubeServiceAccountConfig` |
| `TestRender_LeavesTheFirewallToNftablesAndRunsEveryContainerOnTheHostNetwork` | Docker writes no firewall rule, and the mirror and Caddy publish no port through Docker | The rendered cloud-config |
| `TestRender_SendsMirrorPullsToTheRegistryAndRefusesEveryOtherMethod` | The Caddyfile tries the registry for a GET or HEAD before it answers 405 | The rendered Caddyfile |
| `TestRun_KeepsEveryKeyOfTheEmbeddedConfigurationOutOfTheBuildOutput` | No key of the fixture secrets reaches the build's log or error, on success or failure | A fake imager that prints the whole configuration, as imager does |
| `TestRemoveMemberJob_RemovesTheEtcdMemberBeforeDeletingItsNode` | The member goes before its Node, and a failed removal keeps the Node | Fake talosctl and kubectl |
| `TestRemoveMemberJob_RendersNoRemovalWhileASecondEtcdMemberReadsUnhealthy` | A second unhealthy member leaves no majority, so no Job renders and the refusal names that member | A report body and `internal/redundanttest` |
| `TestRemoveMemberJob_RendersNoRemovalOnAReportReadBeforeTheReplacementBegan` | A report read before the replacement began proves no member serving | The same |
| `TestBootstrapScript_RestoresFromTheSnapshotThenDeletesTheOldNodes` | The restore bootstraps from the snapshot, deletes every Node outside `--keep`, and deletes the talosconfig | Fake talosctl and kubectl |
| `TestReport_PostsEachNodeWithItsTalosVersionAndAllocatableTheEtcdMembershipAndEachApplicationsHealthComparedRevisionAndAppliedObjectsSignedForItsExactBody` | The report carries each node, its Talos version, each of its addresses with the address's type, its zone and region labels with every manager of its zone label, the members, and each Application's health, sync, compared revision, `operation` and applied objects, and makes its reads in order. The `operation` is the phase of Argo CD's last sync, with its message cut to 256 characters unless the sync succeeded, as the jq function `last_sync` in `report/scripts/report.sh` builds it. Each Application's `revision` is its `status.sync.revision`, and one Argo CD compared none for reads `null`, as [the README's section on the revision](../README.md#how-the-report-names-the-revision-each-application-was-compared-against) explains. Its last call runs the proof script with the final body file, the report URL, the namespace and its own service account, and the post carries what the script prints as `Mitstack-Report-Proof`. The test's proof script stands in for block-kit's, which block-kit's own tests cover. An applied object, a Secret among them, carries its API group, object type, namespace, name, sync status and Argo CD's health and message, and no other field. A cluster with no Ingress carries an empty `ingresses` list. The whole body is byte for byte the recorded one | Fake talosctl and kubectl answering `report/testdata/nodes.json`, `report/testdata/applications.json`, `report/testdata/golden/argocd.json`, `report/testdata/golden/healthy-body.json`, a fake `date`, and a local HTTPS server playing the platform |
| `TestReport_PostsEachMemberItReadsFromAHealthyCluster` | One healthy run carries each member that [the README's member table](../README.md#the-members-of-a-report) lists, with the values the fixtures give: `pods` as `report/testdata/golden/pods.json` holds them, `systemVolumes`, `jwks`, `cloudNativePG`, `storage`, `services`, `checks`, Argo CD's Services, Ingresses and pods, `ingresses` as `report/testdata/golden/ingresses.json` holds them, `defaultDenyPolicies`, `repositorySecrets`, `appProjects`, `admission`, `securityScans` and `mesh`. `checks` names the nine checks in the order the platform judges them, and `mesh` passes `callFromInside`, `workloadIdentity` and `plainCallFromOutside`. The `ingresses` member lists five Ingresses in three namespaces, each with its namespace, name, class and the addresses its status holds and no other field: two that Traefik gave an address, one of class `nginx` with none, one whose class is its annotation and one with no class. No SealedSecret's encrypted data reaches the platform | Fake talosctl and kubectl answering the `report/testdata/` fixtures, `report/testdata/ingresses.json` among them, `report/testdata/golden/system-volumes.json`, a local server playing the mesh probe's target, and a fake `date` |
| `TestReport_CutsAnIngressClassTakenFromItsAnnotationTo256Characters` | An Ingress whose class comes from a 300-character annotation carries its first 256 characters | A fake kubectl answering one such Ingress |
| `TestReport_LeavesOutEveryApplicationsAppliedObjectsWhenCarryingThemWouldPassThePlatformsLimit` | When the applied objects would take the body past the platform's 256 KiB, no Application carries them, each keeps its name, health, sync and revision, and the rest of the report still posts. Beside 1000 labelled namespaces, `subscriptionResourceKinds` names each object type whose objects it lists whole and no other | A fake kubectl whose Application applies 4000 objects, or 1000 Degraded objects each with a 256-character message, and `report/testdata/golden/argocd-without-applied.json` |
| `TestReport_NamesEachAppliedObjectsArgoCDHealthAndEachContainersStateButNoTerminatedContainersMessage` | A Degraded object carries Argo CD's health and message cut to 256 characters, a Healthy one no message, and a ServiceAccount a `null` health. An Application whose `resourceHealthSource` reads `appTree` gives its objects no health member. A container waiting on `ImagePullBackOff` carries its reason and message cut to 256 characters, and one that ended carries its reason alone, so no termination message reaches the platform | `report/testdata/applications.json`, `report/testdata/pods.json`, and the cases in `report/testdata/golden/object-health.json`, each naming the member it reads and what that member holds |
| `TestReport_NamesEachEtcdMembersHealthTheLeaderAndTheStatusReadThatFailed` | Each etcd member reads `healthy` only when its node answered `talosctl etcd status` with an empty `ERRORS` cell. A node that does not answer, an alarm and a status read that times out each leave their member unhealthy, and `statusError` names a read that failed | A fake talosctl answering `report/testdata/etcd-status.txt`, cut short or with an alarm |
| `TestReport_NamesEachOpenBaoMembersSealStateTheActiveOneAndEachReadThatFailed` | Each OpenBao member carries `initialized`, `sealed` and `active`, and `leader` names the active one. A member that does not answer, answers too late, or answers JSON that is no object reads `null` with an `error` naming the read and its exit status, and the report still posts | Local HTTPS servers playing each member's `sys/seal-status` and `sys/leader` |
| `TestManifests_PointsTheReportAtEachOpenBaoPodByANameItsCertificateHoldsTrustsItsAuthorityAndOpenBaoTakesItsCallsOnItsAPIPort` | `OPENBAO_MEMBERS` names each pod of OpenBao's StatefulSet at its headless Service, a name that `openbao-tls`, OpenBao's serving certificate, holds. `OPENBAO_CA_FILE` is the key that trust-manager writes in the `openbao-ca` ConfigMap, the copy of OpenBao's certificate authority in each namespace, mounted as an optional volume. OpenBao's Cilium policy in `mitstack-hosting-flows`, the hosting set's manifest of allowed flows, admits the report's pod, as the CronJob labels it, on 8200 and 15008 | The rendered CronJob and the hosting set's TLS, OpenBao and `mitstack-hosting-flows` manifests |
| `TestReport_AsksEachArgoCDRouteWithoutASessionAndKeepsItsStatusItsSizeAndOneBooleanOfUserinfoAndOfSettings` | Each of the 17 routes carries its status and size. Userinfo carries its `loggedIn` boolean and settings its `userLoginsDisabled` boolean, `true` or `false`. Either reads `null` when the answer is not JSON, and `userLoginsDisabled` also when the answer is empty, names none, gives it as text, or comes with another status than 200. No other text of either answer reaches the platform, as [the README's section on the local login](../README.md#how-the-report-shows-whether-a-password-signs-in-to-argo-cd) explains. A request that carries an `Authorization` header, a cookie or a body fails the test, and an Argo CD that does not answer gives each route status 0 and `curl exit 7` | A local HTTPS server playing `argocd-server` without a session, answering three userinfo bodies and seven settings answers |
| `TestManifests_PointsTheReportAtArgoCDsServerServiceOnItsHTTPSPort` | `ARGOCD_URL` names the default set's `argocd-server` Service, whose port 443 serves the server's 8080 | The rendered CronJob and `components/argocd/argocd.yaml` |
| `TestReport_NamesAFailedVolumeCloudNativePGStorageOrServiceReadInlineWithItsExitStatusAsANumberInAFullReport` | A CloudNativePG, volume, storage and Service, pod, Ingress or default-deny read that exits 1 or times out is named inline with its exit status, a number to the tools image's jq, and its stderr, in each member that read fills. An Ingress read that fails is named in `ingresses` and in Argo CD's own Ingresses alike. A volume read that prints no JSON or no `STATE` or `EPHEMERAL`, and a storage and Service read or an Ingress read that prints no JSON, are refused inline. The nodes, Applications and `checks` member still post | Fake reads, the jq `tools/jq.env` pins, and `REPORT_READ_SECONDS=1` |
| `TestReport_ThatCannotReadPostsOneReportNamingTheReadAndExitsNonZero` | When the nodes, Applications, key set or etcd members read fails, the key set read times out, jq cannot build the body, or the proof script fails its token request or prints no proof, exactly one unsigned report is posted. Its `error` names the read or `body`, its exit status or `timedOut`, and the last 1 KiB of its stderr with no NUL, report token, bearer token or JWT | Fake reads that write those to stderr, and `REPORT_READ_SECONDS=1` |
| `TestReport_ListsTheObjectsOfEachKindItReadWholeAndLeavesOutAKindWhoseReadFailsOrIsCutShort` | `subscriptionResources` lists each labelled object with its namespace, its object type as `kubectl --show-kind` names it, its name and its subscription, and `subscriptionResourceKinds` names the object types whose read finished whole. An object type whose read fails, prints no list, is cut short by the time limit, or holds more than 1000 objects is left out of both, the report stays whole, `madeAt` is no later than `reportedAt`, and the body carries no `subscriptionResourcesComplete` | A fake kubectl answering `report/testdata/subscription-resources/`, and `report/testdata/golden/subscription-resources.json` |
| `TestManifests_LetsTheReportListPodsEachKindItReadsAndCloudNativePGsObjectsAndMintATokenForItsOwnServiceAccountAloneAndGrantsNoVerbOnSecrets` | The report's ClusterRole lists pods, each object type in `subscription_kinds` in `report.sh`, the ExternalSecret among them, CloudNativePG's clusters, databases and roles, and claims and storage classes, and names neither `secrets` nor `*`. A Role in `mitstack-node`, bound to the report's service account, grants `create` on `serviceaccounts/token` for that one account by name | The rendered ClusterRole, Role and RoleBinding |
| `TestManifests_NoRoleBoundToTheReportGrantsAVerbOnSecretsOrOnEveryResource` | Every Role and ClusterRole any binding gives the report's service account names neither `secrets` nor `*` | Every rendered binding whose subject is the report |
| `TestManifests_LetsTheReportListSealedSecretsInArgoCDAndDryRunFixturesInTheProbeNamespaceAlone` | The report may list SealedSecrets in `argocd`, and get the fixtures and create pods and Deployments in `mitstack-admission-probe`, and nothing more in those namespaces | The rendered Roles and RoleBindings |
| `TestManifests_TheProbeNamespaceEnforcesPodSecurityRestrictedAndHoldsEachRulesFixtureUnderTheRulesName` | The probe namespace carries the Pod Security `restricted` labels, its one ConfigMap holds each fixture as JSON under its rule's name, and the gateway's image list takes no fixture image | The rendered Namespace and ConfigMap, and `components.Set.Images` |
| `TestManifests_RefusesEveryRequestInTheProbeNamespaceThatIsNotADryRun` | A ValidatingAdmissionPolicy bound to the probe namespace, failing closed, denies every pod and Deployment request whose `dryRun` is not true | The rendered policy and binding |
| `TestReport_ClipsEachFixtureTo4KiBAndEachAnswerTo2KiB` | A long fixture and a long refusal are cut to their bounds | A fixture of 10,400 bytes and a refusal of 5,000 |
| `TestReport_NamesASourceReadThatFailsInsideItsMemberInAFullReport` | A failed SealedSecret, AppProject, fixture or CronJob read stays inside its member with its exit status and stderr | Fake kubectl failing one read at a time |
| `TestReport_SendsTheScansAsUnreadableWhenTheirConfigMapCannotBeRead` | A failed read of the scan's ConfigMap sends `securityScans` as `unreadable` with the exit status and stderr | Fake kubectl failing that read |
| `TestReport_KeepsAScanSummaryOnlyAsOneJSONObjectWithin16KiB` | A missing summary is `null`, and one that is no JSON object or is over 16 KiB reads `unreadable`, while the other summary is kept | The scan ConfigMap edited by jq |
| `TestReport_NamesTheScanCronJobsNewestJobAndTheTwentyNewestEventsOfThatJobAndItsPodsAndKeepsTheRunnerWhenEitherReadFails` | The newest Job the scan CronJob owns comes with its conditions, cut to 256 characters, and the newest 20 events of that Job and its pods, while the CronJob's other Jobs and other objects are left out; a failed Job or event read stays inside its member and `runner.output` keeps its text | `testdata/scan-jobs.json`, `testdata/scan-events.json` and `testdata/golden/scan-cause.json` |
| `TestManifests_LetsTheReportListEventsInKubeSystemAndNowhereElse` | Only the Role in `kube-system` grants the report a list of events | The rendered Roles and ClusterRoles |
| `TestSecurityScan_RunsKubeBenchOnTheTalosBenchmarkAndKubescapeOnNSAAndPatchesBothSummariesIntoItsConfigMap` | kube-bench runs `cis-1.10` on every target and kubescape the NSA framework with no exception and no host sensor. Each summary keeps every failure, the controls most severe first with workloads for Critical and High alone, and the scan patches both with its outcome into its ConfigMap | Fake kube-bench and kubescape answering `report/testdata/scan/`, and fake kubectl and `date` |
| `TestSecurityScan_KeepsAToolThatFailsAsAnErrorWithItsStderrRunsTheOtherAndExitsNonZero` | A kube-bench that exits 1 is kept as its summary's `error`, kubescape still runs, and the scan exits non-zero | A fake kube-bench that fails |
| `TestSecurityScan_KeepsEachSummaryWithinTheReportsBoundAtAnySize` | 2,000 failed checks and 11 High controls on 3,000 workloads each still fit 16 KiB, with the totals whole | Fakes printing outsized results |
| `TestManifests_RunsKubeBenchAsRootOnTheHostsConfigWithNoTokenAndKubescapeAsNonRootWithNoHostPathDailyInKubeSystem` | The scan CronJob runs daily in `kube-system` on a control-plane node and sees the host's processes. Its kube-bench step runs as root with `/etc/kubernetes`, `/var/lib/kubelet` and `/var/lib/etcd` read-only and no token, and its kubescape step runs as user 65532 with the token and no host path. Both drop every capability. The pod mounts no other node directory. The first two keep their `Directory` type check and `/var/lib/etcd` has none, since Talos's kubelet checks a type inside its own container, which holds no `/var/lib/etcd` | The rendered CronJob |
| `TestManifests_LetsTheScanReadNoSecret` | The scan's ClusterRole only gets and lists objects, never a Secret or `*`, and binds the scan's service account alone | The rendered ClusterRole and its binding |
| `TestManifests_RefusesInputsWithoutAProofScript` | `report.Manifests` refuses inputs whose `ProofScript` is empty, so no block renders a report that cannot sign | Inputs with an empty `ProofScript` |
| `TestManifests_RefusesAProofScriptOneByteOverItsLimit` | `report.Manifests` refuses a proof script one byte longer than `report.ProofScriptLimit`, naming both sizes, so no block's script takes more of the builder's user data than the kit keeps free for it | Inputs with a `ProofScript` of 2,049 bytes |
| `TestOutsideProbe_ReadsAClosedPortAResetConnectionAFailedTLSHandshakeAndAServingMirrorAsSuchFromOutsideTheMesh` | The outside probe names a closed port `closed`, a reset or a closed connection `reset`, plain HTTP on the TLS port and a certificate another CA signed `tls-failure`, a 503 `answered` and a 200 `served`, and the report carries each reading with the time the probe took it | The real `mesh-probe.sh` and curl against local peers |
| `TestReport_ReadsEveryGatewayFailedWithTheReasonWhenTheOutsideProbesResultCannotBeTrusted` | A stale result, one with no gateway readings, a missing ConfigMap and a reading the report cannot judge each fail every gateway with the reason | Edited copies of `testdata/mesh-probe.json` |
| `TestManifests_RunsTheOutsideProbeWhereTheMeshLeavesItOutAndLetsItReachEachGatewaysMirrorOn443Alone` | istio-cni leaves the outside probe's namespace out of the mesh, the probe carries the gateways and their CA, the report carries no CA, and the probe's policy reaches each gateway's /32 on 443 and no other address | The rendered manifests |
| `TestManifests_ShipNoCommentLine` | No rendered report manifest holds a line that starts with `#`, since every byte of one counts against a builder's user data | The rendered manifests |
| `TestReport_ReadsATLSConnectionToTheEgressTargetAndWhyOneFailed` | `egress` passes when the target answers over TLS, and otherwise names the target and curl's exit status | A local HTTPS server and a closed port |
| `TestReport_NamesNoGatewayWhenTheClusterHasNone` | An empty `GATEWAYS` gives an empty `gateways` list | The report's environment |
| `TestGateway_ReadsWhetherAReportNamesAReadingOfTheGatewayMadeAfterAGivenTimeAndWhetherItsMirrorServed` | `report.Gateway` counts a reading only when the outside probe took it after the time it is given, reads one with no `probedAt` as before, and names a body it cannot read | Report bodies |
| `TestReport_ListsEachImageReferenceOnceWithItsDigestFromEveryPodAndTheTemplateOfEachDeploymentStatefulSetDaemonSetJobAndCronJob` | `images` names each reference once, with the digest the reference names or a pod's status reports, and a template's tag no pod runs with a `null` digest | A fake kubectl answering `report/testdata/image-pods.json` and `report/testdata/workloads.json` |
| `TestReport_At1000PodsNaming300DistinctImagesTheBodyStaysInsideThePlatformsLimitWithTheImageListWhole` | 1,000 pods naming 300 images make a body within 256 KiB, whose size the test logs, with every image and every pod listed | A fake kubectl answering 1,000 generated pods |
| `TestReport_NamesTheFailedReadInPlaceOfAnImageListAPodOrAWorkloadReadLeavesPartial` | A pod or workload read that fails leaves `images` naming that read with its exit status and stderr, which `report.Images` refuses | A fake kubectl failing one read |
| `TestImages_RefusesAReportThatListsNoImages` | `report.Images` refuses a body without `images`, as a report from an earlier CronJob sends | A report body |
| `TestManifests_RefusesGatewaysWithoutTheirCAsCertificate` | `report.Manifests` refuses gateways whose `GatewayCA` is empty, holds a key, or is more than one certificate | Each input |
| `TestManifests_LabelsEachJoinedNodeWithItsZoneAndTheRegionUnderTheLabellersFieldManager` | The labeller reads each node it names, then labels it with its zone and the region under `mitstack-node-topology` | A fake kubectl that logs each call |
| `TestManifests_LeavesANodeThatHasNotJoinedToALaterRun` | A node the API server does not know yet gets no label, and the run still succeeds | The same |
| `TestManifests_FailsWithoutLabellingWhenANodeCannotBeRead` | A read that fails stops the run before any label, so a refused read is never taken for a node that has not joined | The same |
| `TestManifests_FailsWhenALabelIsRefused` | A label the API server refuses fails the run | The same |
| `TestManifests_MayReadAndPatchTheNodesItNamesAndNoOtherObject` | The labeller's one ClusterRole rule grants `get` and `patch` on `nodes` by name alone, no Role exists, the binding names only its own service account, and that account holds no Talos role | The rendered RBAC |
| `TestManifests_RunsTheToolsImageOnAControlPlaneNodeAsANonRootUserWithAReadOnlyRoot` | The CronJob runs in `mitstack-node` on a control-plane node, one run at a time, in the tools image, as a non-root user on a read-only root, with exactly its four variables | The rendered CronJob |
| `TestManifests_RefusesInputsItCannotLabelANodeWith` | `topology.Manifests` refuses no node, a name that is no DNS label, a zone or region that is no label value, a node named twice, and an image not named by digest | Each input |
| `TestCluster_LabelsEachCloudNodeWithItsLocationAndTheNetworkZone` | The builder's configuration carries the labeller inline, naming each Cloud node by its server's name with its location, and `eu-central` as the region | The configuration the builder embeds |
| `TestRenderWorker_LabelsTheNodeWithItsOwnLabelsBesideTheSubscription` | A dedicated worker's `KubeNodeConfig` carries its `Node.Labels` beside `mitstack.dev/subscription` | The rendered worker configuration |
| `TestRender_RefusesNodeLabelsEveryCloudNodeWouldShareThatReplaceTheSubscriptionsOrThatKubernetesRefuses` | `Node.Labels` is refused on a Cloud node, for a label that replaces the subscription's, for a key or value Kubernetes refuses, and for a label in Kubernetes's own namespace that NodeRestriction refuses a kubelet | Each input |
| `TestReport_NamesEachMeshResultItCannotPassAsFailedWithItsReason` | A target that is down, a missing ConfigMap, a result over 15 minutes old or dated after the report, an answered plain call, a plain call that reached nothing, a refused handshake, a certificate for another workload, for no identity or out of its validity, a result of a shape the report cannot judge, and a ConfigMap that holds no data, a result that is not JSON or a read that prints no JSON each fail the results they touch, with the reason named, and the report still posts whole | Fake reads, a closed port and edits of the fixture |
| `TestMeshProbe_WritesThePlainCallsOutcomeAndTheCertificateZtunnelPresentsIntoTheReportsConfigMap` | The outside probe calls the target's port 8080 and port 15008 by its absolute name, and patches its ConfigMap with curl's exit status and stderr and the certificate's serial, validity and SPIFFE identity | Fake curl and kubectl, and a fake `openssl s_client` printing `report/testdata/mesh-probe-handshake.txt`, which the real `openssl x509` reads |
| `TestMeshProbe_WritesAnAnsweredPlainCallAndAHandshakeThatGaveNoCertificateAsTheyCame` | An answered plain call is written with its answer, and a handshake that gave no certificate is written with a null certificate and its stderr | Fake curl, openssl and kubectl |
| `TestManifests_LetsTheOutsideProbeAndTheScanReadAndPatchTheirOwnConfigMapAndTheReportOnlyReadItByNameInKubeSystem` | The outside probe may only get and patch `kube-system/mitstack-mesh-probe`, the scan's Role may only get and patch `kube-system/mitstack-security-scans`, and the report may only get each, all by name, and the outside probe runs in `kube-system` | The rendered Roles, RoleBindings and CronJob |
| `TestAPIServer_TheOutsideProbeAndTheSecurityScanWriteTheirOwnConfigMapAndNoOtherUnderTheRenderedRBAC` | Run as their CronJobs run them, the outside probe writes its result into `mitstack-mesh-probe` and the security scan writes both summaries and its outcome into `mitstack-security-scans`. The scan's kube-bench step, run as its pod runs it with every capability dropped, reads `--profiling=false` off the API server's, the controller manager's and the scheduler's process and mode 700 off an etcd directory another user owns, and its summary reaches the ConfigMap with no error. The API server refuses the probe a list, a read or a patch of another ConfigMap, and refuses either one a patch of the other's ConfigMap or of a third | A kube-apiserver and etcd from the images the kit pins for Talos, the objects `report.Manifests` renders, the built tools image, a container of stand-in processes named as the control plane's whose process table the kube-bench step shares, and a tmpfs with the mode and owner Talos gives `/var/lib/etcd`, under the `apiserver` build tag |
| `TestAPIServer_AnObjectTalosCreatedKeepsAFieldAReleaseDropsUntilItsComponentsHandoverGivesUpTalossFields` | Cilium's agent, created as Talos creates it from kit release v0.53.0 and then synced to this release as Argo CD syncs it, still runs the privileged `mount-bpf-fs` init container, at generation 1. The component's handover Job, run as Argo CD's application controller, then leaves the agent the init containers the release states and no privileged one, at generation 2, and no object of the component names Talos as a field manager. A handover run before Argo CD applied the component waits out its limit, is refused naming the objects and takes no field away, and a second handover and a second sync change nothing. A handover that starts while two objects of the component are absent waits until Argo CD creates them five seconds later, and succeeds | A kube-apiserver and etcd from the images the kit pins for Talos, `testdata/v0.53.0/cilium.yaml`, the files `rollout.Release.Files` renders and the built tools image, under the `apiserver` build tag |
| `TestHandover_GivesUpTalossFieldsOnEachObjectTalosAndArgoCDBothAppliedAndOnNoOther` | The handover waits once for each object its component states for Argo CD's annotation, with the whole 120 seconds left for the first wait, reads every one of them, applies under Talos's field manager a list that names only the objects both Talos and Argo CD applied and states none of their fields, then reads those objects again | A fake kubectl that logs each call and keeps the applied file |
| `TestHandover_WaitsAgainForAnObjectArgoCDHasNotCreatedYet` | A wait that kubectl ends with `NotFound` runs again after a one-second sleep, twice here, and the handover then succeeds | A fake kubectl whose first two waits report the object absent, and a fake `sleep` that logs its call |
| `TestHandover_FailsNamingWhatItCouldNotHandOver` | The handover fails without applying when its timeout is no number of seconds, when an object lacks Argo CD's annotation when the wait ends, when an object Talos created has no apply of Argo CD's, when an object names Talos for a write that was no server-side apply, or when the objects cannot be read, and fails when the API server refuses the apply or an object still names Talos afterwards | The same |
| `TestRelease_RunsAComponentsHandoverJobAndAppliesItsMarkerInTheWaveOfItsLastObject` | The handover Job is a Sync hook in the wave of the component's last object, and so is the marker, whatever wave the component's objects name, waves below 0 included | The files `Release.Files` renders |
| `TestMeshProbe_TheTargetAdmitsOnlyTheReportAndTheOutsideProbeWhichLooksUpOnlyItsNameAndReachesOnlyItAndTheAPIServer` | The target's Cilium policy admits the report's and the outside probe's pods, as they are labelled, on 8080 and 15008 and no other pod. The outside probe's own policy lets it look up only the target's name through Cilium's DNS proxy, and reach only the API server and the target on those two ports. The target carries `mitstack.dev/app`, serves the answer, identity and name the report expects, and adds no image to the mirror's list | The component set's target, the rendered CronJobs and Cilium's view of each pod's labels |
| `TestRender_EncryptsStateAndEphemeralWithLUKS2UnderTheNodeIDKeyWithEphemeralsKeyLockedToState` | For one and three nodes on the `hcloud` and the `metal` profile, `STATE` and `EPHEMERAL` are LUKS2 under a `nodeID` key, `EPHEMERAL`'s locked to `STATE` and mounted secure | The rendered `VolumeConfig` documents |
| `TestDefault_RunsKyvernoWithBothPoliciesAndCloudNativePGWithTheDatabaseSlicePolicy` | The set carries Kyverno, its app-label and default-deny policies, CloudNativePG and the slice policy, and the mirror holds each of their images by digest | The default set and its mirror list |
| `TestRender_GivesEveryClusterADefaultStorageClassThatProvisionsOnItsNodesUserVolume` | For one node and for three, the configuration carries the user volume `local-path-provisioner` and fetches the provisioner. The set has one default class, the provisioner's. That class waits for its first consumer, makes `local` volumes and deletes a claim's directory with the claim. The provisioner keeps each claim on the user volume and runs with its hardened security context. The mirror holds both of its images by digest. Its manifests hold no Secret, no reference to one, no variable named for a credential and no rule on Secrets | The rendered documents and the set's manifests |
| `TestLocalPathProvisioner_LabelsEveryPodItStartsAndOpensTheAPIServerToItsNamespaceAlone` | The provisioner's pods and its helper pods carry `mitstack.dev/app`. Neither admission policy exempts its namespace, which admits a `hostPath` volume. Its pods may reach the API server and nothing else, and an app pod gains nothing from its policy | Both admission policies, and the flow policy's selectors evaluated in Go against the labels Cilium gives a pod in the namespace and an app pod |
| `TestDatabaseSlicesPolicy_GrantsEachPodExactlyItsAllows` | A slice's Postgres instance, a CloudNativePG Job, an app pod, a pod outside a database slice and a system pod each get exactly their allows, each beside port 15008 | Cilium's view of each pod's labels |
| `TestRender_RunsTheMeshAndItsAdmissionPoliciesOnEveryProfileAndSize` | For one and three nodes on the `hcloud` and the `metal` profile, the configuration fetches Istio's four charts and the mesh probe's target, and carries the mesh-wide `STRICT` policy, the probe policy and both mesh admission policies | The rendered documents |
| `TestRender_PutsEveryPodButKubeSystemsIstiosAndKyvernosInTheMeshFromTheMirror` | The CNI agent selects every pod whatever its labels, skips only `kube-system`, `istio-system` and `kyverno`, which Kyverno's mesh policy exempts too, and leaves DNS to Cilium. The mirror holds Istio's three images, `istio-system` is privileged, istiod renders no injector templates, and Cilium keeps other CNI configuration and its socket load balancing to the host | The CNI agent's and Cilium's ConfigMaps and the mirror list |
| `TestRender_RefusesACallWithoutAWorkloadIdentityButOnTheWebhookAndWebPorts` | Every PeerAuthentication is `STRICT`, the only ports that take a call with no identity are the five exceptions, the CloudNativePG operator's in the default set and four in the hosting set. `mitstack-require-strict-mtls` admits the four hosting exceptions by name, and the operator's sits in `cnpg-system`, which the policy skips | Every PeerAuthentication of the hosting set |
| `TestMesh_KeepsTheOperatorAloneOnAnInstancesManagerPortAndEveryoneOffItsOtherPorts` | A Postgres instance takes 5432 from any caller Cilium admits, 8000 from the operator alone, and nothing else | The AuthorizationPolicy's rules, read by `testutil.Denies` |
| `TestRender_AdmitsZtunnelsPortWhereverArgoCDsChartAdmitsACallBetweenItsPods` | Each Argo CD chart policy that admits a call from its own pods has a policy admitting 15008 from the same pods | Both sets of NetworkPolicies |
| `TestHosting_KeepsTheMembersAloneOnOpenBaosClusterPortAndEveryoneOffItsOtherPorts` | OpenBao takes 8200 from any caller Cilium admits, 8201 from its members alone, and nothing else | The AuthorizationPolicy's rules, read by `testutil.Denies` |
| `TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressingAndAStuckOneAsDegraded` | The `argocd-cm` health check reads each phase CloudNativePG passes through as Progressing, and a cluster waiting for its user as Degraded | The check run in gopher-lua, the Lua runtime Argo CD embeds |
| `TestArgoCD_KeepsEachObjectsHealthOnItsApplicationWhereTheNodeReportReadsIt` | `argocd-cmd-params-cm` sets `controller.resource.health.persist` to `"true"`, so Argo CD 3 writes each object's health into its Application's `status.resources`, where the node report reads it | The rendered `components/argocd/argocd.yaml` |
| `TestArgoCD_StartsEveryClusterWithItsLocalAdminAccountOff` | `argocd-cm` sets `admin.enabled` to `"false"`, so Argo CD starts with no local `admin` account and generates no password for it | The rendered `components/argocd/argocd.yaml` |
| `TestInit_CommitsTheKeysAsPGPMessagesBeforeAnyRequestCarriesTheRootTokenThenWritesTheReadPolicyOnTheAccessorItRead` | One init with `recovery_pgp_keys`, both keys only as PGP messages gpg reads, committed beside another writer's commit and before any request with the root token, no plain key in a log, and the policy on the accessor read from `sys/auth` | A fake OpenBao over TLS, a bare Git repository behind a fake ssh, and the real git, gpg and openssl |
| `TestInit_ReachesTheRepositoryOverSSHOn443WithTheDeployKeyAndGitHubsPinnedHostKeysAlone` | Every Git call goes through ssh to port 443 with the deploy key alone, strict host key checking and the Job's known hosts file | The same fakes, with the fake ssh recording its arguments |
| `TestInit_ChangesNothingOnAStoreItCannotInitialiseOrThatHoldsItsFileAlready` | A recipient gpg cannot encrypt to, a store initialised without the file, and a file already committed for an uninitialised store each stop the script with that state named. A store initialised with its file committed exits 0. None of them gets a write or a push | The same fakes |
| `TestHosting_TraefikPublishesTheAddressItIsGivenOnEveryIngressInPlaceOfItsNodesAddressesAndChangesNothingElse` | `Set.PublishingIngress` gives Traefik's DaemonSet the one argument `--providers.kubernetesingress.ingressendpoint.ip` with the address, in place of the chart's published Service argument. Every other field of every document stays as the chart renders it, and the set the gateways serve is unchanged | `components.HostingServed` |
| `TestHosting_RefusesAnIngressAddressTraefikCannotPublishAndNamesIt` | Two addresses are refused naming the one Traefik cannot publish, since its ingress endpoint takes one. No address, an unset address and `0.0.0.0` are refused too, and a refusal hands back no set | The same |
| `TestHosting_RefusesATraefikManifestThatDoesNotHoldTheChartsPublishedServiceArgumentOnce` | A Traefik manifest that holds the chart's published Service argument no time or twice is refused naming the count, so a chart that renders the argument another way fails here | `components.HostingServed`, its Traefik manifest edited |
| `TestRelease_TraefikPublishesTheAddressThePlaneOutputsOnEveryIngressAndTheNewOneOnceTheLoadBalancerIsReplaced` | A hosting plane's release names, in Traefik's ingress endpoint, the address `Plane.Outputs` gives as `publicIpByLocation`. Once the load balancer holds another address, the next release names that one and no manifest keeps the first. Traefik's Ingress provider is limited to no namespace, so an app's Ingress in any namespace takes the address | `controlplane/cloudtest`, taken as far as its two load balancers |
| `TestRelease_CarriesNoTraefikWithoutAnAddressAndNamesTheIngressLoadBalancerThatHoldsNone` | A state with no ingress load balancer, or with one that holds no public address, renders no release. The error wraps `ErrNoIngressAddress` and names the location | The same fake |
| `TestHosting_GivesArgoCDTheClusterRepositoryOverSSHOnPort443FromTheSealedReadKey` | What `components.Hosting` returns holds one SealedSecret that opens into a Secret in `argocd` labelled `argocd.argoproj.io/secret-type: repository`. Its data is exactly `type` `git`, the cluster repository's `ssh://git@ssh.github.com:443/` URL and the read-only deploy key. The README section "How Argo CD reads the cluster repository" explains each term | The test helper `testutil.Unseal`, which decrypts a SealedSecret the way the controller does, with the throwaway key pair `testutil.HostingInputs` returns, both in `testutil/hosting.go` |
| `TestHosting_GivesTheInitJobTheWriteKeyInTheNamespaceItRunsIn` | The write deploy key opens as `github-deploy-key` in `mitstack-node`, where the init Job runs | `testutil.Unseal` and the same fixture |
| `TestSeal_SealsAValueOnlyTheKeyPairOpensInTheStrictScopeOfItsSecret` | `components.Seal` writes a SealedSecret that its key pair opens, under strict scope, into exactly the named Secret and value, and `components.Hosting` accepts what it seals | `testutil.Unseal` and a throwaway key pair from `testutil.SealedSecretsKeyPair` |
| `TestSealValue_SealsAValueTheControllerOpensAndOpenReadsOnlyUnderItsSecret` | `components.SealValue` returns a value that `testutil.Unseal` opens, `components.Open` reads it back under its own namespace and name, and refuses it under any other | A throwaway key pair from `testutil.SealedSecretsKeyPair` |
| `TestNewDeployKey_MintsAFreshEd25519KeyWhosePrivateHalfMatchesThePublicHalfARepositoryLists` | `components.NewDeployKey` returns an `ssh-ed25519` public half that matches its OpenSSH private half, and a second call mints another key | `testutil.AuthorizedKey`, which reads a private key's public half |
| `TestHosting_RendersNoExternalSecretGeneratorOrOtherWriterOfArgoCDRepositoryCredentials` | What `components.Hosting` returns holds no ExternalSecret or ClusterExternalSecret, the namespaced and cluster-wide External Secrets objects that write a Secret. It holds no object of the API group `generators.external-secrets.io`, whose objects mint values such as GitHub tokens. Argo CD reads a Secret labelled `argocd.argoproj.io/secret-type` as a credential, and the only object so labelled is the SealedSecret `TestHosting_GivesArgoCDTheClusterRepositoryOverSSHOnPort443FromTheSealedReadKey` checks | Every document of every manifest `components.Hosting` returns |
| `TestHosting_TheInitJobTrustsTheSSHHostKeysArgoCDTrustsForGitHubOn443` | The init Job's known hosts are exactly the `[ssh.github.com]:443` entries of `argocd-ssh-known-hosts-cm` | The vendored Argo CD manifest |
| `TestHosting_LetsTheInitJobReachOnlyClusterDNSOpenBaoAndGitHubsSSHOn443AndOpenBaoTakeItsCalls` | The init Job's pod may reach cluster DNS, OpenBao on 8200 or 15008 and `ssh.github.com` on 443 and nothing else, and OpenBao takes its calls | Cilium's view of each pod's labels |
| `TestPrepareScript_RefusesAManifestWhoseBytesDifferFromItsPin` | The gateway installs no manifest whose sha256 differs from the list, names its path in the tools image, and writes no completion file | A fake docker |
| `TestPrepareScript_InstallsTalosctlAndKubectlFromTheToolsImageWithTheirPinsWhileEveryOutsideURLIsRefused` | With every URL refused, the gateway installs talosctl and kubectl with their `tools.env` pins out of the tools image. It reads that image from its own registry by digest, once the image is copied there. Every copy runs the skopeo image `skopeoImage` in `gateway/gateway.go` pins by digest, which the rendered `tools.env` names as `SKOPEO_IMAGE`, through Docker with `--all --preserve-digests` | A curl that refuses every URL, a fake docker, and a fake skopeo that no copy may call |
| `TestPrepareScript_StopsBeforeTheMirrorIsCompleteNamingTalosctlWhenTheToolsImageCarriesAnotherOne` | A tools image whose talosctl differs from its pin powers the gateway off naming talosctl, with no completion file and no talosctl installed | A fake docker that copies out other bytes |
| `TestPrepareScript_CopiesEveryManifestTheDefaultAndTheHostingSetServeOutOfTheToolsImageWithItsListedSHA256` | With every URL refused, the gateway serves each manifest of the hosting set, which holds the default set's, with the sha256 the rendered `manifests.txt` lists | A fake docker that copies each manifest out of the repository at the path `tools/Dockerfile` copies it to |
| `TestPrepareScript_PowersTheGatewayOffAndLeavesTheMirrorIncompleteWhenACopyFails` | A tools image copy that fails runs `systemctl poweroff`, names the image, writes no completion file, and installs no binary or manifest | A fake docker that fails one image |
| `TestPrepareScript_MirrorsTrustManagersIndexIntoTheGatewaysRegistryWithItsDigest` | trust-manager's OCI image index copies into the gateway's registry, and the registry serves the digest `testutil.HostingImages` pins for it. It runs only under the `mirror` build tag | The real skopeo image, registry image and quay.io, through Docker |
| `TestToolsImage_AGatewaysPreparationCopiesTalosctlKubectlAndEveryServedManifestOutOfItByItsDigest` | The rendered `prepare.sh` reads the built tools image from the gateway's registry by digest and installs talosctl, kubectl and every hosting manifest with its pin. It runs only under the `toolsimage` build tag | The tools image the CI job builds, the gateway's registry image and Docker |
| `TestPrepareScript_PowersTheGatewayOffNamingTheCopyOutOfTheToolsImageWhenTalosctlsCopyHangs` | A copy of talosctl out of the tools image that never ends stops within `EXTRACT_SECONDS`, and the gateway powers off with the copy named on its console and in its issue file | A fake docker that sleeps |
| `TestPrepareScript_PowersTheGatewayOffNamingTheManifestTheToolsImageLacks` | A manifest the tools image lacks powers the gateway off naming its path and Docker's error | A fake docker |
| `TestPrepareScript_PowersTheGatewayOffNamingWhyItCouldNotOpenOrRemoveTheToolsContainer` | A `tools.env` with no tools image, a read of the tools image that fails or hangs past `EXTRACT_SECONDS`, and a container removal that fails each power the gateway off naming the stage, with no completion file | A fake docker that fails or sleeps |
| `TestPrepareScript_PowersTheGatewayOffNamingTheImageWhoseCopyHangs` | A copy that hangs ends within `COPY_SECONDS`, and the gateway powers off naming the image | A fake docker that sleeps |
| `TestPrepareScript_FinishesALargeListCopiedSlowlyButSteadilyWithinEachCopysBound` | Five copies that each take longer than a second finish under a two-second bound per copy | A fake docker that sleeps |
| `TestRender_RunsThePreparationAndTheTLSServersStartEachWithinItsBoundAndPowersOffOnEitherFailing` | The preparation and the TLS server's start each run as a oneshot unit with a bound and `FailureAction=poweroff`, and the plane's deadline covers both | The rendered units and `tools.env` |
| `TestRender_ATLSServerThatDoesNotStartWritesTheStageAndCurlsErrorToTheVGAConsoleAndTheIssueFile` | Each unit's `ExecStopPost` writes the stage, its bound and curl's last error to tty1 and the issue file on one 80-column screen, writes nothing on success, and the preparation's writes nothing over the script's own message | The units' commands run with the console and issue file in a scratch directory |
| `TestRender_NamesTheVGAConsoleAndTheIssueFileAsWhereABuildFailureIsWritten` | `tools.env` names `/dev/tty1` and the issue file, and the cloud-config holds no user, key or password | The rendered cloud-config |
| `TestPlane_RefusesTheBuildNamingTheGatewaysImageMirrorOnceTheGatewayPowersItselfOff` | When the gateway powers off while the builder runs, the plane deletes the builder, refuses with class `gateway-mirror-failed` at least 30 minutes before `BuildDeadline`, the 60 minutes a builder has, and leaves the gateway off | `controlplane/cloudtest` |
| `TestPlane_PowersOnAGatewayFoundOffOnceTheBuilderHasFinished` | A gateway found off once the builder ran and powered itself off is powered on, since its mirror completed | `controlplane/cloudtest` |
| `TestPlane_BuildsANodeInEachOfFsn1Nbg1AndHel1OnOneNetwork` | Calling `NextStep` until it returns none builds a node and a gateway in each location on one network, a builder in `fsn1`, and deletes the builder once its snapshot exists | `controlplane/cloudtest`, a fake of the Hetzner Cloud API |
| `TestPlane_EachCallWaitsOnOneHetznerActionAtMostAndCreatesOneNodeAtMost` | No call waits on more than one Hetzner action or creates more than one node | The same fake |
| `TestPlane_RefusesAnAmbiguousClusterBeforeAnyChange` | Two resources of one role refuse with class `ambiguous-cluster` and change nothing | The same fake |
| `TestPlane_TheInstalledHookReceivesTheClusterWithItsKeysInTheInstallsCall` | The block's `Installed` hook gets the rendered cluster with its secrets in the call that installs it, and a worker rendered from it trusts the control plane's machine CA and joins with its cluster token | The same fake |
| `TestPlane_AnInstalledHookThatFailsRollsTheInstallBack` | A hook that returns an error leaves neither a gateway nor a builder behind | The same fake |
| `TestPlane_AppsOnWorkersKeepsTheControlPlanesTaint` | With `AppsOnWorkers` the control-plane configuration keeps Talos's `NoSchedule` taint, and without it the configuration carries no taint | The configuration the builder embeds |
| `TestPlane_DeleteRemovesEveryResourceItBuilt` | `Delete` leaves nothing that carries the subscription's labels | The same fake |
| `TestPlane_TheKitRequiresNeitherBlockKitNorOperatorKit` | The kit's `go.mod` names neither block framework | `go.mod` |
| `TestCluster_RendersTheRecordedComponentsMirrorAndBuilderApartFromTheNodeReport` | A default cluster's components, mirror list and builder user data match their recording | `controlplane/testdata/default-cluster.golden` |
| `TestPlane_LeavesOneWorkerImageLabelledForTheSubscriptionAndNoWorkerBuilder` | An install leaves one available snapshot with the role `worker-image` and deletes its worker builder | `controlplane/cloudtest` |
| `TestPlane_TheWorkerImageHoldsNoneOfTheClustersCAKeysAndItsUserDataFitsHetznersLimit` | The worker builder's configuration holds none of the control plane's CA or service-account keys, and its user data, whose size the test logs, fits in 32,768 bytes | The same fake and the control plane's rendered configuration |
| `TestPlane_TheWorkerBuilderJoinsTheNetworkAtItsFixedAddressAndBuildsOnlyOnceTheGatewayRuns` | The worker builder runs its build at `10.0.0.252`, with the gateway running | The same fake |
| `TestPlane_DeleteDeletesTheWorkerImage` | `Delete` removes the worker image | The same fake |
| `TestPlane_TheWorkerBuilderGoesWithAClusterBuildThatFails` | A cluster whose build failed loses its worker builder on the next call | The same fake |
| `TestNewCA_RefusesACertificateForAnyOtherAddressOrAHostNameUnderCriticalNameConstraints` | The gateway CA's name constraints are critical, and a certificate it signs for another address or for a host name fails verification | Go's verifier |
| `TestPlane_BuildsAGatewayInEachLocationAtItsOwnAddressUnderOneCA` | An install builds a gateway in each of `fsn1`, `nbg1` and `hel1` at its fixed address, each with a certificate for its address alone under the one CA the `Installed` hook receives | The same fake |
| `TestPlane_NoUserDataOrManifestHoldsTheGatewayCAKey` | No server's user data and no manifest of the component set holds the gateway CA's key | The same fake |
| `TestPlane_MovesTheDefaultRouteOffAGatewayThatStoppedServing` | A routed gateway that is off, or that a node report reads failing, loses the default route to a gateway that serves | The same fake and a stand-in for the node report |
| `TestPlane_AddsTheDefaultRouteOnTheNextCallWhenItsAddFailsAfterItsDelete` | A move whose add fails after its delete fails that call, naming the missing route, and the next call adds it first | The same fake |
| `TestPlane_PowersAStoppedGatewayBackOnOnceAnOutageAndReadsItUnhealthyWhenItStaysDown` | A stopped gateway is powered on once with an event naming why it was off, and one that stops again before a report reads it serving stays off and reads unhealthy | The same fake and a stand-in for the node report |
| `TestPlane_UpgradeReplacesOneLocationsGatewayAtATimeAtItsAddressAndNoNodeAndServesTheReleaseOnlyOnceEachIsReplaced` | An upgrade replaces each gateway built from an earlier list, one location at a time, at the same address, with the route never on a missing gateway and no node replaced, and `GatewaysServeRelease` holds only once every location's gateway is built from this release's list and running | The same fake and a stand-in for the node report |
| `TestPlane_GatewaysAwaitTheReleaseWhileAGatewayOfAnEarlierListStandsAndServeItOnlyOnceEveryLocationsGatewayOfItsListServes` | `GatewaysAwaitRelease` reads true while one location's gateway carries an earlier list, and for a cluster with no network or built with one gateway. It reads false once every gateway is of this release's list, with one failing its probe or one location's gateway lost, and `GatewaysServeRelease` holds only when each serves | The same fake and a stand-in for the node report |
| `TestPlane_AnUpgradeWhoseReplacementsServeOnlyInTheLastReportBeforeTheirDeadlineEndsWithinGatewayUpgradeBound` | An upgrade whose every replacement a report first reads serving in the last interval before its deadline replaces all three gateways within `GatewayUpgradeBound` | The same fake |
| `TestCluster_RunsTheNodeReportAndTheOutsideProbeOnceEveryReportInterval` | The cluster's node report and outside probe CronJobs both run every `report.Interval`, the interval `GatewayUpgradeBound` counts in | The rendered component set |
| `TestPlane_UpgradeWaitsOnAReplacementNoReportReadsServingAndNeitherReplacesTheNextNorServesTheRelease` | A replacement no report reads serving holds the upgrade, `GatewaysServeRelease` never holds meanwhile, and `Problems` names it with the newest report's reading | The same fake and a stand-in for the node report |
| `TestPlane_NamesAReplacementRunningPastItsDeadlineUnhealthyAndKeepsWaitingWithoutDeletingAGateway` | A replacement past its 77-minute deadline is named with the deadline in the step's wait and as `CauseGatewayOverdue` in `Unhealthy`, no gateway is deleted before a node report from after its replacement began, and once read serving it keeps its place while the upgrade moves on | The same fake and a stand-in for the node report |
| `TestPlane_NamesNoDeadlineForAReplacementThatHasNotReachedIt` | A replacement before its deadline is waited on with no deadline named and reads no cause | The same fake and a stand-in for the node report |
| `TestPlane_ReplacesAFailedGatewayOfThisReleasesListAtItsAddressFromTheSameListOnceAFreshReportReadsTheOthersServing` | A gateway of this release's list running past its deadline without serving, or down after its one power-on, loses the route, is deleted on readings from after its replacement began, and its successor at the same address carries the same list and the failed gateway's id | The same fake, the node report stand-in and `internal/redundanttest` |
| `TestPlane_AnUpgradeReplacesItsReplacementPastItsDeadlineWithoutServingFromTheSameListAndMovesOn` | An upgrade's replacement in `fsn1` that never serves is replaced from the same list after its deadline, and the upgrade then replaces `nbg1` and `hel1` | The same |
| `TestPlane_ReplacesNoFailedGatewayWhileASecondGatewayDoesNotServeAndNamesIt` | With a second gateway read not serving, or not read at all, no gateway is deleted, and the events carry the guard's refusal naming that gateway | The same |
| `TestPlane_DeletesNoGatewayAndMovesNoRouteWhileNoNodeReportReadsTheGatewaysPastTheirDeadline` | With no node report at all, or a block that reads none, gateways past their deadline are not deleted, the route stays where it is, and the events carry the guard's refusal naming each gateway it cannot read | The same |
| `TestPlane_LeavesASuccessorThatFailsFromTheSameListInPlaceAndReadsUnhealthyNamingBothGateways` | A successor that fails as the gateway it replaced did stays, and `Unhealthy` reads `CauseGatewayFailedAgain` naming both | The same |
| `TestPlane_KeepsAGatewayAReportReadsServingJustAfterItsDeadlineAndEndsItsReplacement` | A gateway read serving after its replacement began is not deleted, and its replacement label goes | The same |
| `TestPlane_DeletesAFailedGatewayOnTheCallAfterHetznerRefusedItsDeleteWithNoSecondRouteMove` | A delete Hetzner refuses fails that call naming the gateway, and the next call deletes it with the route left where it is | The same |
| `TestPlane_ReadsAFailedGatewayUnhealthyNamingItAndTheReplacementUnderWay` | A failed gateway reads `CauseGatewayOverdue` or `CauseGatewayDown`, each naming the plane's replacement, with a detail naming the gateway and when its replacement began | The same |
| `TestPlane_LeavesAGatewayOfAnEarlierListDownAfterItsPowerOnToAnUpgradeAndNamesThatRemedy` | A gateway of an earlier list down after its power-on is not replaced, and reads `CauseEarlierGatewayDown`, whose remedy is an upgrade | The same |
| `TestPlane_UpgradeWaitsOnAReplacementNoReportNamesAndNamesTheReportThatMissedIt` | A replacement no report names holds the upgrade as one read failing does, and `Problems` names the report that missed it | The same fake and a stand-in for the node report |
| `TestPlane_UpgradeDeletesNoGatewayWhileASecondEarlierListGatewayDoesNotServeAndNamesIt` | With a second gateway read not serving, or not read at all, the upgrade deletes nothing, and its event carries the guard's refusal naming that gateway | The same fake, the node report stand-in and `internal/redundanttest` |
| `TestPlane_UpgradeReplacesTheEarlierListGatewayThatDoesNotServeBeforeAnyThatServes` | A gateway that never served, in `fsn1` or in `hel1`, read failing or never read at all, is the first replaced, and its deletion event names each other gateway read serving | The same |
| `TestPlane_UpgradeDeletesAGatewayOnlyOnReadingsReceivedAfterItsReplacementBegan` | Each deletion names when its replacement began and rests only on readings from after it, each naming the node report it came from, received after that | The same |
| `TestPlane_UpgradeBeginsAGatewaysReplacementAgainWhenAnEarlierOneBeganForAnotherList` | A replacement label left by an upgrade to another list counts for nothing, so each deletion is judged since this upgrade began | The same |
| `TestPlane_MovesTheDefaultRouteOnlyToAGatewayAReportReadsServing` | The route leaves a failing gateway for one a report reads serving, passing over one no report names | The same fake and the node report stand-in |
| `TestPlane_AnUpgradesReplacementsMirrorAnImageOfTheReleaseBeforeThatARunningPodNamesAndOneACronJobsTemplateNames` | Each upgrade replacement mirrors the release's list and both images the newest report names, and its list label stays the release list's digest | The same fake and a stand-in for the node report |
| `TestPlane_ALostGatewaysRebuildLeavesOutAnImageNoPodAndNoTemplateNamesAnyMore` | A rebuild mirrors only what the newest report names, and a running gateway keeps what it was built with | The same |
| `TestPlane_ALostGatewaysRebuildKeepsTheImagesAnUpgradesReplacementKeeps` | A rebuild and an upgrade's replacement read the same report into the same mirror list | The same |
| `TestPlane_CreatesNoGatewayWhileTheNewestNodeReportPredatesTheLocationsLossAndNamesItsReceivedAt` | A location that lost its gateway gets none while the newest report predates the loss, `Problems` names that report's time, and the label recording the loss goes once the gateway is back | The same |
| `TestPlane_CreatesNoGatewayFromANodeReportReceivedInTheSecondBeforeTheLossWasSeen` | A report from half a second before the plane saw the loss counts as before it, since the record rounds up | The same |
| `TestPlane_CreatesNoGatewayOnceNodesRunWhileTheClusterHasSentNoNodeReport` | With nodes running and no report, a lost gateway is not rebuilt, and `Problems` says no report came | The same |
| `TestPlane_LeavesOutAnUnresolvedTagAndAnImageOnARegistryTheNodesReachDirectlyAndNamesEachBesideWhatItKeeps` | A tag with no digest and an image outside the mirrored registries stay out, a short name is mirrored under its Docker Hub name, and the event names each image kept and each left out with why | The same |
| `TestPlane_AnUpgradeReplacesNoGatewayWhileTheNewestNodeReportListsNoImages` | An upgrade whose newest report has no `images` refuses with class `gateway-images-unread` and makes no write | The same |
| `TestPlane_AGatewayKeeping300ImagesStaysInsideHetznersUserDataLimit` | A gateway keeping 300 images fits in 32,768 bytes, on a default cluster and one that hosts the platform, and the test logs that size and the install's first gateway's | The same |
| `TestPlane_EveryServerOfAnInstallKeepsTheUserDataReserveWithTheLongestProofScriptUnderEachOptionOfThePlane` | Every server an install creates keeps 1,024 of Hetzner's 32,768 bytes free, on a cluster and on one that hosts the platform, with apps on the control plane and on workers. The plane's proof script is `report.ProofScriptLimit` bytes of random printable text, and the test logs each server's size, as [the README's section on the builder's user data](../README.md#what-fits-in-the-builders-user-data) explains | The same |
| `TestPlane_AnUpgradeWithNoGatewayToReplaceChangesNothingThoughNoReportNamesAGateway` | An upgrade of a cluster whose gateways all carry the current list makes no write and reads no problem, though no report names a gateway | The same fake |
| `TestPlane_APinBumpReplacesNoGatewayWithoutAnUpgrade` | Gateways built from an earlier list stay without `Upgrading` | The same fake |
| `TestPlane_KeepsAClusterBuiltWithOneGatewayAndReadsItUnhealthyWithItsRemedy` | A cluster with one unlabelled gateway keeps it, and `Unhealthy` names the rebuild | The same fake |
| `TestBuilder_TheOutsideProbeReadsEachGatewaysMirrorUnderTheGatewayCAAndTheReportATLSConnectionToLetsEncrypt` | The builder's outside probe names each gateway's address, trusts the install's gateway CA by its certificate alone, and the report opens its egress connection to Let's Encrypt's ACME directory | The configuration the builder embeds |
| `TestBuilder_EveryManifestTheNodeAppliesDecodesToTheSameObjectsOnceTalosTrimsEachOfItsDocuments` | Each manifest a control-plane node applies, inline or served, gives the same objects when each document is trimmed of the blank space around it as when it is not, for the default set, the hosting set and a cluster whose logs go to a receiver | `testutil.TalosObjects`, which decodes a manifest the way `SetYAML` does in Talos v1.14.4 |
| `TestHosting_InstallsEveryNodeWithTheBuilderFetchingTheHostingSetFromTheServingServer` | A hosting install creates a node in each location, with the serving server at `10.0.0.251` serving to the builder alone and the builder pinning its key, and deletes the serving server after the build | The same fake |
| `TestHosting_TheBuildersUserDataStaysWithinHetznersLimitAndHoldsNoHostingManifest` | With a 4096-bit Sealed Secrets key the builder's user data, whose size the test logs, fits in 32,768 bytes and carries none of the hosting set's inline manifests | The same fake |
| `TestHosting_TheServingServerGoesWithABuildThatFails` | A build past its deadline takes the serving server with the builder | The same fake |
| `TestBuildImage_HandsImagerTheConfigurationWithTheHostingSetTheServingServerServed` | The build script fetches the hosting set from the real serving program over pinned TLS, imager gets it appended to the configuration, and the served file is gone | `serve/scripts/serve.py` on a local port, and fake build commands |
| `TestBuildImage_AServerThatFailsThePinGetsNoBuildAndTheConfigurationIsRemoved` | A server whose key fails the pin gets no build, and the script removes the configuration | The same |
| `TestClusterTokens_EnrolOnAClusterThatReadsItsRepositoryWithADeployKeyWritesNoRepositoryCredential` | With `REPOSITORY_CREDENTIAL=deploy-key` the enrolment writes `mitstack-report-token` and `argocd-notifications-secret` alone, and `renew` refuses | Fake kubectl and a local HTTPS server playing the platform |
| `TestClusterTokens_EnrolGivesArgoCDsWebhookThePlatformAndTheNotificationToken` | With either credential the enrolment writes the platform's URL as `platform-url` and the answer's `notificationToken` as `platform-token` in `argocd-notifications-secret` | The same |
| `TestArgoCD_NotifiesThePlatformTheEnrolmentNamesWithItsTokenOfEveryApplicationsOutcome` | `argocd-notifications-cm` posts to `$platform-url/v1/gitops-notifications` with `Bearer $platform-token`, on three triggers every Application subscribes to | The served `components/argocd/argocd.yaml` |
| `TestArgoCD_TheWebhookPostsTheRevisionTheOperationSyncedElseTheComparedOne` | The webhook's body, run with stand-ins for its two template functions, is JSON with the eight fields, the operation's synced revision first and the compared one when there is no operation | The same |
| `TestClusterTokens_EnrolRefusesAnAnswerMissingATokenBeforeWritingAnySecret` | A 200 answer missing the report, repository or notification token fails the enrolment before it writes any Secret | Fake kubectl and a local HTTPS server playing the platform |
| `TestClusterTokens_EnrolFailsASpentTokenWhoseNotificationsSecretIsMissing` | A spent enrolment token with no `argocd-notifications-secret` fails the Job on either credential | The same |
| `TestBuilder_TheEnrolmentAndTheRenewalRunTheClusterTokensScriptTheToolsImageCarries` | The enrolment Job and the renewal CronJob each run `cluster-tokens.sh` from `tools.ScriptDir` in the tools image, with `enrol` or `renew`, so the builder's user data carries no copy of the script | The builder's rendered configuration on the Cloud fake |
| `TestProgram_ServesTheFileOnceToItsClientOverTLS13AndRemovesTheFileAndTheKey` | The serving program answers its one client once over TLS 1.3, and deletes the file and its key | The program run with python3 on a local port |
| `TestHetznerAdapter_CouplesTheVSwitchThroughItsSubnetAndExposesTheNetworksRoutesToIt` | `AddVSwitchSubnet` sends a `vswitch` subnet with the vSwitch's id and turns on `expose_routes_to_vswitch` | A local HTTP server playing Hetzner's Cloud API |

The fixtures in `testutil/testdata/` are throwaway keys made for these tests:
a Talos secrets bundle, a gateway CA with its certificate and key, and a log
receiver's CA. The script tests need `bash`, `curl`, `jq`, `gpg`, `git`, `python3` and
`openssl` on `PATH`. The report's tests also need linux/amd64 and, on their
first run, network access to github.com, where they fetch the jq `tools/jq.env`
pins into the user cache directory.

```bash
go test -race ./...
go test -run TestRemoveMemberJob ./etcd/
```

## What CI runs

`.github/workflows/ci.yml` runs on every pull request, every push to `main`
and a manual run. Every job runs on the runner the repository variable
`CI_RUNS_ON` names, `ubicloud-standard-2` by default.

| Job | What it proves |
|---|---|
| `test` | On a pull request, `checkpolls`, the check that block-kit, the shared kit every block builds on, ships for added sleeps and polling intervals, judges every line the change adds against its base commit and fails a `time.Sleep`, a ticker or a shell `sleep`, naming each file and line. It runs as a tool of the `ci` module, a Go module of its own under `ci/`. Its `ci/go.mod` alone names the block-kit release, because a tool in the kit's own `go.mod` would force that release on every block that requires the kit. A human reviewer judges the waits `checkpolls -help` says the check cannot see, such as a timer reset inside a loop or a `time.After` in a select. `go vet ./...` and `go test -race ./...` then pass on Go 1.26.10, and the `mirror check` step copies trust-manager's index through the gateway's skopeo into its registry |
| `components` | Each rendered component matches a fresh render, and shellcheck passes on every script |
| `tools image` | The tools image builds from the repository root, each of its commands runs, and it holds the report's, its outside probe's, the labeller's, the cluster tokens' and the handover's scripts byte for byte as the repository does. The outside probe and the security scan then write their ConfigMaps through a real API server, the scan's kube-bench step reading a stand-in control plane. A component's handover takes Talos's fields off Cilium's objects through that API server. A gateway's preparation then copies talosctl, kubectl and every served manifest out of it with their pins, as [the section on the tests](#how-the-tests-run-the-scripts) describes |
| `pr key gate` | The pull request's title and at least one of its commits each name a board key, the `MIT-` number of an issue on Plain, mitstack's board. A key counts when it opens the subject followed by a colon, as in `MIT-123: fix the thing`, which delivers the issue, or when it closes the subject as `(refs MIT-123)`, which only names the issue and is the form the kit's commits and titles use. A change with no issue has no key to name, so the job fails it, and the issue is filed on the board first |
| `release` | On a push to `main` with a new version in `.github/version`, and only after `test`, `components` and `tools image` pass, it pushes and signs the tools image and pulls it with no login. It then tags a commit on top that pins the image in `tools.Image`, as [the README](../README.md#how-a-release-is-versioned) describes |
| `record delivery` | On a push to `main`, records on Plain each issue a commit of the push delivers, which means a subject that opens with `MIT-123:`. A subject that only ends with `(refs MIT-123)` records nothing |

## What the kit leaves to its blocks

Every package but `controlplane` renders bytes, so each of these stays with
the block that calls it:

| Step | Why the kit does not do it |
|---|---|
| Delivering the admin talosconfig to the gateway and running the bootstrap there | It needs the block's command channel to the gateway |
| Taking an etcd snapshot and storing it | Where a snapshot lives is the block's choice |
| Keeping the gateway CA's key | The key must live in the block's own store, which the block passes back as `Plane.GatewayCA` |
| Replacing a gateway before its certificate's year ends | The plane replaces a gateway only for an upgrade or a failure, and the block chooses when to upgrade |
| Writing the report token into `mitstack-report-token` | The token comes from the platform at enrolment |
| A dedicated node's address and its vSwitch, Hetzner's private network between dedicated servers | Which dedicated servers a cluster adopts, and over which vSwitch, is the block's choice |
