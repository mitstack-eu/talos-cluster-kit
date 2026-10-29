# talos-cluster-kit

This repository holds the Talos install that mitstack's Hetzner Talos cluster
blocks share. It is for an engineer who changes what a Talos node boots, what
its cluster starts with, or how etcd recovers. A block is a unit of
infrastructure an app subscribes to, as
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines. hcloud-talos-cluster imports this module to run a cluster on Hetzner
Cloud servers. hcloud-hrobot-talos-cluster runs its control plane on Cloud
servers from the `controlplane` package, and builds its workers on Hetzner
dedicated servers from the module's worker render, as
[ADR 0103](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0103-mitstack-offers-five-hetzner-capacity-blocks-and-keeps-the-k3s-block.md)
decides. hcloud-talos-cluster still keeps its own copy of that control plane.

Talos Linux is an operating system that runs only Kubernetes and is managed
through an API, with no shell and no SSH. The module follows the fourth Talos
proof of 19 October 2026, recorded in the workspace note
[Talos on Hetzner Cloud from an embedded configuration](https://github.com/mitstack-eu/mitstack-workspace/blob/main/product/knowledge/2026-10-19-talos-on-hetzner-cloud-from-an-embedded-configuration.md),
and the design that
[ADR 0095](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0095-mitstack-clusters-run-talos-across-fsn1-nbg1-and-hel1-with-a-load-balancer-per-location.md)
accepts.

The module renders and the block acts. Every package but `controlplane` takes
its inputs as values and returns bytes, or runs one local command, and calls
neither Hetzner, GitHub nor the platform. `controlplane` is the one package
that calls Hetzner: it creates and deletes a Cloud control plane through
Hetzner's Cloud API, with the token the block hands it. It imports neither
block-kit nor operator-kit, so any block framework can drive it. Keeping the
install in one module means a fix to the boot, to etcd recovery or to the
Cloud control plane lands once and reaches every block.

## What a cluster is built from

A node boots from a disk image that embeds its whole machine configuration, so
the server needs no user data. The nodes have no public address. A gateway in
each location is the cluster's only server with one: it forwards the nodes'
DNS, NTP and HTTPS, records that traffic, and serves the image mirror and the
larger manifests over TLS on the private network. The table names the parts in
the order a block uses them.

| Package | What it does | Main entry points |
|---|---|---|
| `components` | Holds the pinned component set: Cilium, the custom resource definition (CRD) of Cilium's cluster-wide policy, Argo CD, the policy that keeps pods from Hetzner's metadata service at `169.254.169.254`, Istio's ambient mesh with its mesh-wide mutual TLS policy, Kyverno with its four admission policies, the CloudNativePG operator with the database-slice policy, the local path provisioner behind the default StorageClass, and the node-local OpenTelemetry Collector. It also holds the boot set of a cluster that hosts the platform | `Default`, `Hosting`, `Set.Images`, `LogCollector`, `ParseImage` |
| `nodeconfig` | Renders a control-plane node's or a worker node's machine configuration with Talos's own `machinery` module and validates it against Talos's schema | `Render`, `RenderWorker`, `Cluster`, `Node`, `NewSecrets`, `ParseSecrets`, `Secrets.AdminTalosconfig` |
| `image` | Runs imager, Sidero Labs' open-source image builder, with the `hcloud` or the `metal` profile, and discards its output | `Run`, `Build` |
| `mirror` | Lists every image the nodes pull, each by digest | `List`, `Render` |
| `gateway` | Renders the cloud-config of the gateway in each location, and mints the cluster's gateway CA and each gateway's certificate | `Render`, `Inputs`, `NewCA`, `CA.Issue`, `ReleaseURL` |
| `etcd` | Renders the Job that removes a lost node's etcd member and then its Node object, and holds the gateway's script that bootstraps etcd empty or from a snapshot | `RemoveMemberJob`, `BootstrapScript`, `Bootstrap.Args` |
| `report` | Renders the node report, a CronJob that posts each node with its Talos version and allocatable resources, the etcd membership, how each node's `STATE` and `EPHEMERAL` are encrypted, Argo CD's health with the objects each Application applied, CloudNativePG's clusters, databases and roles, the objects a block labels with its subscription, and the API server's key set to the platform, with a `checks` member declaring which of the platform verdict's checks it carries. When any read but the volume, CloudNativePG and labelled-object reads fails or times out, it posts a report holding only an `error` that names the read, in place of the full report. The report holds no right on Secrets | `Manifests` |
| `openbaoinit` | Renders OpenBao's init Job, which the hosting set carries: it initialises a hosting cluster's OpenBao once, commits the root token and recovery key to the cluster repository as PGP messages, then writes the policy each slice's External Secrets reads with | `Manifests`, `Inputs`, `InitFile` |
| `controlplane` | Builds and deletes a Talos control plane on Hetzner Cloud: a network, a firewall, a gateway, a builder that makes the cluster's own snapshot, a node in each of `fsn1`, `nbg1` and `hel1`, and an API and an ingress load balancer. Each call reads the resources by their labels and takes the one change still missing. A refusal is a `*Refusal` with a `Class` and a `Message`, which the block answers as its own framework's refusal | `Plane`, `Plane.Read`, `Plane.EnsureNetwork`, `Plane.NextStep`, `Plane.Problems`, `Plane.Outputs`, `Plane.Delete`, `NewAPI`, `Refusal` |
| `tools` | Holds `tools/tools.env`, which pins the talosctl and kubectl that the tools image and the gateway both install, and `tools/jq.env`, which pins the tools image's jq, and names the tools image a release published | `Env`, `Pin`, `Image` |
| `testutil` | Holds the fixtures, fakes and decoders this module's tests and a block's tests share | `NewCluster`, `NewTools`, `Golden` |

For example, this renders the configuration a Cloud cluster's image embeds,
for a cluster with one location:

```go
cfg, err := nodeconfig.Render(nodeconfig.Cluster{
	Name:                 "shop",
	SubscriptionID:       "sub_0123456789ab",
	Profile:              nodeconfig.HCloud,
	Components:           components.Default(),
	Secrets:              secrets, // from nodeconfig.NewSecrets(time.Now())
	Network:              netip.MustParsePrefix("10.0.0.0/16"),
	APIAddresses:         []netip.Addr{netip.MustParseAddr("10.0.0.3")},
	Gateways:             []netip.Addr{netip.MustParseAddr("10.0.0.2")},
	GatewayCA:            gatewayCA.Cert, // from gateway.NewCA
	Logs:                 components.LogExport{Endpoint: "otlp.mitstack.dev:4317", CA: receiverCA}, // the log receiver's CA
	ServiceAccountIssuer: issuer, // the issuer every token names, such as https://mitstack-oidc-7kq2m4xw3pa3hd6rz5tn2cjvle.s3.eu-central-1.amazonaws.com/sub_0123456789ab
}, nodeconfig.Node{})
```

Log export is optional. A builder whose cluster has no log receiver leaves
`Logs` at the zero `components.LogExport`. The configuration then carries no
Collector and no log destination, and the gateway's `Inputs.Logs` left at zero
forwards HTTPS alone. `nodeconfig.Render` refuses a `LogExport` with only one
of its two fields set.

A Cloud cluster renders once with an empty `Node`, because every node boots
from one snapshot and takes its name from Hetzner's metadata service. A
dedicated node has no metadata service, so a dedicated cluster renders once per
node with that node's `Node.Hostname`.

A worker is a node that runs the apps' pods and holds no key of the cluster's
certificate authorities. It joins the cluster through
`nodeconfig.RenderWorker`, which takes the same `Cluster` and the same secrets
as `Render`. `Cluster.Profile` names the
server the configuration boots on. A block whose control plane runs on Cloud
servers therefore renders a dedicated worker from a copy of its `Cluster`
with `Profile` set to `nodeconfig.Metal`:

```go
robot := cluster // the Cluster the control plane rendered from
robot.Profile = nodeconfig.Metal
cfg, err := nodeconfig.RenderWorker(robot, nodeconfig.Node{
	Hostname: "wk-hel1",
	VSwitch: &nodeconfig.VSwitch{
		// The public card's permanent MAC, read in the rescue system, such as 9c:6b:00:55:d8:f5.
		MAC: mac,
		// The vSwitch's VLAN in Robot.
		VLAN: 4000,
		// The node's address in the network's vSwitch subnet.
		Address: netip.MustParsePrefix("10.0.1.2/24"),
	},
})
```

A dedicated server reaches the cluster's network over a vSwitch, Robot's
private VLAN between dedicated servers, which a Hetzner Cloud network couples
to. `Node.VSwitch` gives the node three documents. A `LinkAliasConfig` names
the public card `pub0` by its permanent MAC, since Talos's names for links
are not known before the first boot. A `LinkConfig` brings `pub0` up with no
address, which keeps Talos's default DHCP off it. A `VLANConfig` gives the
node its address on `vsw0`, with MTU 1400 and its default route through the
vSwitch subnet's first address, Hetzner's vSwitch gateway. These are the
documents that booted the node in the workspace note
[Talos on a Hetzner Robot server behind a vSwitch gateway](https://github.com/mitstack-eu/mitstack-workspace/blob/main/product/knowledge/2026-10-28-talos-on-a-hetzner-robot-server-behind-a-vswitch-gateway.md).
`vswitchDocuments` in `nodeconfig/documents.go` renders them, and a Metal
node without `VSwitch` runs Talos's default DHCP on every link.

## How a block drives the Cloud control plane

The Cloud control plane is the part of a cluster that runs on Hetzner Cloud
servers: a Talos node in each of `fsn1`, `nbg1` and `hel1`, which run
Kubernetes itself, and the gateway, a Cloud server that gives the nodes their
only route out and serves them their images and manifests. The plane builds
the nodes from the cluster's own snapshot, a Hetzner disk image that a
short-lived builder server writes and that holds every key of the cluster.

The platform calls a block's verbs, such as subscribe, and gives each call
two minutes, while a control plane takes far longer to build. So
`controlplane.Plane` keeps no state of its own. Every resource it creates
carries the labels `mitstack.dev/block`, `mitstack.dev/subscription` and
`mitstack.dev/role`. The roles are `gateway`, `builder`, `node`, `image` for
the snapshot, `worker-builder`, `worker-image`, `serving`, `api` and
`ingress`. Each call reads the labels back and takes
the one change still missing, such as creating the firewall once the network
exists. A block names its plane once per call, from its own id and the
subscription:

```go
plane := controlplane.Plane{
	API:             controlplane.NewAPI(token, "hcloud-hrobot-talos-cluster"), // token: the app's Hetzner Cloud API token
	Block:           "hcloud-hrobot-talos-cluster",
	Subscription:    "sub_0123456789ab",
	AppName:         "shop",
	ServerType:      "cx33",
	Boundary:        "mitstack-hcloud-hrobot-talos-cluster", // separates the parts of the gateway's user data
	PlatformURL:     "https://api.mitstack.dev",             // where every node registers and reports
	RepoURL:         "https://github.com/acme/shop-cluster", // the Git repository Argo CD applies to the cluster
	Installed:       installWorker,                          // the block's own func(ctx, controlplane.Network, nodeconfig.Cluster) error
	AppsOnWorkers:   true,
	WorkerAddresses: []string{"10.0.1.2"}, // the Robot worker's address in the network's vSwitch subnet
}
st, err := plane.Read(ctx)
```

A call then runs in this order:

1. `plane.Refusal(st)` returns a `*Refusal` when the labelled resources are
   ambiguous, such as two gateways. The block stops and answers it as its
   framework's refusal, with the `Class` and `Message` it carries, before any
   change.
2. `plane.EnsureNetwork(ctx, st)` creates the network when none exists and
   returns it.
3. `plane.NextStep(ctx, st, network)` returns the next change with a name,
   such as `create the firewall`, and the block runs it with `Step.Take`. Each
   change waits on one Hetzner action at most.

When `NextStep` returns no change, the work in flight is Hetzner's or the
nodes', such as the builder writing the snapshot. The block answers that the
subscription is still converging, and the platform calls it again later. `Problems` names what is wrong with a
plane that stands, `Outputs` gives the private API endpoint and the ingress
address the block provides, and `Delete` removes everything the labels find.

`Installed` runs once, inside the `NextStep` change that creates the gateway
and the builder, since that change renders the cluster's configuration.
That call is the only moment the cluster's keys exist outside the snapshot,
so a block that adds workers renders them there with
`nodeconfig.RenderWorker`. When `Installed` returns an error, the plane
deletes the gateway and builder it just created, and a later call starts the
install again.

Every install also builds a worker image, so a later upgrade or scaling
creates Cloud workers from it with no rebuild. The same change that creates
the builder creates a second one, the worker builder, from the configuration
`nodeconfig.RenderWorker` returns. That configuration holds none of the
cluster's CA keys. The worker builder joins the network at `10.0.0.252`, and
it starts once the gateway runs, beside the cluster's own builder. Its snapshot carries the role `worker-image`,
and the plane deletes the worker builder once that snapshot is available.
`Delete` removes the worker image with the rest.

A block that installs a cluster hosting the platform sets `Plane.Hosting`. The
section [How the hosting set reaches the builder](#how-the-hosting-set-reaches-the-builder)
explains what changes.

`AppsOnWorkers` decides where an app's pods run. Off, as for a cluster with
Cloud nodes alone, every pod may run on the control-plane nodes. On, the
control-plane nodes keep Talos's `NoSchedule` taint, a mark Kubernetes reads
as "schedule no pod here unless the pod tolerates it", so an app's pods run
only on the workers. Cilium, the cluster's network, ztunnel, the mesh's agent
on each node, and the log collector tolerate the taint and still run on every
node. The node report and the etcd member removal Job tolerate it too, and run
only on control-plane nodes, since each reads etcd through the Talos API of
the node it runs on. Every other pod, Argo CD and the registration Job among
them, waits Pending until a worker joins.

A cluster that hosts the platform keeps its stores on the control plane and
its web traffic on the workers. OpenBao's three members tolerate the taint,
and the local volumes the hosting set pins to the control-plane nodes keep one
member on each. Traefik, cert-manager, trust-manager, External Secrets,
Sealed Secrets and OpenBao's init Job tolerate no taint, so they run on the
workers with Argo CD and the apps. Each member mounts the certificate
cert-manager issues, so the members start only once a worker has joined and
runs cert-manager.

Traefik runs where apps run, so `AppsOnWorkers` also moves the ingress load
balancer's targets. Off, it targets the nodes by their label. On, it targets
each address in `WorkerAddresses` and no control-plane node, so the nodes
that hold etcd and the Kubernetes API take no internet traffic. A worker
address must sit in a vSwitch subnet of the network, as Hetzner's [load
balancer FAQ](https://docs.hetzner.com/networking/load-balancers/faq/) requires
of a private target. The plane adds the target only once the block has
coupled that subnet, and removes any control-plane target the load balancer
holds.

`plane.go`, `converge.go`, `install.go` and `observe.go` hold the plane, and
`api.go` holds the Hetzner Cloud client behind the `API` interface. The
package's tests run against the fake in `controlplane/cloudtest`, which a
block's tests can share.

### The order a cluster boots in

A cluster boots in one order: the gateway, then a complete mirror, then the
nodes. A node's kubelet pulls its image from the gateway's mirror once, with
no fallback to a public registry, and Talos restarts a failed kubelet only when
its inputs change. A node that boots before the mirror holds its images
therefore stays down until Talos's boot timeout reboots it, about 70 minutes
later.

The gateway's `gateway/scripts/prepare.sh` fills the mirror, fetches the
served manifests, and only then writes `/var/lib/mitstack-gateway/mirror-complete`.
Caddy, which serves both on the private network, starts only once that file
exists, so the gateway answers no `/v2/` request before the mirror is
complete.

The plane reaches the cluster only through Hetzner's API, and the gateway
answers nothing from outside the private network, so the builder carries the
signal. `controlplane/scripts/build-image.sh`, which both builders run, builds
the image while the mirror fills, then asks `https://10.0.0.2/v2/` under the
gateway's CA every 10 seconds, for about 40 minutes at most, until it answers.
Only then does it write its disk and power off. The plane creates a node only
from the snapshot of that disk, so every node boots after the mirror is
complete. While the builder waits, `NextStep` returns no change and `Problems`
says the builder is building the image or waiting for the mirror.

`BuildDeadline`, 60 minutes from the builder's creation, covers the build, that
wait and the disk write, so a mirror that never answers fails the script first.
The plane deletes a builder still running past it and refuses the cluster as
`image-build-failed`, naming where the
builder's CPU and disk last moved.

The gateway copies each image with skopeo, a tool that copies container images
between registries. `skopeoImage` in `gateway/gateway.go` pins the skopeo
image by digest, the rendered `tools.env` names it as `SKOPEO_IMAGE`, and
`prepare.sh` runs it through the gateway's own Docker. Ubuntu 24.04's packaged
skopeo, version 1.13.3, cannot copy an OCI image index, the list that names one
image per CPU architecture, and keep its digest unchanged. trust-manager, one of
the hosting set's components, ships as such an index.

A copy that fails powers the gateway off, because the plane holds no
credential into the cluster and reads only what Hetzner reports. When the
gateway has run and is off before the builder finished, `NextStep` deletes the
builder and refuses the cluster with the class `gateway-mirror-failed`, whose
message names the gateway's image mirror. That refusal comes on the plane's
next call after the gateway powers off, and the plane's tests require it at
least 30 minutes before `BuildDeadline`.

## What the configuration holds

`nodeconfig.Render` starts from the configuration Talos itself generates for a
control-plane node and changes it in these ways. `nodeconfig.RenderWorker`
starts from Talos's worker configuration and makes the same changes, except
where the last row says.

| Concern | What the rendered configuration does | Where |
|---|---|---|
| DNS and time | Every node asks Hetzner's resolvers `185.12.64.1` and `185.12.64.2` and syncs from `ntp1.hetzner.de`, `ntp2.hetzner.com` and `ntp3.hetzner.net`, and Sidero's discovery service is off | `resolverDocument`, `timeSyncDocument` in `nodeconfig/documents.go` |
| Images | `docker.io`, `ghcr.io`, `public.ecr.aws`, `quay.io` and `registry.k8s.io` go to every gateway's mirror in turn, with no fallback to the public registry | `mirrorDocuments` |
| Components | Cilium, its policy CRD, Argo CD, Kyverno, the CloudNativePG operator, the local path provisioner and Istio's four charts come from the first gateway over TLS. The metadata policy, the four admission policies, the `cnpg-system` and `local-path-storage` namespaces, the database-slice policy, the provisioner's flow policy, the mesh's policies and the Collector sit in the configuration | `manifestDocuments` |
| Logs | Talos sends service and kernel logs to the node-local Collector on `127.0.0.1`, which exports them over OTLP with TLS verified. A cluster whose `Logs` is the zero `LogExport` runs no Collector and keeps its logs on the node | `adaptV1Alpha1`, `kernelLogDocument`, `components.LogCollector` |
| Networking | Flannel and kube-proxy are off, since Cilium replaces both, and etcd and the kubelet bind to the private network | `adapt` |
| Default route | A Cloud node keeps DHCP on `eth0` and routes everything outside the network through Hetzner's router at the network's first address. The block's network route of `0.0.0.0/0` then carries that traffic to the gateway. Hetzner's DHCP gives the node no default route, and Talos runs no default DHCP on a link that has a `LinkConfig`. A dedicated node with a vSwitch routes through the vSwitch gateway instead | `routeDocuments`, `vswitchDocuments` |
| Talos API from pods | Only pods in `mitstack-node` may hold a Talos identity, with `os:admin`, `os:etcd:backup` or `os:reader` | `talosAPIAccessDocument` |
| Disk encryption | `STATE` and `EPHEMERAL` are LUKS2 volumes, each under a key Talos derives from the node's UUID and the volume's name. `EPHEMERAL`'s key also takes the random salt its node's `STATE` holds. Talos 1.14's disk image holds neither volume, so every node formats and encrypts both on its first boot, under its own UUID and its own salt. The keys protect a disk that leaves its server, and not against whoever holds the Hetzner project's token | `systemVolumeDocuments` |
| User volumes | Each user volume the set names, `local-path-provisioner` on every cluster and OpenBao's in the hosting set, is a directory Talos mounts at `/var/mnt/<name>` | `userVolumeDocuments` |
| Service-account issuer | Every service-account token names `Cluster.ServiceAccountIssuer` as its issuer, which Talos passes to the API server as `--service-account-issuer`. `Render` refuses a cluster with no issuer, or with one that is not https, has no host, or carries a query or fragment. [How AWS comes to trust a cluster's tokens](#how-aws-comes-to-trust-a-clusters-tokens) says why | `adapt` in `nodeconfig/documents.go`, `checkIssuer` in `nodeconfig/nodeconfig.go` |
| Worker nodes | A worker joins through the first private API load balancer and carries no manifest and no Talos API access, since only a control-plane node applies either. Its secrets are the Talos and Kubernetes CA certificates, the trustd and bootstrap tokens and the discovery identity. It holds no CA key, so neither the etcd authority's key nor the service-account signing key | `RenderWorker`, `documents` |

A control-plane node's configuration holds every key of the cluster, its
certificate authorities included, but no admin client certificate. Talos's etcd
bootstrap accepts only `os:admin`, its full-rights role, so the gateway runs
it once with a short-lived talosconfig from `Secrets.AdminTalosconfig` and
deletes that file on exit.

### The order the manifests apply in

Talos applies a control-plane node's manifests in the order of their names,
after it moves every Namespace and then every CRD to the front. A create that
fails for any reason but a malformed object ends the pass, and Talos tries the
whole pass again later. A webhook that cannot answer yet therefore holds back
every object after the one it refuses.

`applyOrder` in `components/order.go` sets that order, and `TalosName` puts
each manifest's place in it before its name. A node's configuration therefore
names Cilium `00-cilium`, and the gateway serves it at `/manifests/00-cilium.yaml`.
The order runs in four groups.

| Group | Manifests | Why it comes there |
|---|---|---|
| Mesh dataplane | Cilium, its policy CRD, Istio's base chart, istiod, istio-cni and ztunnel | A pod outside `kube-system`, `istio-system` and `kyverno` starts only once istio-cni and ztunnel run |
| Cluster reporting | The node report, the cluster tokens and the Collector | A component that never starts still leaves the platform its report |
| Components and policies | Every other component, policy, key and volume | No object here waits on a webhook but Istio's, whose pod runs outside the mesh |
| What a webhook validates | external-secrets and trust-manager, whose charts hold cert-manager objects, then `mitstack-hosting-tls` | cert-manager's and trust-manager's webhooks answer only once their pods run |

A new manifest takes a place in `applyOrder`, since `TalosName` panics on one
it leaves out. `TestBuilder_TalosAppliesEveryObjectAWebhookInterceptsAfterTheWebhookTheMeshAndTheNodeReport`
in `controlplane/render_test.go` sorts the Default and hosting sets as Talos
v1.14.4 does. It fails when an object a webhook's rules intercept applies
before the webhook's own manifests, ztunnel or istio-cni, or before the node
report and the cluster tokens.

## How AWS comes to trust a cluster's tokens

A pod on the cluster can call AWS, such as to pull an image from a private
registry, with a short-lived service-account token in place of a stored AWS
key. Each token names the cluster's issuer, a URL, and carries a signature
from the cluster's signing key. AWS accepts the token once the account holds
an IAM OIDC provider for that issuer, and once AWS can fetch two files from
the issuer's URL over public HTTPS. The discovery document describes the
issuer, and the key set holds the public keys that check a token's signature.
The [IAM OIDC provider page](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_providers_create_oidc.html)
requires an issuer that begins with `https://`, allows a path, and allows no
query.

The API server serves both files itself, at
`/.well-known/openid-configuration` and `/openid/v1/jwks`, but only on the
cluster's private network. The block therefore passes a public issuer as
`Cluster.ServiceAccountIssuer`, such as an S3 bucket's URL under the
subscription id. `adapt` in `nodeconfig/documents.go` writes it as the
`issuerURL` of Talos's `KubeServiceAccountConfig`, and Talos passes that to
the API server as `--service-account-issuer`. The node report,
`report/scripts/report.sh`, reads the API server's key set and posts it as
its `jwks` member. The platform then publishes the discovery document and that
key set at the issuer, such as in that bucket.

The report's read works only because Kubernetes serves its discovery
endpoints for an issuer that complies with
[OpenID Connect Discovery](https://openid.net/specs/openid-connect-discovery-1_0.html),
a "URL using the https scheme with no query or fragment components".
Kubernetes's
[service account issuer discovery](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#service-account-issuer-discovery)
page says so, and `checkIssuer` in `nodeconfig/nodeconfig.go` therefore
refuses any other issuer. The report reads as the service account it already
runs as, because Kubernetes binds the `system:service-account-issuer-discovery`
role to every service account by default.

The platform keeps the first key set a cluster reports, and takes a different
one only with a proof, a token signed by a key in the set it holds. A
cluster's signing key comes from the `Secrets` its configuration was rendered
with. While its block keeps those secrets, the key set stays the same, so the
report carries no proof.

## How a cluster isolates each app

Every cluster runs Kyverno, an admission controller that checks each object
the API server is asked to create. Four of its policies apply to every
namespace outside the system namespaces, which are `kube-system`,
`kube-public`, `kube-node-lease`, `argocd`, `kyverno`, `cnpg-system`,
`mitstack-node` and `istio-system`.

| Policy | What it does | File |
|---|---|---|
| `mitstack-require-app-label` | Refuses a pod without a non-empty `mitstack.dev/app` label, which an app's network policies select its pods by | `components/admission/policies/mitstack-require-app-label.yaml` |
| `mitstack-generate-default-deny` | Gives each namespace, new or existing, the NetworkPolicy `mitstack-default-deny`, which blocks every connection its pods make or take until an app's own policy allows it | `components/admission/policies/mitstack-generate-default-deny.yaml` |
| `mitstack-require-mesh` | Refuses a pod on the host network and a pod annotated `sidecar.istio.io/status`, the two kinds of pod the mesh leaves out. [The mesh section](#how-every-call-between-pods-proves-who-makes-it) explains the mesh | `components/admission/policies/mitstack-require-mesh.yaml` |
| `mitstack-require-strict-mtls` | Refuses a PeerAuthentication, Istio's policy for which calls a pod takes, that lets a pod or one of its ports take a call with no workload identity. It admits by name only the four exceptions of the hosting set, the extra components a cluster that hosts the platform runs. The CloudNativePG operator's exception sits in `cnpg-system`, which the policy skips | `components/admission/policies/mitstack-require-strict-mtls.yaml` |

Kyverno's webhooks skip the same system namespaces, so a Kyverno outage
cannot stop a system pod, such as the node report in `mitstack-node`.

The CloudNativePG operator in `cnpg-system` runs the Postgres clusters an app
keeps its data in. A namespace labelled `mitstack.dev/database-slice=true` is
a database slice. The Cilium policy `mitstack-database-slices` lets that
slice's Postgres instances reach the API server and each other, and admits
the operator on port 8000 and every app slice on port 5432. A pod in any other
namespace gains none of those allows. Each of those flows also admits port
15008, where a call arrives inside the mesh, as
[the next section](#how-every-call-between-pods-proves-who-makes-it) explains.

The CloudNativePG manifests are a byte-for-byte copy of release v0.4.0 of
mitstack-eu/cluster-bases, the repository of pinned third-party manifests.
Argo CD's built-in health check reads a Postgres switchover, failover,
upgrade or configuration change as Degraded. The kit's `argocd-cm` reads each
of those as Progressing, since CloudNativePG passes through them on its own.

## How every call between pods proves who makes it

A workload identity is a short-lived certificate that names the service
account a pod runs as. ADR 0095 requires that every call between services
carries mutual TLS with one, so both ends prove who they are, and that a call
without one is refused. A network policy decides which pod may reach which by
its address and labels, so it cannot tell a real caller from a pod that took
over that address. A certificate can. The ADR asked for Cilium's own mutual
authentication if it was ready, and a mesh otherwise. Cilium v1.19 documents
that feature as an incomplete beta that authenticates once per pair of
identities, so this module runs Istio 1.30.7 in ambient mode.

Ambient mode adds no container to your pods. Each node runs ztunnel, a proxy
that holds a certificate for every pod on the node and wraps each TCP call
between two pods in mutual TLS, sent to port 15008 of the pod it calls. The
certificate names the pod's service account as a SPIFFE identity, the
standard URI form for naming a workload in a certificate, such as
`spiffe://cluster.local/ns/app-shop/sa/default`. istiod, Istio's control
plane, issues it from a root it creates inside the cluster. Each certificate
lasts 24 hours, and ztunnel asks for a new one when half of that has passed.

Istio ships as four charts, `base`, `istiod`, `cni` and `ztunnel`. The `cni`
chart runs the CNI agent, a pod on every node that hooks each new pod's
network into the mesh. ztunnel and the CNI agent change each pod's network,
which only Kubernetes' most permissive Pod Security level, privileged, allows.

| Part | What it does | File |
|---|---|---|
| Istio's four charts | Run istiod, the CNI agent and ztunnel in `istio-system`, a namespace at the privileged Pod Security level | `components/istio/` |
| The CNI agent's settings | Put every pod in the mesh, whatever its labels, except the pods of `kube-system`, `istio-system` and `kyverno` and a pod on the host network | `components/render/istio-cni-values.yaml` |
| `default` in `istio-system` | The mesh-wide PeerAuthentication in `STRICT` mode, so a pod refuses any call that presents no workload certificate | `components/mesh/strict-mtls.yaml` |
| `mitstack-ambient-probes` | A Cilium policy that admits the kubelet's health probes into every pod. The CNI agent rewrites their source to `169.254.7.127`, so ztunnel can let them past, and Cilium would otherwise drop that address in a default-deny namespace | `components/mesh/probes.yaml` |
| `mitstack-database-instance-ports` | Keeps every caller but the CloudNativePG operator off port 8000 of a Postgres instance, where its instance manager, the CloudNativePG process inside each Postgres pod, answers the operator, and everyone off its ports other than 5432 and 8000 | `components/mesh/cloudnative-pg.yaml` |
| Argo CD's mesh policies | Admit port 15008 between the same Argo CD pods the chart's own network policies admit | `components/mesh/argocd.yaml` |

istiod injects no sidecar, because `components/render/istiod-values.yaml`
leaves out the injector's templates. A pod with a sidecar would sit outside
ambient mode, and `mitstack-require-mesh` refuses a pod that claims one.

Cilium still decides which pods may talk, but Cilium sees a call inside the
mesh only as port 15008. Every Cilium policy that admits a call between two
pods on a port it lists therefore admits 15008 from the same peers. Inside
the mesh, Cilium can no longer tell those listed ports apart. Where that
matters, an AuthorizationPolicy, Istio's rule for which caller may reach which
port, which ztunnel enforces, limits the ports again. One is
`mitstack-database-instance-ports` for a Postgres instance.

Two kinds of caller hold no workload identity. The API server calls each
admission webhook, and a web client outside the cluster calls Traefik.
A PeerAuthentication with a port-level exception lets that port alone take a
call without an identity, and the Cilium policies still decide who reaches it.

| PeerAuthentication | Port that takes a call without an identity | Who calls it |
|---|---|---|
| `cnpg-system/mitstack-cloudnative-pg-webhook` | 9443 on the CloudNativePG operator | The API server |
| `cert-manager/mitstack-cert-manager-webhook` | 10250 on cert-manager's webhook, in the hosting set | The API server |
| `cert-manager/mitstack-trust-manager-webhook` | 6443 on trust-manager, in the hosting set | The API server |
| `external-secrets/mitstack-external-secrets-webhook` | 10250 on the External Secrets webhook, in the hosting set | The API server |
| `traefik/mitstack-traefik-web` | 8000 and 8443 on Traefik, in the hosting set, the container ports behind its NodePorts 30080 and 30081 | Web clients, through the load balancers |

Cilium's Istio page asks for two settings when Cilium replaces kube-proxy.
`cni.exclusive: false` keeps the CNI configuration Istio chains after
Cilium's, and `socketLB.hostNamespaceOnly: true` leaves a pod's service calls
for ztunnel to see. `components/render/cilium-values.yaml` sets both.

You can read the certificate a pod holds from the ztunnel on its node, whose
admin port lists each certificate with its serial number and expiry. This
reads them for a pod `shop-0` in namespace `app-shop`:

```bash
node=$(kubectl -n app-shop get pod shop-0 -o jsonpath='{.spec.nodeName}')
ztunnel=$(kubectl -n istio-system get pod -l app=ztunnel --field-selector "spec.nodeName=$node" -o name)
kubectl -n istio-system port-forward "$ztunnel" 15000:15000 & forward=$!
sleep 2
curl -s localhost:15000/config_dump | jq '.certificates'
kill "$forward"
```

## Where a claim keeps its data

Every cluster has a default StorageClass, `local-path`, so a claim that names
no class still binds. Rancher's local path provisioner v0.0.37 serves it from
namespace `local-path-storage`. Each volume is a directory on a node's own
disk, so the provisioner never calls Hetzner and holds no Hetzner token.

The class waits for its first consumer, so a claim binds once a pod that
mounts it is scheduled. The provisioner then starts a helper pod on that node,
which creates the claim's directory under `/var/mnt/local-path-provisioner`,
the Talos user volume `local-path-provisioner`. The claim gets a `local`
volume pinned to that node. A pod cannot mount a `local` volume whose directory
is gone, so a node that lost its disk never hands a claim an empty directory.
The provisioner's own default is `hostPath`, which it recreates empty, and its
README calls `local` volumes "an experimental feature". Kubernetes documents
`local` volumes with no such mark. This claim, for example, gets a directory on
whichever node first runs a pod that mounts it:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: shop
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
```

The data outlives its pod, and every later pod that mounts the claim runs on
the same node. The data is lost with the node's machine, as Talos documents
for all local storage. A workload that must survive a lost node keeps copies
on other nodes itself, as CloudNativePG does with three instances. Deleting
the claim deletes its directory, because the class's reclaim policy is
`Delete` and the provisioner's teardown script removes it.

A claim's size caps nothing. The provisioner ignores a claim's capacity, and
Talos documents that a `directory` volume suits no workload that needs an
enforced quota. One claim can therefore fill `EPHEMERAL`, which etcd, the
container images and the logs share. The chart sets `allowVolumeExpansion:
true` on the class, but no size exists on the node for a resize to change.

The admission policies treat the provisioner as an app. Its pods and the
helper pods it starts carry `mitstack.dev/app: local-path-provisioner`, and
Kyverno gives its namespace `mitstack-default-deny`. The Cilium policy
`mitstack-local-path-flows` lets the namespace's pods reach the API server and
nothing else. A helper pod carries the provisioner's labels and service
account, so the policy cannot tell the two apart. The namespace enforces the
privileged Pod Security Standard, because a helper pod mounts the user volume
through `hostPath`, which Talos's default level, baseline, refuses.

That service account's token reaches every node's host. Its Role lets it
create pods in `local-path-storage`, where the privileged level admits a pod
with any host access, and its ClusterRole reads pods, their logs and
ConfigMaps in every namespace. It reads no Secret. The provisioner itself runs
as user 65534 with a read-only root, no privilege escalation and no
capabilities.

| File | What it holds |
|---|---|
| `components/render/local-path-provisioner-values.yaml` | The values the chart renders with: both images by digest, the default class, its binding mode and `local` volumes, the node path and the provisioner's security context |
| `components/local-path-provisioner/local-path-provisioner.yaml` | The rendered chart: the provisioner, its configuration with the helper pod's template, and the class |
| `components/local-path-provisioner/namespace.yaml` | The namespace and its Pod Security level |
| `components/local-path-provisioner/flows.yaml` | The policy `mitstack-local-path-flows` |
| `components/components.go` | `Default`, which serves the chart, carries the namespace and the policy inline, and names the user volume `LocalPathVolume` |

| Page | What the provisioner's setup takes from it |
|---|---|
| [Talos: Local Storage](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage) | The provisioner keeps its volumes on a user volume at `/var/mnt/local-path-provisioner`, its class may be the default, and its namespace is labelled privileged |
| [Talos: User Volumes](https://docs.siderolabs.com/talos/v1.14/configure-your-talos-cluster/storage-and-disk-management/disk-management/user) | A `directory` volume is a bind mount on `EPHEMERAL` with no enforceable quota, and Talos propagates every user volume's mount into the kubelet |
| [Local Path Provisioner](https://github.com/rancher/local-path-provisioner/blob/v0.0.37/README.md) | `nodePathMap` names each node's directory, the provisioner ignores a claim's capacity, and a class's `defaultVolumeType` picks `local` or `hostPath` |
| [Kubernetes: storage classes](https://kubernetes.io/docs/concepts/storage/storage-classes/) | A claim with no class gets the class annotated `storageclass.kubernetes.io/is-default-class`, and `WaitForFirstConsumer` delays binding until a pod mounts the claim |
| [Kubernetes: persistent volume types](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#types-of-persistent-volumes) | A `hostPath` volume is "for single node testing only", and the page says to "consider using `local` volume instead" |
| [Kubernetes: Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/) | The baseline level forbids `hostPath` volumes |

## How a cluster that hosts the platform boots

A cluster that runs mitstack's own control plane must start its secret store,
External Secrets and its web entry point before Argo CD or the platform runs.
`components.Hosting` returns that boot set. It is the default set with six
more charts and the manifests that join them. A cluster built from `components.Default` carries
none of the hosting set's manifests, and the golden files in
`nodeconfig/testdata/golden/` and `gateway/testdata/golden/` record its render.

A block opts in through `controlplane.Plane`, which mints the hosting keys
and renders the set itself. The block sets `Plane.Hosting` to the cluster
repository, the recipient key and a `DeployKeys` hook:

```go
plane.Hosting = &controlplane.Hosting{
	Repository: "acme/shop-cluster", // the cluster repository, owner/name
	Recipient:  recipient,           // the armored public key behind openbaoSealKeyRecipient
	DeployKeys: func(ctx context.Context, keys controlplane.HostingKeys) (controlplane.SealedDeployKeys, error) {
		// Create the write and read-only deploy keys on the repository, then seal each private half.
		write, err := components.Seal(keys.SealedSecretsCertPEM, components.DeployKeyNamespace,
			components.DeployKeySecret, components.DeployKeyItem, writeKey)
		if err != nil {
			return controlplane.SealedDeployKeys{}, err
		}
		read, err := components.Seal(keys.SealedSecretsCertPEM, components.RepositoryCredentialNamespace,
			components.RepositoryCredentialName, components.RepositoryCredentialItem, readKey)
		return controlplane.SealedDeployKeys{Write: write, Read: read}, err
	},
}
```

The install calls `DeployKeys` once, in the change that creates the gateway
and the builders, with the 32-byte OpenBao seal key and a 4096-bit Sealed
Secrets key pair it has just minted. Those keys exist only in that call's
memory and in what the hook keeps of them. An error from the hook stops the
install before any server exists. An install that fails after the hook leaves
its deploy keys on the repository, with their private halves gone, so the
block's hook deletes keys it created earlier before it creates new ones.

`components.Hosting` checks what the hook returns. It refuses a seal key that
is not 32 bytes, a certificate of another key, and a deploy key not sealed to
that key pair. It refuses a sealed file that holds more than its one
SealedSecret and its one value, and anything but three distinct node names. It
also refuses init inputs that `openbaoinit.Manifests` refuses: a tools image
without a digest, a repository that is not `owner/name`, and a recipient that
is not one armored public key able to encrypt.

### What the hosting set runs

Each chart renders from a version pinned with the sha256 its repository's
`index.yaml` publishes. Every image is named by digest, and `mirror.List`
returns each one for the gateway's mirror.

| Component | What it does here | Values |
|---|---|---|
| OpenBao 2.7.1 | Three Raft members, one per control-plane node, each tolerating the control-plane taint. The listener serves TLS 1.3 only, with the certificate cert-manager issues, and reloads it when it is renewed. The static seal reads its key from `/openbao/seal/key` | `components/render/openbao-values.yaml` |
| cert-manager v1.21.2 | Issues OpenBao's serving certificate from OpenBao's own authority, and the External Secrets webhook's certificate | `components/render/cert-manager-values.yaml` |
| trust-manager v0.25.0 | Copies that authority into the ConfigMap `openbao-ca`, under `ca.crt`, in every namespace | `components/render/trust-manager-values.yaml` |
| External Secrets v2.11.0 | Copies values from OpenBao into Kubernetes Secrets | `components/render/external-secrets-values.yaml` |
| Sealed Secrets 0.40.0 | Opens a `SealedSecret` with the block's key pair, and never mints or renews a key | `components/render/sealed-secrets-values.yaml` |
| Traefik v3.7.13 | Serves web traffic on NodePorts 30080 and 30081, with one pod on every node that takes apps | `components/render/traefik-values.yaml` |

The authority and the certificates sit in `components/hosting/tls.yaml`. A
`SecretStore`, the External Secrets object that names a store, verifies
OpenBao through its Vault provider's `caProvider`, as in this example:

```yaml
caProvider:
  type: ConfigMap
  name: openbao-ca
  key: ca.crt
```

### Where each secret lives

The gateway serves every served manifest to every node over the private
network. A served manifest therefore holds no Secret and no private key, and
each secret the block passes in sits in an inline manifest. An inline manifest
travels inside the machine configuration, which already holds every key of
the cluster.

| Inline manifest | What it holds | Template |
|---|---|---|
| `mitstack-openbao-seal-key` | The Secret `openbao-seal-key` with the 32-byte seal key | `components/hosting/seal-key.yaml` |
| `mitstack-sealed-secrets-key` | The key pair, labelled as Sealed Secrets' active key | `components/hosting/sealed-secrets-key.yaml` |
| `mitstack-openbao-deploy-key` | The `SealedSecret` that opens to the cluster repository's write deploy key, for OpenBao's init Job | `components/hosting/deploy-key.yaml` |
| `mitstack-cluster-repository` | The `SealedSecret` that opens to Argo CD's credential for the cluster repository, holding its read-only deploy key | `components/hosting/cluster-repository.yaml` |
| `mitstack-openbao-storage` | OpenBao's StorageClass and the local volume of each member | `components/hosting/storage.yaml` |
| `mitstack-openbao-init` | OpenBao's init Job, and the ConfigMap with the recipient's public key. It holds no secret, and sits inline because it names this cluster's repository | `openbaoinit/openbao-init.yaml` |

Talos creates each object of a manifest once and never edits it afterwards. A
new seal key or key pair therefore reaches a running cluster only through
OpenBao's and Sealed Secrets' own rotation, and never through a new render.

### How the hosting set reaches the builder

The builder's user data carries the cluster's machine configuration, and
Hetzner accepts at most 32,768 bytes of user data. The hosting set's inline
manifests do not fit beside the rest. A hosting install therefore creates a
third short-lived server, the serving server, which hands them to the builder
over the private network.

The serving server joins the network at `10.0.0.251` with no public address.
Its user data holds the hosting set's inline manifests as configuration
documents, a TLS key minted for this install, and the program in
`serve/scripts/serve.py`. The program serves that one file once, to the
builder's address `10.0.0.250` alone, over TLS 1.3. It deletes its key once
loaded, and the file after one complete fetch.

The builder's user data gains the file's URL and the sha256 of the serving
key's public half. `controlplane/scripts/build-image.sh` fetches the file with
curl's `--pinnedpubkey`, so it trusts that one key and no authority. It
appends the file to the configuration in its memory-backed scratch directory
before imager runs. A fetch that fails removes the configuration and fails the
build before imager starts.

The plane deletes the serving server once the builder is gone, once the
snapshot exists, or once the build is overdue. A build that fails therefore
takes the serving server with it. The tests in `controlplane/hosting_test.go`
log the builder's user data size, about 30,200 bytes with a 4096-bit Sealed
Secrets key, against Hetzner's limit of 32,768.

The `serve` package holds the server's key, its files and its command, and a
block reuses it to serve any one file to one client.

### How Argo CD reads the cluster repository

This section is for an engineer who builds a cluster block that hosts
mitstack's platform, and for anyone checking which key such a cluster reads
its code with. It uses seven terms.

| Term | Meaning here |
|---|---|
| Platform-hosting cluster | A cluster that runs mitstack's own control plane |
| Hosting set | The manifests `components.Hosting` returns for a platform-hosting cluster, which the install renders from the keys it mints |
| Inline manifest | A manifest of the set that travels inside each node's machine configuration instead of being served over the network, which is where every secret goes |
| Cluster block | A block, a unit of infrastructure an app subscribes to, whose Go program imports this module and installs a Talos cluster, such as hcloud-hrobot-talos-cluster |
| Cluster repository | The GitHub repository the block creates for each cluster, whose manifests Argo CD applies to the cluster |
| Deploy key | An SSH key GitHub grants access to one repository alone, read-only or with write access, and that the repository's admin can revoke on its own |
| Sealed Secret | An object holding values encrypted to the cluster's RSA key pair, which the Sealed Secrets controller decrypts into an ordinary Secret |

Argo CD is the controller that applies a cluster's Git repositories to the
cluster, and it needs a credential to read them. A platform-hosting cluster
reads its cluster repository with a read-only deploy key, over GitHub's SSH
endpoint on port 443. The cluster's gateway forwards only port 443 and the
log receiver's port, so port 22 is closed to it. The URL has this form:

```text
ssh://git@ssh.github.com:443/acme/shop-cluster.git
```

`components.RepositorySSHURL` returns it for an `owner/name`. Argo CD checks
GitHub's host key against the `[ssh.github.com]:443` entries of the
`argocd-ssh-known-hosts-cm` ConfigMap in `components/argocd/argocd.yaml`.
OpenBao's init Job checks the same entries.

The cluster block creates two deploy keys on the cluster repository during the
install. The write key lets OpenBao's init Job commit `openbao-init.asc`. The
read-only key is Argo CD's. The block seals each private half in
`DeployKeys` with `components.Seal`, which encrypts it to the minted
certificate in Sealed Secrets' strict scope. Strict scope ties the encrypted
value to one name and namespace, so it opens nowhere else. The block keeps no
copy of either private half.

| Deploy key | Name and namespace it opens as | Item |
|---|---|---|
| Write | `github-deploy-key` in `mitstack-node` | `ssh-privatekey` |
| Read-only | `mitstack-cluster-repository` in `argocd` | `sshPrivateKey` |

`components.Hosting` copies nothing from the read key's SealedSecret but the
encrypted value. It writes a new SealedSecret around the value, labelled as an
Argo CD repository, with the repository's `type` and `url` unencrypted. Sealed
Secrets opens it into this Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: mitstack-cluster-repository
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  type: git
  url: ssh://git@ssh.github.com:443/acme/shop-cluster.git
  sshPrivateKey: <the read-only deploy key>
```

A block that gives Argo CD another repository on a hosting cluster, such as an
app slice's own repository, builds the same kind of credential from three
functions in `components/seal.go`:

| Function | What it returns |
|---|---|
| `components.NewDeployKey(comment)` | A fresh Ed25519 key, its public half in the `ssh-ed25519 AAAA...` form GitHub's `POST /repos/{owner}/{repo}/keys` takes and its private half as OpenSSH PEM |
| `components.SealValue(cert, namespace, name, value)` | The sealed value alone, for a block that writes its own SealedSecret around it, with its own `template` |
| `components.Open(key, namespace, name, sealed)` | The value a SealedSecret opens to, as the controller holding the key opens it, so a test can check what a block sealed |

`components.Seal` wraps `SealValue` in a SealedSecret manifest that holds the
value under one item.

On a hosting cluster the enrolment Job in `controlplane/manifests/cluster-tokens.yaml`
takes only the cluster's report token from the platform. It writes no
repository credential, and the cluster runs no CronJob that renews one. The
environment value `REPOSITORY_CREDENTIAL=deploy-key` tells
`controlplane/scripts/cluster-tokens.sh` so. A cluster that hosts no platform
reads its repository with the short-lived token the platform issues, as
`REPOSITORY_CREDENTIAL=platform-token`.

### Where OpenBao keeps its data

Each node keeps its member's Raft data on the Talos user volume `openbao`,
which Talos mounts at `/var/mnt/openbao`. A Cloud server has one disk, and the
partition Talos calls `EPHEMERAL` spans all of it. The volume is therefore a
directory on `EPHEMERAL`, which needs no free space on the disk.

The StorageClass `openbao-local` provisions nothing. Each member's claim binds
a local volume pinned to one named node, so a member keeps its node and its
data. The class's `Retain` policy keeps that data if the claim goes, where the
default class `local-path` deletes it. Talos documents that a node's local
data is lost with its machine, which is why OpenBao runs three members and
Raft carries the data.

### Which flows the hosting namespaces allow

Kyverno gives each hosting namespace `mitstack-default-deny` and refuses a pod
without `mitstack.dev/app`, as it does every app namespace. Every pod of the
hosting set carries that label. The Cilium policy `mitstack-hosting-flows` in
`components/hosting/flows.yaml` names each flow the set needs.

| Pods | What the policy allows |
|---|---|
| cert-manager and trust-manager | Calls to the API server, and the API server's calls to their webhooks |
| External Secrets | Calls to the API server, to cluster DNS and to OpenBao on 8200, and the API server's calls to its webhook |
| OpenBao | Calls between members on 8200 and 8201, to the API server and to cluster DNS, and the calls of External Secrets and the init Job on 8200 |
| OpenBao's init Job | Lookups through Cilium's DNS proxy, calls to OpenBao on 8200, and SSH to `ssh.github.com` on 443, which the gateway forwards |
| Traefik | Web traffic from outside the cluster on its two ports, calls to the API server, and calls to the app slices |

The API server runs on each control-plane node's host network. When it calls a
webhook pod on another node, Cilium gives the call that node's identity rather
than `kube-apiserver`. A webhook port therefore takes a second rule, which
admits the nodes labelled `node-role.kubernetes.io/control-plane`.
Cilium gives a node an identity by that label because
`components/render/cilium-values.yaml` sets `nodeSelectorLabels` and
`nodeLabels`. A worker's host and every other node stay off the webhook ports,
so the Robot worker that runs apps cannot call a webhook.

Each flow between two pods on a named port also admits port 15008, where the
call arrives inside the mesh. The AuthorizationPolicy `mitstack-openbao-ports`
in `components/hosting/mesh.yaml` keeps the ports themselves: OpenBao takes
calls on 8200, and on 8201 from its own members alone. The same file holds the
hosting set's four port-level exceptions that
[the mesh section](#how-every-call-between-pods-proves-who-makes-it) lists.

### How OpenBao is initialised

OpenBao starts uninitialised. Its first init returns the root token, which
holds every right in the store, and a recovery key, which OpenBao asks for
before it regenerates a root token. Both must reach the owner, the person who
runs mitstack and keeps its keys, without anyone typing them or opening a route
into the cluster.

The init Job sends them through Git. It commits them to the cluster
repository, the GitHub repository the block creates for each cluster and Argo
CD applies, as `openbao-init.asc` at its top. Each is a PGP message to the
recipient key, the owner's OpenPGP public key, which the block's parameter
`openbaoSealKeyRecipient` names and the block passes as `Init.Recipient`. It is
a different key from OpenBao's seal key. The owner keeps its private half, so
only the owner can read the file.

The Job runs `openbaoinit/scripts/openbao-init.sh` in the tools image, in
`mitstack-node`, because that is where the `SealedSecret` `github-deploy-key`
opens. The pod mounts the Secret that `SealedSecret` opens to, holding the
cluster repository's write deploy key, and Kubernetes starts none of its
containers until that Secret exists. The script runs Git over SSH with that key
alone, at the URL `components.RepositorySSHURL` returns. ssh accepts only a
GitHub host key from the `[ssh.github.com]:443` entries the Job's ConfigMap
carries, which are the ones Argo CD trusts.

The script talks only to `openbao-0`, the member OpenBao's Helm guide says to
initialise. The other two members join it through Raft, OpenBao's replicated
storage, and a member that has not joined yet still reads as uninitialised.
Talking to one member keeps the script from initialising a second store. Each
request verifies OpenBao against the cluster's own authority. The script first
encrypts a test message to the recipient key, and stops before it sends
anything when gpg cannot encrypt to it. It then reads the store's state, and
looks for `openbao-init.asc` in a fresh clone of the repository's default
branch.

| OpenBao | `openbao-init.asc` | What the Job does |
|---|---|---|
| Uninitialised | Absent | Initialises, commits the file, then sets OpenBao up |
| Initialised | Committed | Changes nothing and exits 0 |
| Initialised | Absent | Exits 1 and says the root token and recovery key are lost |
| Uninitialised | Committed | Exits 1 before the init, since the file may open another store |

The init asks for the recovery key whole, as one share with a threshold of
one, and passes the recipient's binary key as `recovery_pgp_keys`. OpenBao then
returns the recovery key already encrypted, as a binary PGP message. The script
turns that into ASCII armor, the text form between the `BEGIN` and `END` lines
below, and leaves out the CRC24 checksum line that RFC 9580 says not to add.
It encrypts the root token to the same recipient with gpg. It commits both and
pushes the commit with the write key, before any request carries the root
token. A push another writer beat is fetched afresh and tried again. The file
reads:

```text
OpenBao root token:
-----BEGIN PGP MESSAGE-----
...
-----END PGP MESSAGE-----
OpenBao recovery key:
-----BEGIN PGP MESSAGE-----
...
-----END PGP MESSAGE-----
```

No answer OpenBao gives to the init can stop the script before the commit,
since the commit is the only copy of the root token. A recovery key in the
answer that is missing, or is not one base64 PGP message, is replaced by a line
saying it is lost. The script then commits the root token alone, finishes the
setup, and exits 1 naming the lost recovery key. A pod killed between the init
and the commit still loses both keys, and the next pod stops on finding the
store initialised with no file.

With the root token, the script enables the KV v2 mount `kv` and Kubernetes
auth, and points that auth method at the API server. It then reads the method's
accessor from `GET /v1/sys/auth`, because the accessor exists only once the
method is enabled. It writes the read-only policy `app-secrets-reader` with
that accessor in place. In this example `auth_kubernetes_f1e2d3c4` stands for
the accessor OpenBao generates:

```hcl
path "kv/data/apps/{{identity.entity.aliases.auth_kubernetes_f1e2d3c4.metadata.service_account_namespace}}/*" {
  capabilities = ["read"]
}
path "kv/metadata/apps/{{identity.entity.aliases.auth_kubernetes_f1e2d3c4.metadata.service_account_namespace}}/*" {
  capabilities = ["read", "list"]
}
```

A token that logged in with a service account in namespace `shop` can then
read only `kv/data/apps/shop/*`. The Job writes no Kubernetes auth role, so no
service account can log in with this policy until a role names it.

The root token lives in the script's memory alone. The script hands curl each
header that carries a token on curl's standard input, through the shell's
builtin `printf`, and never prints a token. The script retries each step while
its pod lives. A pod lost after the commit may leave the mount, the auth method,
its configuration or the policy unwritten, and a new pod finds the file and
changes nothing. The owner then decrypts the root token from
`openbao-init.asc` and writes whichever of the four is missing.

### The documentation the hosting set follows

| Page | What the set takes from it |
|---|---|
| [Talos: inlineManifests and extraManifests](https://docs.siderolabs.com/kubernetes-guides/advanced-guides/inlinemanifests) | A served manifest is fetched from its URL, an inline one lives in the configuration, and Talos only creates objects |
| [Talos: User Volumes](https://docs.siderolabs.com/talos/v1.14/configure-your-talos-cluster/storage-and-disk-management/disk-management/user) | A user volume mounts at `/var/mnt/<name>`, and a `directory` volume lives on `EPHEMERAL` |
| [Talos: disk layout](https://docs.siderolabs.com/talos/v1.14/configure-your-talos-cluster/storage-and-disk-management/disk-management/layout) | `EPHEMERAL` takes all unallocated space by default |
| [Talos: Local Storage](https://docs.siderolabs.com/kubernetes-guides/csi/local-storage) | Local storage is lost when its machine fails |
| [OpenBao: TCP listener](https://openbao.org/docs/configuration/listener/tcp/) | TLS is on unless `tls_disable` is set, and `tls_auto_reload` picks up a renewed certificate |
| [OpenBao: Integrated Storage (Raft)](https://openbao.org/docs/configuration/storage/raft/) | `path`, `retry_join` with `leader_ca_cert_file`, and the node ID from `BAO_RAFT_NODE_ID` |
| [OpenBao: static seal](https://openbao.org/docs/configuration/seal/static/) | The key is 32 bytes, read through `file://` |
| [OpenBao: `/sys/init`](https://openbao.org/api-docs/system/init/) | `recovery_shares`, `recovery_threshold` and `recovery_pgp_keys`, each key the base64 of its binary form, apply to an auto-unseal such as the static seal |
| [OpenBao: HTTP API](https://openbao.org/api-docs/) | OpenBao treats `PUT` and `POST` as synonyms |
| [OpenBao: HA with Raft on Kubernetes](https://openbao.org/docs/platform/k8s/helm/examples/ha-with-raft/) | You initialise `openbao-0`, and the other members join it |
| [OpenBao: `/sys/auth`](https://openbao.org/api-docs/system/auth/) and [`/sys/mounts`](https://openbao.org/api-docs/system/mounts/) | Each listing names a method by its path, with its `accessor`, and `options.version` set to `"2"` mounts KV v2 |
| [OpenBao: KV v2](https://openbao.org/docs/secrets/kv/kv-v2/) | A policy names a secret under `data/` and its listing under `metadata/` |
| [OpenBao: Kubernetes auth](https://openbao.org/docs/auth/kubernetes/) and [its API](https://openbao.org/api-docs/auth/kubernetes/) | `kubernetes_host` is required, a pod's own token and CA serve for the review, and a login's alias carries `service_account_namespace` |
| [OpenBao: templated policies](https://openbao.org/docs/concepts/policies/#templated-policies) and [`/sys/policies/acl`](https://openbao.org/api-docs/system/policies/) | A path may name `identity.entity.aliases.<mount accessor>.metadata.<key>` |
| [GitHub: managing deploy keys](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys#deploy-keys) | A deploy key reaches one repository, is read-only unless granted write access, and goes with that repository |
| [GitHub: using SSH over the HTTPS port](https://docs.github.com/en/authentication/troubleshooting-ssh/using-ssh-over-the-https-port) | `ssh.github.com` takes SSH on port 443, at `ssh://git@ssh.github.com:443/<owner>/<repo>.git` |
| [Argo CD: private repositories](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/) and [declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#repositories) | A Secret labelled `argocd.argoproj.io/secret-type: repository` carries `type`, `url` and `sshPrivateKey`, and SSH host keys come from `argocd-ssh-known-hosts-cm` |
| [OpenSSH: ssh_config](https://man.openbsd.org/ssh_config) | `StrictHostKeyChecking yes` refuses a host key missing from `UserKnownHostsFile`, and `IdentitiesOnly` offers the named key alone |
| [Kubernetes: Secrets](https://kubernetes.io/docs/concepts/configuration/secret/#restriction-secret-must-exist) | No container of a pod starts until every Secret it requires is available |
| [RFC 9580 section 6.1](https://www.rfc-editor.org/rfc/rfc9580#section-6.1) | ASCII armor should carry no CRC24 footer |
| [GnuPG: key-related options](https://www.gnupg.org/documentation/manuals/gnupg/GPG-Key-related-Options.html) and [commands](https://www.gnupg.org/documentation/manuals/gnupg/Operational-GPG-Commands.html) | `--recipient-file` encrypts to the one key in a file, and `--dearmor` unpacks an armored key |
| [Cilium: DNS-based policy](https://docs.cilium.io/en/v1.19/security/policy/layer3/#dns-based) | `toFQDNs` learns a name's addresses from DNS answers that pass Cilium's DNS proxy |
| [Cilium: mutual authentication](https://docs.cilium.io/en/v1.19/network/servicemesh/mutual-authentication/mutual-authentication/) and [ztunnel encryption](https://docs.cilium.io/en/v1.19/security/network/encryption-ztunnel/) | Both are beta, and mutual authentication caches one handshake per pair of identities |
| [Cilium: Istio integration](https://docs.cilium.io/en/v1.19/network/servicemesh/istio/) | With kube-proxy replaced, ambient mode needs `socketLB.hostNamespaceOnly: true` and `cni.exclusive: false` |
| [Istio: ambient platform prerequisites](https://istio.io/v1.30/docs/ambient/install/platform-prerequisites/) | Under a Cilium default-deny policy, the kubelet's probes need a policy admitting `169.254.7.127/32`, and BPF masquerading stays off |
| [Istio: ambient and network policy](https://istio.io/v1.30/docs/ambient/usage/networkpolicy/) | A call inside the mesh arrives on port 15008, so a policy that admits a port must admit 15008 too |
| [Istio: PeerAuthentication](https://istio.io/v1.30/docs/reference/config/security/peer_authentication/) | `STRICT` in the root namespace binds the whole mesh, and `portLevelMtls` needs a selector |
| [curl: `--header`](https://curl.se/docs/manpage.html#-H) | `-H @-` reads headers from standard input |
| [cert-manager: CA](https://cert-manager.io/docs/configuration/ca/) and [SelfSigned](https://cert-manager.io/docs/configuration/selfsigned/) issuers | A self-signed root bootstraps the CA issuer |
| [trust-manager](https://cert-manager.io/docs/trust/trust-manager/) | A `Bundle` reads Secrets in its trust namespace and writes a ConfigMap to every namespace |
| [External Secrets: HashiCorp Vault provider](https://external-secrets.io/latest/provider/hashicorp-vault/) | A private CA is named through `caProvider` or `caBundle` |
| [Sealed Secrets: bring your own certificates](https://github.com/bitnami/sealed-secrets/blob/main/docs/bring-your-own-certificates.md) | The controller uses each key pair in its namespace that carries the active label |
| [Sealed Secrets: scopes](https://github.com/bitnami/sealed-secrets#scopes) and [its crypto](https://github.com/bitnami/sealed-secrets/blob/main/docs/developer/crypto.md) | A strict-scope value is sealed with RSA-OAEP over SHA-256, labelled with its namespace and name, around an AES-GCM session key |
| [Traefik chart values](https://github.com/traefik/traefik-helm-chart/blob/master/traefik/VALUES.md) | `deployment.kind`, `ports.web.nodePort`, `ports.websecure.nodePort` and `service.spec` |

## Why the image build discards its output

The embedded configuration holds every key of the cluster, and imager prints
its whole profile into its build log, so that log is as secret as the image.
`image.Run` therefore sends imager's output nowhere and reports only its exit
status. You reproduce a failed build with a throwaway configuration.

## How etcd recovers

A lost control-plane node leaves its etcd member and its Node object behind.
`etcd.RemoveMemberJob` renders a Job that looks up the member by hostname,
removes it, and only then deletes the Node. Its Talos identity holds
`os:admin`, the only role Talos accepts for a member removal, and its
Kubernetes identity may delete that one Node.

A restore onto new servers runs the gateway's script with `--recover-from`. It
bootstraps etcd from the snapshot, waits for the API server, and deletes every
Node object that `--keep` does not name. The fourth proof's restore stalled
until the old Node objects went, because Argo CD's controller stayed bound to
a dead server's Node.

## How a release is versioned

A block takes a change only by requiring a newer tag of this module in its
`go.mod`, as `require github.com/mitstack-eu/talos-cluster-kit v0.1.0`. The
file `.github/version` names the next release. Once a pull request merges and
every CI job passes on `main`, the `release` job tags that commit `v` followed
by the version, unless the tag exists already. The `record delivery` job then
records the push's board keys, the `MIT-` issue keys its commits name, on
Plain, mitstack's board.

A release also publishes the tools image, which the member removal Job and the
node report run in. The `release` job pushes it to the public package
`ghcr.io/mitstack-eu/talos-cluster-kit-tools`, signs its digest keyless as
this repository's CI on `main`, and refuses to tag unless a client with no
login can pull it. An image's digest is known only once it is built, so it
cannot sit in the commit the image is built from. The job therefore tags a
commit on top of the merged one, changing only the line of `tools/tools.go`
that sets `tools.Image` to the pushed reference. On `main`, `tools.Image`
names no published image, with the tag `unreleased` and a digest of zeros. A
block reads it from the tag its `go.mod` requires, so a `go.mod` bump alone
moves its tools image.

The gateway fetches each served manifest from the release a block requires, at
the URL `gateway.ReleaseURL` gives, and refuses any file whose sha256 differs
from the one the kit renders.

## How you change the module

The tests compare each rendered configuration and the gateway's cloud-config
byte for byte with the recordings in `testdata/golden/`. An edit to what a node
boots fails them until you record the new bytes on purpose, with `-update`.
[The development page](docs/development.md) lists each script, its test and
what CI runs. Run the suites from the repository root:

```bash
go vet ./...
go test -race ./...
go test ./nodeconfig/ ./gateway/ -update
```
