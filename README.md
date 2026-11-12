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
DNS, NTP and HTTPS, and HTTP to the cluster's ingress load balancer alone. It
records that traffic, and serves the image mirror and the
larger manifests over TLS on the private network. The table names the parts in
the order a block uses them.

| Package | What it does | Main entry points |
|---|---|---|
| `components` | Holds the pinned component set: Cilium, the custom resource definition (CRD) of Cilium's cluster-wide policy, Argo CD, the policy that keeps pods from Hetzner's metadata service at `169.254.169.254`, Istio's ambient mesh with its mesh-wide mutual TLS policy and the mesh probe's target, Kyverno with its four admission policies, the CloudNativePG operator with the database-slice policy, the local path provisioner behind the default StorageClass, and the node-local OpenTelemetry Collector. It also holds the boot set of a cluster that hosts the platform | `Default`, `Hosting`, `HostingServed`, `Set.PublishingIngress`, `Manifest` (whose `Name` is a component's name), `Set.Images`, `LogCollector`, `ParseImage` |
| `nodeconfig` | Renders a control-plane node's or a worker node's machine configuration with Talos's own `machinery` module and validates it against Talos's schema | `Render`, `RenderWorker`, `Cluster`, `Node`, `NewSecrets`, `ParseSecrets`, `Secrets.AdminTalosconfig` |
| `image` | Runs imager, Sidero Labs' open-source image builder, with the `hcloud` or the `metal` profile, and discards its output | `Run`, `Build` |
| `mirror` | Lists every image the nodes pull, each by digest | `List`, `Render` |
| `gateway` | Renders the cloud-config of the gateway in each location, and mints the cluster's gateway CA and each gateway's certificate | `Render`, `Inputs`, `Deadline`, `NewCA`, `CA.Issue` |
| `etcd` | Renders the Job that removes a lost node's etcd member and then its Node object, and holds the gateway's script that bootstraps etcd empty or from a snapshot | `RemoveMemberJob`, `BootstrapScript`, `Bootstrap.Args` |
| `report` | Renders the node report, a CronJob that reads the cluster and posts what it read to the platform, signed for its exact body. [What the node report carries](#what-the-node-report-carries) lists its members, the nine checks it names and what happens when a read fails. It also renders the outside probe, a CronJob in `kube-system` that calls the mesh probe's target with no workload identity and reads each gateway's mirror from outside the mesh, the admission probe namespace and the daily security scan. The report holds no right on Secrets | `Manifests`, `Interval` |
| `topology` | Renders the topology labeller, a CronJob that labels each Cloud node with its zone and region, and holds the two label names | `Manifests`, `Labels`, `ZoneLabel`, `RegionLabel`, `FieldManager` |
| `openbaoinit` | Renders OpenBao's init Job, which the hosting set carries: it initialises a hosting cluster's OpenBao once, commits the root token and recovery key to the cluster repository as PGP messages, then writes the policy each slice's External Secrets reads with | `Manifests`, `Inputs`, `InitFile` |
| `rollout` | Gives a running cluster a release's component set, the manifests one kit release carries; [How a running cluster takes a release's components](#how-a-running-cluster-takes-a-releases-components) defines each term. A block commits the set's files to the cluster repository, where Argo CD applies each component through an Application of its own, which is Argo CD's record of one repository directory it keeps applied to the cluster. Files that an earlier release committed and this release no longer states are retired, which means deleted. Each component's sync ends with a handover Job, which takes Talos's fields off the objects Talos created, so they hold only what the release states. The read-back then checks from the node report that the cluster runs each component as the release states it | `Release.Files`, `Release.Retired`, `Release.ReadBack`, `Release.ToolsImage`, `LegacyFiles` |
| `controlplane` | Builds and deletes a Talos control plane on Hetzner Cloud: a network, a firewall, a gateway in each of `fsn1`, `nbg1` and `hel1`, a builder that makes the cluster's own snapshot, a node in each of `fsn1`, `nbg1` and `hel1`, and an API and an ingress load balancer. Each call reads the resources by their labels and takes the one change still missing. A refusal is a `*Refusal` with a `Class` and a `Message`, which the block answers as its own framework's refusal | `Plane`, `Plane.Read`, `Plane.EnsureNetwork`, `Plane.NextStep`, `Plane.Problems`, `Plane.Unhealthy`, `Plane.Outputs`, `State.IngressAddresses`, `Plane.Release`, `ErrNoIngressAddress`, `Plane.GatewaysAwaitRelease`, `Plane.GatewaysServeRelease`, `Plane.Delete`, `NewAPI`, `Refusal`, `TopologyLabels`, `GatewayUpgradeBound`, `GatewayCertRenewBefore`, `KeyGatewayCertExpires` |
| `tools` | Holds `tools/tools.env`, which pins the talosctl and kubectl that the tools image carries and the gateway copies out of it, `tools/jq.env`, which pins the tools image's jq, and `tools/kube-bench-talos.yaml`, the settings kube-bench reads over its own on Talos. It names the tools image a release published and the directory in that image that holds the node report's, its outside probe's, the labeller's, the cluster tokens' and the handover's scripts, and where in that image each served manifest sits | `Env`, `Pin`, `Image`, `ScriptDir`, `ManifestPath` |
| `redundant` | Carries a removal's redundant set, each member with its reading, to the guard the block passes, block-kit's `redundancy.Set.AllowRemoval`, which this module does not import | `Set`, `Guard`, `Held` |
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
Kubernetes itself, and a gateway in each location, a Cloud server that gives
the nodes their only route out and serves them their images and manifests. The plane builds
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
	API:               controlplane.NewAPI(token, "hcloud-hrobot-talos-cluster"), // token: the app's Hetzner Cloud API token
	Block:             "hcloud-hrobot-talos-cluster",
	Subscription:      "sub_0123456789ab",
	AppName:           "shop",
	ServerTypes:       []string{"cx23", "cpx22"}, // each node and builder takes the first its location can order
	Boundary:          "mitstack-hcloud-hrobot-talos-cluster", // separates the parts of the gateway's user data
	PlatformURL:       "https://api.mitstack.dev",             // where every node registers and reports
	ReportProofScript: reportproof.TokenRequestScript,         // block-kit's script that signs each node report
	RepoURL:           "https://github.com/acme/shop-cluster", // the Git repository Argo CD applies to the cluster
	Installed:         install,                                // the block's own func(ctx, controlplane.Network, nodeconfig.Cluster, gateway.CA) error
	GatewayCA:         kept,                                   // the CA the block keeps, nil before the install
	Upgrading:         operation == "upgrade",                 // starts replacing gateways built from an earlier list
	ReadGateway:       readGateway,                            // reads a gateway from the newest node report since a time
	ReadReport:        readReport,                             // returns the newest node report since a time, for removals and kept images
	Guard:             guard,                                  // block-kit's redundancy guard, judging every removal
	Notify:            emitEvent,                              // receives each certificate, route move, power-on and deletion
	AppsOnWorkers:     true,
	WorkerAddresses:   []string{"10.0.1.2"}, // the Robot worker's address in the network's vSwitch subnet
}
st, err := plane.Read(ctx)
```

`ServerTypes` lists the server types of the control-plane nodes and the
builders by preference, so one sold-out type does not stop a node. Each server
takes the first type Hetzner can order in its location at the moment it is
created, through `controlplane.OrderableTypes`, as each gateway does from
`GatewayServerTypes`. A running server keeps its type. When Hetzner can order
none of the list in a location, the call refuses with the class
`server-unavailable`, naming the list and the location.

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
deletes the gateways and builders it just created, and a later call starts the
install again.

## How the gateways are kept

Each location runs one gateway at a fixed private address: `fsn1` at
`10.0.0.2`, `nbg1` at `10.0.0.248` and `hel1` at `10.0.0.249`. Every node
trusts all three, and the network's default route goes to one of them. A
gateway is immutable. Nothing reaches it after its first boot, and a change of
the pinned image list replaces it.

The gateway CA carries critical name constraints that permit those three
addresses and no host name. `Installed` hands the block the CA, key included.
The block keeps it in its own store and passes it back as `Plane.GatewayCA`.
Without the CA, the plane refuses to build a gateway, with class
`gateway-ca-unknown`.

A gateway serves when Hetzner reads it running at its address and the newest
node report since its creation reads its mirror answering. A gateway no report
has read does not serve. `ReadGateway` gives the plane that reading, with `At`,
the time it was taken, which `report.GatewayReadAt` reads: the outside probe's
`probedAt`, else the body's `madeAt`, else the time the platform received the
report. The route is a problem only while its gateway is off its address or a
report reads its mirror failing, since a new cluster sends its first report
once it is built. Each call does one of the following,
as the cluster needs:

1. It moves the default route off a gateway that does not serve, to one that
   does, by deleting the route and adding it again.
2. It powers a stopped gateway back on, once. A gateway that still does not
   serve is never powered on again.
3. It replaces one gateway built from an earlier list, once the upgrade's own
   call, with `Upgrading` set, has marked the network. It takes first a gateway
   that does not serve, since replacing it takes nothing from those that do. It
   moves the route off the old gateway to one that serves, leaving it where it
   is when none does, and labels the old gateway with the time its
   replacement began, `mitstack.dev/removal-began`, and the digest of the list
   it is replaced for, `mitstack.dev/removal-for`. A label for another list
   counts for nothing, so an upgrade that took over from an abandoned one
   begins the replacement again. It deletes it only once
   block-kit's redundancy guard finds every other gateway read serving by a
   node report from after that time, and its event names each of those
   readings. Until then it deletes nothing, and each call records the guard's
   refusal, naming every other gateway with its reading or that it has none.
   It then builds the replacement at the same address with a fresh
   certificate, from a node report read after the deletion, as the
   paragraphs below explain. The next location waits until a node report
   reads the replacement serving.
4. It replaces a gateway built from this release's list that has failed,
   before an upgrade's next replacement. A gateway has failed when it is still
   down after that one power-on, or when it was never powered back on, runs
   past its deadline, and the newest node report since its creation does not
   read it serving. The
   plane moves the route off it and labels it as in step 3, with this
   release's digest, and deletes it once the guard finds every other gateway
   read serving by a node report from after that time. Just before the
   deletion it records the gateway's id on the network as
   `mitstack.dev/gateway-failed-<location>`. The location's next gateway is
   built from the same list at the same address, as the paragraphs below
   explain, and carries that id as `mitstack.dev/gateway-replaces`. A
   gateway that carries it and fails too is left in place, since a fault that
   repeats on one list needs a release with a new one. A gateway read serving
   again before its deletion keeps its place, and the plane takes its
   replacement label off.
5. It replaces a gateway whose serving certificate has reached its renewal
   point, which is `GatewayCertRenewBefore`, 30 days, before the certificate
   lapses. Hetzner fixes a server's user data when it creates the server, so
   the plane renews a certificate by replacing its gateway and never edits
   one. A certificate is valid for 365 days, and the plane labels each
   gateway with the time its certificate lapses,
   `mitstack.dev/gateway-cert-expires`, in Unix seconds. A gateway built
   before that label is judged from its creation, and so is one whose label
   names no time, which `Problems` names. The plane leaves a gateway that
   step 4 replaces to that step. Of the others it takes first one that does
   not serve, as in step 3, and else the first in location order. It moves
   the route off it and labels it as in step 3, with `certificate-renewal` in
   place of a digest. It deletes it once the guard finds every other gateway
   read serving by a node report from after that time. The location's next
   gateway is built at the same address with a new key and a certificate the
   kept CA signs, so the nodes trust it with no change to their
   configuration, and the old key goes with its server.

A renewal waits while an upgrade runs, since each gateway an upgrade builds
carries a new certificate. When no upgrade runs, a gateway built from an
earlier list is renewed like any other. Its successor is built from this
release's list and keeps each image the node report names, so once every
location's gateway has been renewed the gateways serve this release.

The plane keeps a gateway past its renewal point, and its event says why, in
four cases. In three it renews no gateway: the block holds no gateway CA, it
passes no redundancy guard, or no node report lists the images a replacement
keeps. In the fourth, Hetzner can order no gateway type in the gateway's
location, and the plane goes on to the next location's gateway.

No replacement begins, and none deletes a gateway, while Hetzner can order no
gateway type in that location, since the location would stay without a
gateway. An upgrade's replacement and a failed gateway's refuse with class
`server-unavailable`.

The plane acts only when a block calls it, so a renewal begins on the block's
first call at or after the renewal point.

The plane hands `Notify` an event for each route move, power-on, deletion and
kept gateway above, and for each certificate it issues. A certificate's event
names its serial, its validity and the time its renewal begins:

```text
issued the certificate of the gateway in fsn1 at 10.0.0.2: serial 5fc39fcbfd347a646563fca9ebfa5f81, valid from 2026-10-21T12:00:00Z until 2027-10-21T12:00:00Z, renewed by replacing the gateway from 2027-09-21T12:00:00Z
```

`Unhealthy` names a failed gateway with one of these causes, and its detail
names the gateway, the reading and how its replacement stands. A block's
catalogue type lists each cause word for word.

| Cause | The gateway | What happens next |
|---|---|---|
| `CauseGatewayDown` | Built from this release's list, and still down after its power-on | The plane replaces it |
| `CauseGatewayOverdue` | Built from this release's list, running past its deadline, and not read serving by the newest node report since its creation | The plane replaces it |
| `CauseGatewayFailedAgain` | A successor that failed from the same list as the gateway it replaced | The plane leaves it, and an upgrade to a release with a new list replaces it |
| `CauseEarlierGatewayDown` | Built from an earlier list, and still down after its power-on | An upgrade replaces it |

Each node pulls through the gateways' mirrors with no fallback to a public
registry, so a gateway built once nodes run keeps every image the cluster
still runs. Without that, a worker added after an upgrade could not pull an
image of the release before, nor one an app deployed. The cluster's admission
policies alone decide which images an app may run, and the gateway adds no
list of its own.

Such a gateway mirrors the release's pinned list and each image the `images`
member of the newest node report names, which `ReadReport` gives the plane.
It mirrors each one under its name and digest. It leaves out a tag that no
pod's status resolves to a digest, and an image on a registry the nodes reach
directly, outside `components.MirroredRegistries`. `Notify` receives an event
naming the report it read, each image it keeps beyond the release's list, and
each it leaves out with the reason. A running gateway never changes, so an
image a cluster stopped running leaves at the next gateway built.

When a location loses its gateway while nodes run, whether an upgrade deleted
it or it was lost, the plane records the loss as the network label
`mitstack.dev/gateway-gone-<location>`, rounded up to a whole second. It builds
the replacement only from a report that `ReadReport` returns for that time and
that was read after it, the rule the redundancy guard holds a removal to. The
replacement therefore never misses an image deployed while the location had no
gateway. Until such a report arrives, `NextStep` returns no change, and
`Problems` names the newest report's time. The plane clears the label once the
location has its gateway again. A plane given no `ReadReport` builds no
gateway once nodes run.

An upgrade's replacement reads the same list. When the cluster has sent no
report, or the newest one lists no images or names a read that failed, the
plane replaces no gateway. A report from a CronJob that predates the list
lists no images. The plane refuses the upgrade with class
`gateway-images-unread`, naming why, before any change.

Hetzner accepts at most 32,768 bytes of user data, and the kept images would
pass that as text. The gateway's user data therefore carries its cloud-config
gzipped and base64-encoded in a MIME part of type `application/gzip`, which
cloud-init decompresses before it reads the part. The install's first gateway
adds the etcd bootstrap as a second, plain part. The plane refuses user data
over the limit. `gateway.Render` refuses a cloud-config over the limit too,
unless `Inputs.Gzipped` says its caller sends it gzipped and judges its size
once compressed. Its tests log about 18,000 bytes for the install's first
gateway and about 29,900 for a gateway keeping 300 images, or about 19,300 and
31,300 on a cluster that hosts the platform.

A gateway bounds its own build. Each image copy into its mirror must finish
within 15 minutes. The tools image's read from the mirror, each file's copy
out of it and the removal of its container must each finish within 5 minutes.
The whole preparation must finish within an hour, and the TLS server's first
answer must come within 2 minutes. A stage that fails or overruns writes the
stage and its error to the VGA console, which Hetzner's web console shows, and
to the login prompt of every later boot, and then powers the gateway off. The
gateway has no login, so that screen is the only place to read why its build
failed.

The plane gives each gateway a deadline of 77 minutes after its creation,
`gateway.Deadline`, which adds 15 minutes for its boot and package install to
the bounds above. A gateway of this release's list that still runs by then,
and that the newest node report since its creation does not read serving, is
replaced, as step 4 above describes. While
an upgrade waits on such a gateway, the step's wait, which a block reports as
its status detail, names the gateway and its deadline. `Unhealthy` returns
cause `CauseGatewayOverdue` with the same words.

An upgrade replaces the gateways one location at a time, so it takes up to
`GatewayUpgradeBound`, 4 hours and 36 minutes, when no gateway fails. Each
location adds three intervals of the node report, `report.Interval`, to the
replacement's deadline. Two of them cover the reading of the other gateways,
since the report carries the outside probe's last result, and one covers the
report whose images the replacement keeps. The bound leaves out the time the
platform takes to call the block for each step between those waits. A block's
settle budget must cover the bound and that time, or the platform fails an
upgrade the plane still carries out.

Every step that removes a gateway, an etcd member or a Cloud worker asks one
guard first, block-kit's `redundancy.Set.AllowRemoval`. This module imports no
block framework, so the block passes the guard as `Plane.Guard`, or as
`RemoveMember.Guard` for etcd. The block's adapter copies each
`redundant.Set` into block-kit's own set and returns its answer unchanged. A
plane with no guard removes nothing, and its step fails naming the missing
guard. A refusal of class `redundancy-below-minimum` holds the step, and the
next call asks again. The kit's own tests pass `internal/redundanttest`, which
answers as block-kit v0.38.0's guard does, and the block's tests run the real
guard.

A cluster built before gateways ran in each location has one gateway with no
list label. The plane keeps its route and refuses its upgrade with class
`gateway-predates-zones`, and `Unhealthy` reads it as built by an older
release that must be rebuilt.

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

The load balancer forwards ports 80 and 443 to Traefik's NodePorts 30080 and
30081, where Traefik's two entry points listen: `web` for HTTP and `websecure`
for HTTPS. On both ports it sends a PROXY protocol header, which carries the
client's address. Traefik sees the load balancer's private address as the
sender, so each entry point reads that header from the cluster network,
`10.0.0.0/16`, and from no other address.

Traefik writes the ingress load balancer's public address into the status of
every Ingress it serves. Argo CD v3.5.5 reads an Ingress whose status holds no
address as Progressing, which means still starting, so without that address
no app behind an Ingress ever reads Healthy. The chart's own setting copies
each node's `ExternalIP` address there, and a node of the kit holds none, since
only a gateway has a public address. `Set.PublishingIngress` in `components`
therefore replaces that setting with Traefik's `ingressEndpoint.ip` argument,
which names one address. It refuses a second address, naming it, and it
refuses to render with no address at all.

The address has one source. `State.IngressAddresses` in `controlplane` reads
it from the ingress load balancer Hetzner holds. `Plane.Outputs` gives it to
the block as `publicIpByLocation`, and `Plane.Release` gives it to Traefik, as
[the section on how a running cluster takes a release](#how-a-running-cluster-takes-a-releases-components)
describes. The gateways serve Traefik's manifest without an address, since one
tools image is built for every cluster. A new cluster's Traefik therefore
takes the address from the first commit a block makes to the cluster
repository.

An app's Ingress asks for its certificate from the ClusterIssuer
`letsencrypt-production`, which the hosting set carries in
`components/hosting/tls.yaml`. It orders from Let's Encrypt's production
directory and answers the HTTP-01 challenge with a solver pod that Traefik
reaches in the app's namespace. That pod sits behind a ClusterIP Service,
since an app's quota allows no NodePort Service. Before Let's Encrypt
validates, cert-manager fetches the challenge itself from the app's public
hostname on port 80. The hosting flows therefore let cert-manager look names
up through Cilium's DNS proxy, reach `acme-v02.api.letsencrypt.org` on 443 and
`*.apps.mitstack.dev` on 80, and nothing else. That fetch leaves through the
gateway, which forwards port 80 to the ingress load balancer's public address
alone. Hetzner fixes a server's user data when it creates the server, so only
a cluster built on v0.27.0 or later carries that rule.

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

The gateway's `gateway/scripts/prepare.sh` fills the mirror first. It then
copies talosctl, kubectl and the served manifests out of the tools image the
mirror now holds, and only then writes `/var/lib/mitstack-gateway/mirror-complete`.
Caddy, which serves the mirror and the manifests on the private network, starts
only once that file exists, so the gateway answers no `/v2/` request before
the mirror is complete.

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
The order runs in five groups.

| Group | Manifests | Why it comes there |
|---|---|---|
| Mesh dataplane | Cilium, its policy CRD, Istio's base chart, istiod, istio-cni and ztunnel | A pod outside `kube-system`, `istio-system` and `kyverno` starts only once istio-cni and ztunnel run |
| Cluster reporting | The node report, the cluster tokens and the Collector | A component that never starts still leaves the platform its report |
| Components and policies | Every other component, policy, key and volume | No object here waits on a webhook but Istio's, whose pod runs outside the mesh |
| What a webhook validates | external-secrets and trust-manager, whose charts hold cert-manager objects, then `mitstack-hosting-tls` | cert-manager's and trust-manager's webhooks answer only once their pods run |
| Node labelling | The topology labeller | Last, a new manifest changes no other manifest's `TalosName` |

A new manifest takes a place in `applyOrder`, since `TalosName` panics on one
it leaves out. `TestBuilder_TalosAppliesEveryObjectAWebhookInterceptsAfterTheWebhookTheMeshAndTheNodeReport`
in `controlplane/render_test.go` sorts the Default and hosting sets as Talos
v1.14.4 does. It fails when an object a webhook's rules intercept applies
before the webhook's own manifests, ztunnel or istio-cni, or before the node
report and the cluster tokens.

## How each node is labelled with its zone

Every node carries two Kubernetes labels that say where it runs. The label
`topology.kubernetes.io/zone` holds its Hetzner location, `fsn1`, `nbg1` or
`hel1`. The label `topology.kubernetes.io/region` holds `eu-central`, the
Hetzner network zone every location of a cluster sits in, which
`controlplane.NetworkZone` names. A dedicated worker in Hetzner's `HEL1-DC1`
data centre takes `hel1`.

The labels reach the two kinds of node by two routes, since Talos sets a
node's labels only from its machine configuration.

| Node | Where its labels come from | Why |
|---|---|---|
| A Cloud node | The topology labeller, a CronJob in `mitstack-node` that the `topology` package renders | Every Cloud node boots the one configuration the builder embeds in the cluster's snapshot, so that configuration cannot name each node's own location |
| A dedicated worker | Its own configuration, from `nodeconfig.Node.Labels`, which a block fills with `controlplane.TopologyLabels` and the worker's location | Each dedicated worker gets a configuration rendered for it alone |

`nodeconfig.Render` refuses `Node.Labels` on a Cloud node, which would land on
every Cloud node, and refuses a label that replaces `mitstack.dev/subscription`.
It also refuses a label in Kubernetes's own `kubernetes.io` and `k8s.io`
namespaces unless NodeRestriction lets a kubelet set it, as it does the two
topology labels. A worker's Talos applies the node's labels with the kubelet's
credentials, so the API server would refuse such a label and every other label
of the node with it. A `node-role.kubernetes.io/` label is refused this way.
The rule also holds for a dedicated control-plane node, though Talos labels one
with an administrator's client, so a dedicated node's labels follow one rule
whatever its role. Talos's own validation refuses a key or value that is no
Kubernetes label.

The labeller runs every five minutes on a control-plane node. It names each
Cloud node by the name Hetzner gives its server, which is the node's Kubernetes
name. For each one it reads the Node, labels it with `kubectl label
--overwrite`, and leaves a node that has not joined yet to a later run. A read
that fails stops the run without a label, so the CronJob's failed Job shows
it. Each label it sets names the field manager `mitstack-node-topology` in the
Node's `managedFields`. A label that `kubectl` set by hand names `kubectl-label`
there instead, until the next run takes it over. The node report carries each
zone label's managers as `zoneLabelManagers`, so the platform can tell which
mechanism set it.

The labeller's ClusterRole grants `get` and `patch` on the Cloud nodes it
names, and nothing on any other object. It cannot list nodes, and it cannot
touch a node's status, which the API server serves apart from the rest of a
Node. A `patch` on one of those nodes still reaches more than the two labels.
It can change the node's other labels and its annotations, add or remove a
taint, mark the node unschedulable, and set its pod address ranges or provider
ID while they are still empty. It can also add a finalizer, which would hold up
a deletion of the node, such as the one `etcd/scripts/remove-member.sh` makes.
Labels and taints steer where pods land, and the hosting flows admit calls from
a node by its `node-role.kubernetes.io/control-plane` label. Every node the labeller names is a control-plane node already, so a
patch can take that label away but cannot give it to a node that lacks it.

The grant is acceptable because only a pod running as the labeller's service
account holds it. Running such a pod takes the right to create pods in
`mitstack-node`, which already reaches further: a pod there can mount any
Secret in the namespace, the Talos API identities and the report token among
them. The labeller's own pod runs the kit's tools image, as a user other than
root, on a read-only root file system.

A cluster gains the labeller and the report's zone fields only when it is
built from a release that carries them. A running cluster's nodes keep the
configuration their snapshot embedded, which the kit never changes after the
install. The gateway fills its image mirror once, at its first boot, so the
nodes could pull no newer tools image either.

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

Each report is also signed. The report token, the bearer secret the
enrolment Job in `controlplane/manifests/cluster-tokens.yaml` takes from the
platform, proves only that the sender holds it. The signature lets the platform tell the cluster's own report
from one sent by whoever holds a leaked copy of that token. The node never
holds a private key for this. Once the body is final, the report runs the
proof script its block passes as `Plane.ReportProofScript`, or as
`report.Inputs.ProofScript`. A block passes `reportproof.TokenRequestScript`
from block-kit, mitstack's shared Go module for writing blocks, and that
script asks the API server for a token for the report's own service account.
The token names the platform's origin, the scheme and host of `PlatformURL`
such as `https://api.mitstack.dev`, and the SHA-256 of the exact body. It goes
in the `Mitstack-Report-Proof` header. The Role `mitstack-node-report-sign`
lets that service account create a token for itself by name and for no other
account.

The proof script travels in the node report's manifest, inside the builder's
user data. `report.Manifests` therefore refuses a script longer than
`report.ProofScriptLimit`, 2,048 bytes.

An error report goes unsigned, because the token request may be what failed.
A proof script that exits 0 and prints nothing counts as a failed token
request too, so the report never posts a full body without a proof. block-kit's
[Signing a node report](https://github.com/mitstack-eu/block-kit/blob/main/docs/helpers.md#signing-a-node-report)
explains the proof and how the platform checks it.

The script comes from the block because this kit requires no block framework,
so a block on any framework release can use it. A block signs its reports
once it is on block-kit v0.12.0 or later, the first release with
`reportproof`. `report.Manifests` refuses inputs without a proof script, so a
block that has not passed one finds out when it renders the report.

## What Cilium holds of each node

Cilium is the cluster's network, so its pods change each node's kernel
networking. This section lists what they hold of the node, for you if you
judge a security verdict or change `components/render/cilium-values.yaml`.
The kit renders one Cilium for every Talos cluster, in the default set and in
the hosting set.

No Cilium container runs privileged, which would give it every power root has
on the node. Each container first drops every Linux capability, a named part
of root's powers such as `NET_ADMIN` for changing routes. It then adds only
the capabilities in its row of the table.

| Workload | Container | Capabilities it adds | Where the list comes from |
|---|---|---|---|
| `cilium` | `config` | `NET_ADMIN` | The chart's template |
| `cilium` | `apply-sysctl-overwrites` | `SYS_ADMIN`, `SYS_CHROOT`, `SYS_PTRACE` | The chart's default |
| `cilium` | `clean-cilium-state` | `NET_ADMIN`, `SYS_ADMIN`, `SYS_RESOURCE` | Talos's guide |
| `cilium` | `install-cni-binaries` | None | The chart's template |
| `cilium` | `cilium-agent` | `CHOWN`, `KILL`, `NET_ADMIN`, `NET_RAW`, `IPC_LOCK`, `SYS_ADMIN`, `SYS_RESOURCE`, `DAC_OVERRIDE`, `FOWNER`, `SETGID`, `SETUID` | Talos's guide |
| `cilium-envoy` | `cilium-envoy` | `NET_ADMIN`, `SYS_ADMIN` | The chart's default |
| `cilium-operator` | `cilium-operator` | None | The chart's default |

The lists for `cilium-agent` and `clean-cilium-state` are the ones
[Talos's guide to deploying Cilium](https://docs.siderolabs.com/kubernetes-guides/cni/deploying-cilium)
and [Cilium's install page, under Talos Linux](https://docs.cilium.io/en/v1.19/installation/k8s-install-helm/)
both give. They leave out `SYS_MODULE` of the chart's defaults, since Talos
lets no Kubernetes workload load a kernel module, and `SYSLOG`, which neither
page lists. The other lists are the chart's own.
[The chart's values at v1.19.9](https://github.com/cilium/cilium/blob/v1.19.9/install/kubernetes/cilium/values.yaml#L347-L432)
say what each capability is for, and
[its envoy values](https://github.com/cilium/cilium/blob/v1.19.9/install/kubernetes/cilium/values.yaml#L2831-L2850)
do the same for envoy, Cilium's proxy on each node.

The chart's one privileged container is `mount-bpf-fs`, an init container that
mounts the BPF filesystem, where Cilium keeps its kernel maps. Talos mounts
that filesystem at `/sys/fs/bpf` on every node at boot. `run` in
[its init at v1.14.4](https://github.com/siderolabs/talos/blob/v1.14.4/internal/app/init/main.go#L43-L53)
calls `PseudoSub` in
[its mount helpers](https://github.com/siderolabs/talos/blob/v1.14.4/internal/pkg/mount/v3/helpers.go#L381-L389),
which mounts it on every start. Talos's guide says to keep Cilium from
mounting it, so the values set `bpf.autoMount.enabled: false`, and the chart
renders no `mount-bpf-fs`.

With that setting the chart also drops envoy's own mount of `/sys/fs/bpf`.
The agent hands envoy that path as its BPF root, as `BpfRoot` in
[its listener configuration at v1.19.9](https://github.com/cilium/cilium/blob/v1.19.9/pkg/envoy/xds_server.go#L1012).
Envoy opens Cilium's connection-tracking and address maps under it, in
[`bpf_metadata.cc` of the proxy the chart pins](https://github.com/cilium/proxy/blob/00e749f11f69334c625e53fc1ce2fc6c7f34a51f/cilium/bpf_metadata.cc#L264-L291).
`envoy.extraHostPathMounts` in the values gives envoy the mount.

Beyond capabilities, all three workloads run on the node's network. The
chart's templates fix `hostNetwork: true` for the agent and for envoy, and its
`operator.hostNetwork` value, true by default, sets it for the operator. The
agent's pod runs with no seccomp and no AppArmor profile, the chart's
`podSecurityContext` default. The agent and envoy mount the host paths that
`components/cilium/cilium.yaml` lists under each DaemonSet's `volumes`.

`TestManifests_NoCiliumContainerRunsPrivilegedOrAddsACapabilityBeyondTheReadmesList`
in `rendered_set_test.go` reads the table above. It fails when a Cilium
container of either set runs privileged, keeps the container runtime's default
capabilities, or adds other capabilities than its row names. A chart version
that changes a list therefore fails until the table records it.

## How a cluster isolates each app

Every cluster runs Kyverno, an admission controller that checks each object
the API server is asked to create. Seven of its policies apply to every
namespace outside the system namespaces, which are `kube-system`,
`kube-public`, `kube-node-lease`, `argocd`, `kyverno`, `cnpg-system`,
`mitstack-node` and `istio-system`. The three policies that judge a pod's
volumes, limits and image skip five more namespaces, which
[the table of skipped namespaces](#which-namespaces-the-pod-policies-skip)
explains.

| Policy | What it does | File |
|---|---|---|
| `mitstack-require-app-label` | Refuses a pod without a non-empty `mitstack.dev/app` label, which an app's network policies select its pods by | `components/admission/policies/mitstack-require-app-label.yaml` |
| `mitstack-generate-default-deny` | Gives each namespace, new or existing, the NetworkPolicy `mitstack-default-deny`, which blocks every connection its pods make or take until an app's own policy allows it | `components/admission/policies/mitstack-generate-default-deny.yaml` |
| `mitstack-require-mesh` | Refuses a pod on the host network and a pod annotated `sidecar.istio.io/status`, the two kinds of pod the mesh leaves out. [The mesh section](#how-every-call-between-pods-proves-who-makes-it) explains the mesh | `components/admission/policies/mitstack-require-mesh.yaml` |
| `mitstack-require-strict-mtls` | Refuses a PeerAuthentication, Istio's policy for which calls a pod takes, that lets a pod or one of its ports take a call with no workload identity. It admits by name only the four exceptions of the hosting set, the extra components a cluster that hosts the platform runs. The CloudNativePG operator's exception sits in `cnpg-system`, which the policy skips | `components/admission/policies/mitstack-require-strict-mtls.yaml` |
| `mitstack-disallow-host-path` | Refuses a pod, or a Deployment or other controller whose template makes a pod, that mounts a host path, since a host path reaches the node's file system | `components/admission/policies/mitstack-disallow-host-path.yaml` |
| `mitstack-require-resource-limits` | Refuses a pod, or a controller whose template makes one, with a container or init container that sets no CPU limit or no memory limit. A LimitRange fills limits only into the pod, after the controller has been admitted, so the template must carry them | `components/admission/policies/mitstack-require-resource-limits.yaml` |
| `mitstack-restrict-image-registries` | Refuses a pod, or a controller whose template makes one, with a container, init container or ephemeral container whose image is outside `ghcr.io/mitstack-eu/` and mitstack's registry in Amazon ECR, `839001574838.dkr.ecr.eu-central-1.amazonaws.com/`. It admits by exact reference only the five images talos-cluster-kit pins for pods an operator or a block starts in an app namespace: cert-manager's ACME solver, the CloudNativePG operator, PgBouncer and Postgres 16 images, and `docker.io/library/busybox:1.37.0` at the digest the policy names, the image that the local path provisioner and the mesh probe run and the kube-app-deploy block runs in its `secrets-import` Job. Another busybox tag or digest is refused | `components/admission/policies/mitstack-restrict-image-registries.yaml` |

Kyverno's webhooks skip the same system namespaces, so a Kyverno outage
cannot stop a system pod, such as the node report in `mitstack-node`.

### Which namespaces the pod policies skip

`mitstack-disallow-host-path`, `mitstack-require-resource-limits` and
`mitstack-restrict-image-registries` skip every namespace the other policies
skip. They also skip the last five namespaces of the table below, because the
components the kit installs there run from upstream registries, set no limits
or mount a host path. `traefik`, `cert-manager`, `openbao` and
`external-secrets` exist only on a cluster that hosts the platform. No
namespace of an app, including the control plane's own app, is in this list.
Neither is `mitstack-admission-probe`, where the node report tries its refused
fixtures, as [the section on the report's admission rules](#how-the-report-tries-each-admission-rule)
explains. The policies judge each of them.

| Namespace | What runs there | Why the pod policies skip it |
|---|---|---|
| `kube-system` | Kubernetes' own components, Cilium (the network plugin), CoreDNS, Sealed Secrets, the CronJob that calls the mesh probe from outside the mesh and the daily security scan | Their images come from upstream registries and the scan reads node directories |
| `kube-public` | Nothing; Kubernetes creates the namespace | Every cluster has it, and the other policies skip it too |
| `kube-node-lease` | Nothing; Kubernetes keeps node leases in it | Every cluster has it, and the other policies skip it too |
| `argocd` | Argo CD | Its images come from `quay.io`, and a refusal would stop it applying the cluster |
| `kyverno` | Kyverno's controllers | Its images come from `ghcr.io/kyverno`, and a refusal would stop Kyverno restarting |
| `cnpg-system` | The CloudNativePG operator | Its image comes from `ghcr.io/cloudnative-pg` |
| `mitstack-node` | The node report, the topology labeller, the enrolment Job and OpenBao's init Job | They run the tools image, the one image talos-cluster-kit builds with its scripts in it, which every gateway mirrors |
| `istio-system` | istiod, the CNI agent and ztunnel | Their images come from `docker.io/istio`, and the CNI agent and ztunnel mount host paths |
| `traefik` | Traefik, the hosting set's ingress | Its image comes from Docker Hub |
| `cert-manager` | cert-manager and trust-manager | Their images come from `quay.io/jetstack` and their values set no limits |
| `openbao` | OpenBao's three members | Its image comes from `quay.io/openbao` and its values set no limits |
| `external-secrets` | External Secrets | Its images come from `ghcr.io/external-secrets` and its values set no limits |
| `local-path-storage` | The local path provisioner and its helper pods | The image comes from Docker Hub and a helper pod mounts the directory of a claim's volume on the node as a host path |

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
| The mesh probe's target | A pod in namespace `mitstack-mesh-probe` that answers on port 8080, which the node report and its outside probe call, as [the next section](#how-the-node-report-shows-the-mesh-refusing-a-call) explains | `components/mesh/probe.yaml` |

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

## How the node report shows the mesh refusing a call

The node report, the CronJob that posts each cluster's state to the platform
every five minutes, carries a `mesh` member. It shows that the cluster's mesh
answers a call with a workload identity and refuses one without, with no shell
on the cluster. The previous section explains the mesh: ztunnel, its node
proxy, takes every call to a pod in the mesh on that pod's port 15008, in
mutual TLS, and `kube-system`, `istio-system` and `kyverno` stay outside it.

The report calls a target, a busybox `httpd` in namespace
`mitstack-mesh-probe`. That is an ordinary app namespace, inside the mesh and
under Kyverno's label, mesh and strict mutual TLS policies and
`mitstack-default-deny`. The three pod policies judge it like any app, and
admit its busybox image because the registry policy names that exact reference.
The target answers
`mitstack-mesh-probe answered` on port 8080. It runs the busybox image the
local path provisioner already runs, by the same digest, so it adds no image to
the gateway's mirror, the registry every node pulls from. Each node fetches the
target's manifest from the gateway, as it fetches Istio's charts, because the
builder's user data, which carries every inline manifest, has no room left for
it. The Cilium policy `mitstack-mesh-probe-callers` admits two callers to the
target, on 8080 and on 15008, and no other pod:

- The node report itself, which runs inside the mesh, so ztunnel wraps its call
  in mutual TLS.
- The outside probe, a CronJob in `kube-system`. That namespace is outside the
  mesh, so the probe's calls carry no workload identity. Every five minutes it
  makes a plain call to port 8080. It also opens a TLS handshake to the
  target's port 15008. In a TLS handshake the server sends its certificate
  before it asks for the caller's, so the probe sees the certificate ztunnel
  presents for the target, and ztunnel then refuses the probe for presenting
  none. It then reads each gateway's mirror on 443, where ztunnel cannot turn a
  closed port into a TLS failure, and names how each connection ended. The
  probe writes these outcomes into the ConfigMap
  `kube-system/mitstack-mesh-probe`. It may read and patch that one ConfigMap
  by name, since `kubectl patch` reads an object before it patches it, and the
  report may only read it.

The probe's own Cilium policy, `mitstack-mesh-probe-outside`, lets it look up
the target's name and no other, reach the API server, the target's two ports
and each gateway's address on 443, and nothing else. The probe writes the
target's name with a final dot, which makes it an absolute DNS name. Without the dot, the resolver would
first try the name under each search domain, Cilium's DNS proxy would refuse
those names, and the probe's resolver, musl's, stops at the first refusal.

`mesh` holds three results, each with `passed` and, when it fails, a `reason`:

| Result | Passes when |
|---|---|
| `callFromInside` | The report's own call gets the target's answer |
| `workloadIdentity` | The certificate the probe saw on port 15008 names `spiffe://cluster.local/ns/mitstack-mesh-probe/sa/mitstack-mesh-probe`, and the time of the probe's run falls within the certificate's validity. The result also carries the certificate's `serial` and its validity, `notBefore` to `notAfter` |
| `plainCallFromOutside` | The plain call reached the target and was closed with no answer, which curl reports as exit status 52, an empty reply, or 56, a reset connection. `workloadIdentity` and `callFromInside` must pass too, so a target that is down never reads as refusing |

Suppose the mesh stopped holding the target, for example because ztunnel no
longer runs on its node. No certificate then comes back from port 15008, so
`workloadIdentity` fails, and `plainCallFromOutside` fails with it whatever
the plain call did. A plain call that gets an answer fails on its own.

Each of these also fails the results it touches, with the reason named, and
the report still posts in full:

- a missing ConfigMap;
- a result older than 15 minutes, three of the probe's runs;
- a result dated after the report;
- a refused handshake;
- a result the report cannot parse.

[How every call between pods proves who makes it](#how-every-call-between-pods-proves-who-makes-it)
says a workload certificate lasts 24 hours and ztunnel asks for a new one when
half of that has passed. Two reports more than 12 hours apart that carry two
different serials, each within its validity, therefore show the target's
identity replaced before it expired.

The report reads the identity, serial and validity of the certificate
presented on port 15008. It does not check that istiod, the mesh's control
plane, signed that certificate.

## What the node report carries

The node report is a CronJob in `mitstack-node`. Each run reads the cluster
through the API server and the Talos API and posts one body to the platform.
The platform judges that body on nine named checks, and the result is the
cluster's verdict. This section is for you if you read a report's body or
change `report/scripts/report.sh`.

### The members of a report

| Member | What it holds | Where it comes from |
|---|---|---|
| `nodes` | Each node with its Talos version, allocatable resources, every address it holds with that address's type as `addresses`, `topology.kubernetes.io/zone` and `topology.kubernetes.io/region` labels, and, as `zoneLabelManagers`, each field manager that wrote its zone label | `kubectl get nodes --show-managed-fields` |
| `etcd` | Each etcd member, `healthy` only when its node answered `talosctl etcd status` with an empty `ERRORS` cell, the `leader`, and a `statusError` naming a status read that failed | `talosctl etcd members` and `talosctl etcd status` |
| `systemVolumes` | How each node's `STATE` and `EPHEMERAL` volumes are encrypted | `talosctl get volumestatus` |
| `argocd` | Each Application's health, sync, `revision` and applied objects, each object with Argo CD's health of it, as [the section on the revision](#how-the-report-names-the-revision-each-application-was-compared-against) and [the section on object health](#how-the-report-shows-each-objects-health-and-each-containers-state) describe, under `routes` the answer each of 17 routes gives a caller with no session, and the `argocd` namespace's Services and Ingresses. The settings route's entry also says [whether any account can sign in with a password](#how-the-report-shows-whether-a-password-signs-in-to-argo-cd) | The Applications, `ARGOCD_URL` and the Ingress read |
| `kubernetes.argocdPods` | Each pod in `argocd` with its containers' images | The pod read |
| `pods` | Every pod's namespace, name, node or `null`, phase, and `containers`, each init and app container with its state and reason, as [the section below](#how-the-report-shows-each-objects-health-and-each-containers-state) describes | `kubectl get pods -A` |
| `images` | Each image reference a pod or the pod template of a Deployment, StatefulSet, DaemonSet, Job or CronJob names, once, as `image` with the `digest` it names or that a pod's status reports it runs by, and otherwise `null`. A failed read leaves `{exitStatus, stderr}` with `read` naming `pods` or `workloads`, in place of a partial list | The pod read and `kubectl get deployments,statefulsets,daemonsets,jobs,cronjobs -A` |
| `cloudNativePG` | Each Cluster with its phase, instance counts and `currentPrimary`, and each Database and DatabaseRole with whether it applied | CloudNativePG's objects |
| `openbao` | Each member's `initialized`, `sealed` and `active`, and the active member's name as `leader` | `sys/seal-status` and `sys/leader` on each member in `OPENBAO_MEMBERS`, verified against `OPENBAO_CA_FILE` |
| `storage` and `services` | Each claim with its storage class, phase, `requestedSize` (what it asks for) and `capacity` (what is bound, `null` while it is Pending), the default storage classes, and every Service with its type | One read of claims, StorageClasses and Services |
| `ingresses` | Every Ingress in every namespace with its `namespace`, `name` and `class`, and under `status.loadBalancer.ingress` each address its controller wrote, as an `ip` or a `hostname`. The class is the Ingress's `ingressClassName`, or else its `kubernetes.io/ingress.class` annotation cut to 256 characters, or `null`. An Ingress no controller serves carries an empty list, which is how Argo CD reads it as still starting | `kubectl get ingresses -A`, the read `argocd` takes its own Ingresses from |
| `defaultDenyPolicies` | Each NetworkPolicy named `mitstack-default-deny`, with its pod selector, policy types, rule counts and `managedBy` label | The NetworkPolicy read |
| `subscriptionResources` and `subscriptionResourceKinds` | The objects a block labels `mitstack.dev/subscription`, and the object types the report read whole | One read per object type in `subscription_kinds` in `report.sh` |
| `jwks` | The key set the API server signs service-account tokens with | `/openid/v1/jwks` |
| `mesh` | The mesh probe's results, which [the section on the mesh](#how-the-node-report-shows-the-mesh-refusing-a-call) explains | The probe's target and its ConfigMap |
| `gateways` | Each gateway's `address`, its `connection`, and `passed` when its mirror answered HTTP 200 on `/v2/` over a certificate the gateway CA signs, and otherwise a `reason` naming curl's exit status or the answer. The `connection` is `closed` when nothing took the connection, `reset` when the peer reset or dropped it, `tls-failure` when the handshake or the certificate failed, `timed-out` after 5 seconds without an answer, `answered` for an HTTP status other than 200, `served` for 200, and `failed` for any other curl error. `probedAt` is when the outside probe read it | The outside probe's reading of each gateway's mirror. `GATEWAYS` lists the gateways' private addresses, and the probe verifies each mirror against `GATEWAY_CA`, the gateway CA's certificate alone, from outside the mesh |
| `egress` | `passed` when a TLS connection to Let's Encrypt's ACME directory opened and answered, and otherwise a `reason` naming curl's exit status. The connection leaves through the network's default route, so it reads the egress of the gateway that route names | `EGRESS_URL`, `https://acme-v02.api.letsencrypt.org/directory` |
| `repositorySecrets` | Each repository credential the cluster declares, with its `name`, `secretType`, `project` and `url`. These come from the template of each SealedSecret in `argocd` that carries an `argocd.argoproj.io/secret-type` label, so the report reads no Secret and carries no ciphertext. A credential written straight into a Secret, as the cluster tokens write `cluster-repository` on a cluster that reads its repository with a platform token, is not listed | `kubectl get sealedsecrets.bitnami.com -n argocd` |
| `appProjects` | Each AppProject with its `sourceRepos`, which the platform holds each app's credential to | `kubectl get appprojects.argoproj.io -n argocd` |
| `admission` | Each fixture in the `admission-fixtures` ConfigMap of `mitstack-admission-probe` whose key names a rule, with the `rule`, the `fixture` up to 4 KiB, the dry run's `exitStatus` and the API server's `message` up to 2 KiB | `kubectl create --dry-run=server` in the probe namespace, [described below](#how-the-report-tries-each-admission-rule) |
| `securityScans` | The scan CronJob's last start and success as `runner`, with that CronJob's newest Job as `runner.job` (its counts and conditions) and the newest 20 events of that Job and its pods as `runner.events`, so a scan that never succeeds names its cause, the scan's outcome as `lastRun`, and the summaries it kept as `kubeBench` and `kubescape`. A summary that is not one JSON object within 16 KiB reads `unreadable` | The `mitstack-security-scans` ConfigMap and the `mitstack-security-scan` CronJob in `kube-system`, [described below](#how-the-security-scan-runs) |
| `checks` | Contract `node-report/v1` and the nine checks the report declares | Fixed text in `report.sh` |

The nine checks are `routes`, `exposure`, `credentials`, `version`,
`advisories`, `admission`, `default-deny`, `cis-benchmark` and `nsa-scan`. The
platform fails a check whose member says `unreadable`, and gives that reason
as the finding.

The 17 routes are listed in `argocd_routes` in `report.sh`. Each request
sends no credential, cookie or body, so Argo CD sees a caller with no session,
one who has not signed in. The report keeps each answer's status and its size
in `bytes`. Argo CD answers two of the routes for such a caller, userinfo and
settings, and the report keeps one boolean of each:
[the section on the local login](#how-the-report-shows-whether-a-password-signs-in-to-argo-cd)
describes both.

### How the report names the revision each Application was compared against

A block that deploys through Argo CD commits to the repository Argo CD reads,
and can then read its Application's health from the node report to judge the
deploy. Health alone does not say which commit Argo CD judged, so a Healthy
Application read before Argo CD saw the block's commit looks the same as one
read after it. Each Application therefore carries `revision`, the commit Argo
CD last compared it against, from the Application's `status.sync.revision`. A
reader can then tell whether the health belongs to its own commit.

The `revision` reads `null` in two cases, and a reader takes either as no
revision to judge. Argo CD writes no revision until it has compared the
Application once. An Application with several sources keeps one revision per
source in `status.sync.revisions`, which the report does not read. The
revision stays when the report leaves out the applied objects to keep the body
within the platform's 256 KiB. Two Applications of such a report read like
this, where Argo CD wrote a revision for `cluster-root` and none for `shop`:

```json
{"argocd": {"applications": [
  {"name": "cluster-root", "health": "Degraded", "sync": "Synced", "revision": "9b2c4e7a1f3d5c8e0a6b4d2f9e7c5a3b1d8f6e4c"},
  {"name": "shop", "health": "Degraded", "sync": "OutOfSync", "revision": null}
]}}
```

### How the report shows each object's health and each container's state

A failed deploy needs the object that failed and the reason, which an
Application's own health cannot give. So each object an Application applied
carries Argo CD's `health` of it. An object with any health but `Healthy` also
carries Argo CD's `message`, cut to 256 characters. A kind Argo CD runs no
health check for, such as a ServiceAccount, reads `"health": null`.

Argo CD 3 keeps an object's health on its Application only while
`controller.resource.health.persist` is `"true"` in `argocd-cmd-params-cm`.
Otherwise it keeps the health only in its resource tree, which the report
does not read. talos-cluster-kit sets the value in
`components/render/argocd-values.yaml`, which adds one health entry per
object to each Application's status, about 30 bytes for a Healthy object. An
Application that reads `"resourceHealthSource": "appTree"`, as on a cluster
still running an earlier release, gives its objects no `health` member at
all. A reader then sees that the report could not read the health, rather
than reading `null` as no health check.

Each pod lists its init and app containers under `containers`, each with its
`state` and the `reason` Kubernetes gives. A waiting container also carries
its `message`, cut to 256 characters. A terminated container carries its
reason alone, since its message is whatever the app wrote to its termination
log. One Application and one pod read like this:

```json
{
  "argocd": {"applications": [{"name": "cluster-root", "health": "Degraded", "sync": "Synced", "revision": "9b2c4e7a1f3d5c8e0a6b4d2f9e7c5a3b1d8f6e4c", "resources": [
    {"group": "external-secrets.io", "kind": "ExternalSecret", "namespace": "app-shop", "name": "shop-db", "sync": "Synced",
      "health": "Degraded", "message": "could not get secret data from provider: ..."},
    {"group": "", "kind": "ServiceAccount", "namespace": "app-shop", "name": "shop-runtime", "sync": "Synced", "health": null}
  ]}]},
  "pods": [{"namespace": "app-pg", "name": "shared-1", "node": "poc-talos4-cp2", "phase": "Pending", "containers": [
    {"name": "bootstrap-controller", "state": "terminated", "reason": "Completed"},
    {"name": "postgres", "state": "waiting", "reason": "ImagePullBackOff", "message": "Back-off pulling image ..."}
  ]}]
}
```

`applied` and `container` in `report/scripts/report.sh` build these entries.
The report's ClusterRole grants no new right for them, since both come from the
Application and pod reads the report already makes.

### How the report shows whether a password signs in to Argo CD

Argo CD's chart turns on a local `admin` account that signs in with a
password. Nothing on a cluster signs in as that account, so talos-cluster-kit
turns it off. `components/render/argocd-values.yaml` sets
`configs.cm.admin.enabled` to false, which the chart writes into `argocd-cm`,
the ConfigMap that holds Argo CD's settings, as `admin.enabled: "false"`.
Argo CD v3.5.5 generates the account's first password only while the account
is on, so a new cluster holds no admin password.

The report shows the result on every run. Argo CD's settings route answers a
caller with no session, and its answer holds `userLoginsDisabled`, which reads
`true` while no enabled account may log in. The report keeps that one boolean
on the settings entry of the `routes` list in its `argocd` member, as in this
entry from a cluster whose local login is off:

```json
{"path": "/api/v1/settings", "status": 200, "bytes": 1432, "userLoginsDisabled": true}
```

The entry's `userLoginsDisabled` reads `null` when the route answers another
status than 200, when its answer is empty or not JSON, or when the answer
holds no boolean under that name. Argo CD v3.5.5 leaves the field out of an
answer where it would read `false`, so a cluster whose local login is on reads
`null`. The entry holds no `userLoginsDisabled` at all when Argo CD does not
answer the route, and it then carries an `error`. A reader takes only `true`
as proof that no password signs in.

The userinfo route's entry carries `loggedIn` the same way: the boolean its
answer holds, which says whether Argo CD took the caller for signed in, or
`null`.

`argocd_route` in `report/scripts/report.sh` builds each entry. It keeps the
status, the size and the one boolean, so no other text of either answer
leaves the cluster.

### How the report tries each admission rule

The `mitstack-admission-probe` namespace enforces Pod Security `restricted`
and holds one fixture per rule in `report/admission/`, each breaking its own
rule and no other. Five are Deployments, which Pod Security only warns about,
so the Kyverno rule they break is what refuses them. The sixth,
`pod-security-restricted`, is a privileged Pod that breaks no Kyverno rule.
The `admission-fixtures` ConfigMap holds each fixture as one line of JSON.
The gateways mirror every image an inline manifest names on a line of its
own, and a fixture's images exist only to be refused.

The report asks the API server for a server-side dry run of each fixture.
It may only read the fixtures and create pods and Deployments in that
namespace. A ValidatingAdmissionPolicy there refuses every request that is not
a dry run, so nothing the report sends is ever kept.

The platform requires a refusal naming its rule for each of the six. A
Talos cluster runs four of the five Kyverno rules the Deployment fixtures break:
`mitstack-require-app-label`,
`mitstack-disallow-host-path`, `mitstack-require-resource-limits` and
`mitstack-restrict-image-registries`. The fixture for the fifth,
`mitstack-require-image-digest`, reads `accepted`, and the admission check
fails naming it until the cluster enforces that rule.

### How the security scan runs

The `mitstack-security-scan` CronJob runs once a day in `kube-system`, on a
control-plane node, from the tools image. kube-bench runs the `cis-1.10`
benchmark, the one Sidero documents for Talos, since kube-bench's version
mapping picks none for Talos. It reads each component's flags from its
process and `/etc/kubernetes`, `/var/lib/kubelet` and `/var/lib/etcd`
read-only. kubescape scans the NSA framework with an empty exception list and
no host sensor, under an identity that may read the cluster's objects but no
Secret.

kube-bench reads the files of the node it runs on, so the scan runs on a
control-plane node, the only kind that holds the API server, the controller
manager, the scheduler and etcd. Each run reads one control-plane node, the
one the scheduler picks, and no run reads a worker's kubelet.

The etcd directory is mounted with no type check. Talos's kubelet service
starts the kubelet in a container of its own, whose mount list holds
`/etc/kubernetes` and `/var/lib/kubelet` and no `/var/lib/etcd`. The kubelet
checks a host path's type against its own files, so a pod that asks for a
check on the etcd directory waits on "does not exist" and never starts.
Kubernetes makes no check on a host path with no type. Talos runs the
container runtime on the node itself, which is where the mount is made, so
kube-bench is expected to read the mode and owner Talos gave the directory.
No test in this repository runs on a Talos node, so a cluster's own scan is
what shows the mount.

Three more things let kube-bench read a Talos node truthfully.

| What the scan does | Why |
|---|---|
| It names no variable that starts with `KUBE_BENCH_` | kube-bench reads its own settings under that prefix. It takes `KUBE_BENCH_VERSION` for its `--version` flag and refuses that flag beside `--benchmark` |
| The tools image installs `procps` | kube-bench finds each component's process with `ps -C`, which busybox's `ps` refuses. Without it, every check of a component's flags reads nothing and fails |
| `tools/kube-bench-talos.yaml` stands where kube-bench reads the settings that override its config for `cis-1.10`, and names `/var/lib/etcd` as etcd's data directory | Talos gives that directory mode 700 and etcd's own user. kube-bench's default looks up a path inside it, which the scan's root may not do with every capability dropped, and kube-bench then stops |

The two tools run as two steps of one pod, so neither holds both the host's
files and the API token. kube-bench runs first, as root with those three
directories and no token, and leaves its summary in a shared scratch
directory. kubescape runs after it as a non-root user, with the token and no
host file, and then patches both summaries. A non-root user cannot reach a
root process through the host's process table.

The scan patches each summary into the `mitstack-security-scans` ConfigMap,
which its Role lets it read and patch by name, and which the report may only
read. Each failed list is cut to fit 16 KiB while the totals stay whole, so the
platform sees when a list was cut. A tool that fails is kept as its summary's
`error`, and the Job runs again. `tools/scan.env` pins kube-bench, kubescape
and the NSA framework by SHA-256.

A scan that never succeeds leaves no summary, so the report says why itself.
`runner.job` is the CronJob's newest Job with its active, succeeded and failed
counts and its conditions, and `runner.events` holds the newest 20 events of
that Job and of the pods it created, newest first. A Job that cannot create
its pod, for example because an admission webhook refuses it, shows the refusal in an event and
nowhere else. The report's Role in `kube-system` lets it list that
namespace's events and no other namespace's, and it keeps only those of the
Job. When the Job or event read fails, that member holds `{exitStatus, stderr}`
and `runner.output` still says when the CronJob last started a scan.

### What happens when a read fails

Each read runs under `REPORT_READ_SECONDS`, 21 seconds by default. Most reads
that fail stay inline in a full report, as `{exitStatus, stderr}` in the member
that read fills, so one store that does not answer hides nothing else. A route
or OpenBao read that fails is kept in its own entry the same way, and a gateway
or egress read that fails reads `passed: false` with its reason. An object
type whose read fails is left out of `subscriptionResourceKinds`.

Every report needs four reads: the nodes, the Applications, the key set and
the etcd members. When one of those, the body build or the proof script fails
or times out, the script posts a report holding only an `error` that names it,
and exits non-zero.

No Application carries its applied objects when they would take the body past
the platform's 256 KiB, and each still carries its name, health, sync and
revision. Nothing trims `storage`, `services`, `pods` or `images`, since a
partial list would read as a whole one. In the report's tests, 1,000 pods
naming 300 distinct images make a body of 151,968 bytes.

### How a report is signed

The proof script is block-kit's `token-request.sh`, which the block passes in
`REPORT_PROOF_SCRIPT`. Once the body is final, the report runs it with the
body file, and it asks the API server for a token minted for the body's
SHA-256. The post carries that token as `Mitstack-Report-Proof`, so the
platform can tell the body came from this cluster unchanged. A report holding
only an `error` goes unsigned. The report's service account holds no right on
Secrets.

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
returns each one for the gateway's mirror. The gateway copies each image under
the name the list gives it. A node's containerd expands a Docker Hub name that
has no namespace, so it asks the mirror for `docker.io/library/traefik` when a
pod names `docker.io/traefik`. `mirror.List` therefore refuses such a name, and
every Docker Hub image goes on the list under its full name.

| Component | What it does here | Values |
|---|---|---|
| OpenBao 2.7.1 | Three Raft members, one per control-plane node, each tolerating the control-plane taint. The listener serves TLS 1.3 only, with the certificate cert-manager issues, and reloads it when it is renewed. The static seal reads its key from `/openbao/seal/key` | `components/render/openbao-values.yaml` |
| cert-manager v1.21.2 | Issues OpenBao's serving certificate from OpenBao's own authority, the shared Postgres server's from the database authority, and the External Secrets webhook's certificate. It reads each `ClusterIssuer`'s key in `mitstack-trust` | `components/render/cert-manager-values.yaml` |
| trust-manager v0.25.0 | Copies each authority's certificate from `mitstack-trust` into a ConfigMap of the authority's name, under `ca.crt`, in every namespace | `components/render/trust-manager-values.yaml` |
| External Secrets v2.11.0 | Copies values from OpenBao into Kubernetes Secrets | `components/render/external-secrets-values.yaml` |
| Sealed Secrets 0.40.0 | Opens a `SealedSecret` with the block's key pair, and never mints or renews a key | `components/render/sealed-secrets-values.yaml` |
| Traefik v3.7.13 | Serves web traffic on NodePorts 30080 and 30081, with one pod on every node that takes apps, and writes the ingress load balancer's address into each Ingress's status | `components/render/traefik-values.yaml` |

The two authorities that the next section describes, and the certificates
they sign, sit in `components/hosting/tls.yaml`. A
`SecretStore`, the External Secrets object that names a store, verifies
OpenBao through its Vault provider's `caProvider`, as in this example:

```yaml
caProvider:
  type: ConfigMap
  name: openbao-ca
  key: ca.crt
```

### How the two authorities reach every namespace

A client trusts a server only when it can check the server's certificate
against an authority, a certificate authority it already holds. The hosting
set creates two authorities, and each one vouches for one kind of server.
Every namespace gets both authorities' certificates. No slice, the namespace
a block gives one app, gets either key.

| Authority | `ClusterIssuer` | What it signs | ConfigMap in every namespace |
|---|---|---|---|
| OpenBao's | `openbao-ca` | OpenBao's serving certificate, `openbao-tls` in `openbao` | `openbao-ca` |
| The database authority | `mitstack-database-ca` | The certificate of the Postgres server that the postgres-db-kube block runs for every app on the cluster. The block asks for it in the server's namespace, its database slice | `mitstack-database-ca` |

A `ClusterIssuer` is a cert-manager authority any namespace can name, and a
`Bundle` is trust-manager's instruction to copy a certificate into a ConfigMap
in every namespace. Both keys sit in the namespace `mitstack-trust`. No
workload runs there, so only cert-manager and trust-manager read it. Each
authority's own certificate is signed by its own key, through the self-signed
`Issuer` `mitstack-trust-selfsigned` there. cert-manager reads a `ClusterIssuer`'s key only in one namespace,
its cluster resource namespace. trust-manager reads a `Bundle`'s sources only
in one namespace too, its trust namespace. The set points both at
`mitstack-trust`, through
`clusterResourceNamespace` in `components/render/cert-manager-values.yaml` and
`app.trust.namespace` in `components/render/trust-manager-values.yaml`. The
`letsencrypt-production` `ClusterIssuer` keeps its Let's Encrypt account key
there too. trust-manager can read that key, but no `Bundle` names it, so it is
never copied.

A `ClusterIssuer` signs for any namespace whose `Certificate` names it, and an
app's repository may hold a `Certificate` or an Ingress annotated with an
issuer. An app that obtained a certificate for `postgres-rw` from the database
authority could pose as the Postgres server to every other app. Kyverno, the
admission policy engine every cluster runs, therefore holds the policy
`mitstack-restrict-trust-issuers` in `components/hosting/tls.yaml`. It refuses
a `Certificate`, or the `CertificateRequest` that cert-manager signs, when it
names `openbao-ca` outside `openbao`. It also refuses one naming
`mitstack-database-ca` outside a namespace labelled
`mitstack.dev/database-slice: "true"`.

The k3s-app-slice block puts that label on a database slice's namespace when
it creates the slice. An app cannot set it, because the slice's Argo CD project
admits no cluster-wide object, and a Namespace is one. Kyverno's webhooks skip
`kube-system`, `kube-public`, `kube-node-lease`, `argocd`, `kyverno`,
`cnpg-system`, `mitstack-node` and `istio-system`. Those are the namespaces of
Kubernetes itself and of the cluster's own components, which no app writes to.

An app checks the shared Postgres server against the database authority by
mounting its ConfigMap as a volume and connecting with `sslmode=verify-full`.
The volume goes in the pod spec of the app's Deployment:

```yaml
volumes:
  - name: authority
    configMap:
      name: mitstack-database-ca
containers:
  - name: app
    volumeMounts:
      - name: authority
        mountPath: /authority
        readOnly: true
```

In this example the database slice is `app-postgres`, so the server answers
at `postgres-rw.app-postgres.svc.cluster.local`. The database and its user,
here both `orders`, are the ones the app's database block created:

```bash
psql "host=postgres-rw.app-postgres.svc.cluster.local dbname=orders user=orders sslmode=verify-full sslrootcert=/authority/ca.crt"
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
takes the serving server with it.

The `serve` package holds the server's key, its files and its command, and a
block reuses it to serve any one file to one client.

### What fits in the builder's user data

This section is for an engineer who adds a manifest, a script or a setting to
the kit. It says what the builder's user data carries, what reaches a node
another way, and which test keeps room free.

Hetzner accepts at most 32,768 bytes of user data, and the builder's carries
everything a control-plane node boots with.

| Part | How it reaches a node |
|---|---|
| The machine configuration with the cluster's keys | In the builder's user data, gzipped |
| The default set's inline manifests | Inside the machine configuration |
| The node report, the topology labeller, the cluster tokens and the cluster's Application, each rendered for one cluster | Inside the machine configuration |
| `controlplane/scripts/build-image.sh` and the gateway CA's certificate | In the builder's user data |
| The node report's, its outside probe's, the security scan's, the labeller's and the cluster tokens' scripts | In the tools image at `tools.ScriptDir`, where each command names its script's path |
| Each served manifest | From the gateway, which copies it out of the tools image |
| The hosting set's inline manifests | From the serving server, where the builder fetches them |

A script in the tools image costs no server any user data, because the
gateway mirrors that image whole. A served manifest does cost some. The
gateway's user data has the same limit, and it names each served manifest with
its sha256 and its path, about 75 bytes once gzipped. The gateway of a cluster
that hosts the platform takes about 31,300 bytes when it keeps 300 images.

`TestPlane_EveryServerOfAnInstallKeepsTheUserDataReserveWithTheLongestProofScriptUnderEachOptionOfThePlane`
in `controlplane/render_test.go` keeps room free. It installs a cluster four
ways: with and without the platform, and with apps on the control plane or on
workers. Each plane takes a proof script of `report.ProofScriptLimit` bytes of
random printable text, which gzip shortens by about a sixth. The test fails
when any server's user data leaves less than 1,024 bytes of the limit, and it
logs each size.

The reserve covers what the test does not hold at its largest. A block's names
and URLs come first. An app name and a subscription id of 63 characters, a
repository URL of 159 and a platform host of 75 add 544 bytes when they
compress poorly. The builder's size also moves from one test run to the next,
by up to 224 bytes with the same settings. Inside one run, where every install
shares one set of minted keys, it moves by 40 at most. The two sum to 768
bytes, and the reserve is a third more.

With that proof script the builder takes about 30,800 bytes, or about 30,900
on a cluster that hosts the platform. With a one-line proof script, as the
tests in `controlplane/render_test.go` and `controlplane/hosting_test.go` log
it, the builder takes about 28,100 and 28,200 bytes.

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
takes no repository token from the platform, and the cluster runs no CronJob
that renews one. The environment value `REPOSITORY_CREDENTIAL=deploy-key` tells
`controlplane/scripts/cluster-tokens.sh` so. A cluster that hosts no platform
reads its repository with the short-lived token the platform issues, as
`REPOSITORY_CREDENTIAL=platform-token`.

### How Argo CD tells the platform its verdict

A block that commits to the cluster repository waits for Argo CD's verdict on
that commit: whether the Application synced it and is healthy. The platform
answers that wait at `GET /v1/gitops-notifications`. Argo CD's notifications
controller posts each Application's sync outcome to the platform's
`POST /v1/gitops-notifications`, on every cluster this kit builds.

The `argocd-notifications-cm` ConfigMap in `components/argocd/argocd.yaml`
holds four parts. The webhook is the HTTP call to the platform, and its body
is the JSON below. A trigger is a condition on an Application that sends the
call. The subscription, in Argo CD's sense rather than a platform
subscription, attaches every Application to all three triggers. Its values
come from `components/render/argocd-values.yaml`.

| Trigger | When it posts |
|---|---|
| `on-deployed` | The operation succeeded and the Application is healthy, once per synced revision |
| `on-sync-failed` | The operation ended in `Error` or `Failed` |
| `on-health-degraded` | The Application's health is `Degraded` |

The ConfigMap names no platform and holds no token. Argo CD fills in
`$platform-url` and `$platform-token` from `argocd-notifications-secret`,
which the enrolment Job writes from the block's `PLATFORM_URL` and the
`notificationToken` of the platform's enrolment answer. A post for an
Application that synced `ea12fd5f` carries this body:

```json
{
  "app": "app-mitstack-home",
  "project": "default",
  "sync": "Synced",
  "revision": "ea12fd5f",
  "health": "Healthy",
  "phase": "Succeeded",
  "message": "successfully synced (all tasks run)",
  "reconciledAt": "2026-10-30T17:29:10Z"
}
```

Only the enrolment answer carries the notification token, and Talos creates
each manifest object once, so a cluster gains the wiring at its build alone.

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
| External Secrets | Calls to the API server and to OpenBao on 8200, lookups through Cilium's DNS proxy, calls to `sts.amazonaws.com`, `sts.*.amazonaws.com` and `api.ecr.*.amazonaws.com` on 443, and the API server's calls to its webhook. The policy `mitstack-external-secrets-ssm` of its own adds calls to `ssm.*.amazonaws.com` on 443 |
| OpenBao | Calls between members on 8200 and 8201, to the API server and to cluster DNS, and the calls of External Secrets and the init Job on 8200 |
| OpenBao's init Job | Lookups through Cilium's DNS proxy, calls to OpenBao on 8200, and SSH to `ssh.github.com` on 443, which the gateway forwards |
| Traefik | Web traffic from outside the cluster on its two ports, calls to the API server, and calls to the app slices |

External Secrets' `ECRAuthorizationToken` generator trades a service
account's token for AWS credentials at STS, then asks ECR's API for a pull
token. Cilium admits a call by name only after its DNS proxy saw the lookup
that returned the address, so the lookups go through the proxy. In the middle
of a Cilium `matchPattern`, a `*` matches characters other than a dot
([Cilium's FQDN selector at v1.19.9](https://github.com/cilium/cilium/blob/v1.19.9/pkg/policy/api/fqdn.go#L42-L61)).
`sts.*.amazonaws.com` therefore admits a regional endpoint such as
`sts.eu-central-1.amazonaws.com`, and admits no name with a further label.

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

External Secrets reads SSM parameters, AWS's store of configuration values,
through `mitstack-external-secrets-ssm` alone. That policy sits in
`flows.yaml` as an object of its own, so a release that deletes it takes the
reach back whole and leaves every other flow as it was.
`ssm.*.amazonaws.com` admits a regional endpoint such as
`ssm.eu-central-1.amazonaws.com`, for the same reason as the STS pattern.

### How a running cluster takes a release's components

Talos creates each object of a manifest once, at the cluster's first build,
and never changes or deletes it. Talos v1.14.4 records each object it created
in an inventory and skips any object listed there
(`ManifestApplyController` in Talos's
[`manifest_apply.go`](https://github.com/siderolabs/talos/blob/v1.14.4/internal/app/machined/pkg/controllers/k8s/manifest_apply.go)).
A component a later release adds, changes or drops therefore reaches a
running cluster only another way. The platform never dials into a cluster, so the
block holds no credential for the cluster's API.

The `rollout` package takes that other way through the cluster repository.
The build installs one Argo CD Application of the cluster's own, named
`cluster`, which applies every manifest in the repository's `cluster/`
directory. This section calls the `rollout` package's work the carriage: a
block's commit of a release's components to that repository, for the
cluster's Argo CD to apply.

A block runs the carriage at each upgrade of a running cluster, the block's
operation that moves it to a newer kit release. It builds the release and
waits until `Plane.GatewaysServeRelease` reports that every gateway holds the
release's images. The gateways are the cluster's image mirror, so no component
then names an image the cluster cannot pull. The block commits after that.

`Plane.GatewaysAwaitRelease` tells the block whether the cluster has yet to
take the release. It reads true while some location's gateway was built from
an earlier list, whose digest its `mitstack.dev/gateway-list` label holds in
place of this release's. It also reads true for a cluster with no network and
for one built with a single gateway, since neither has taken the release. A
location that lost its gateway awaits nothing, since the plane builds its next
gateway from this release's list. Once it reads false, the carriage waits only
on a gateway of this list that is missing or does not serve yet, such as one
that fails its probe. `GatewaysServeRelease` holds only while
`GatewaysAwaitRelease` reads false, and both sort the gateways by one rule, so
the kit alone decides which gateway is current.

Two things are called a release here. A kit release is one tagged version of
this module, such as v0.47.0. A `rollout.Release` is the Go value that lists
the component manifests one kit release carries; the text writes it in code
form, and "release" alone means the kit release.

A component is one named manifest of the set: a `components.Manifest`, whose
`Name` is the label `components.Default` and `components.HostingServed` give
it where they build the set, such as `cilium`, `argocd` or `kyverno`. A component's name becomes part of its Application's name and of its handover
Job's, which is `component-<name>-handover`. That Job name must be a valid
Kubernetes name: at most 63 characters, made of
lowercase letters, digits and hyphens, and starting and ending with a letter
or digit. `Release.Files` refuses a name that is not.

A block builds a `rollout.Release` with `Plane.Release` in `controlplane` from
the same plane it builds the cluster with, and from the state `Plane.Read`
returns. It holds every manifest the build
applies, with two exceptions. It leaves out the hosting set's inline
manifests, since only an install's keys render them, and it leaves out the
`cluster` Application, since the carriage runs through that Application. The block then commits the files
`Release.Files` returns in one commit, and reads the result back with
`Release.ReadBack`:

```go
state, err := plane.Read(ctx)
release, err := plane.Release(state)
files, err := release.Files("acme/shop-cluster") // commit each file's Bytes at its Path, in one commit
retired, err := release.Retired(heldPaths)       // take each of these out in that same commit
reading, err := release.ReadBack(reportBody)     // what the newest node report says of the release
```

In the example, `"acme/shop-cluster"` is the cluster repository as GitHub
names it, `owner/name`: `Release.Files` builds each Application's source URL
from it and refuses any other form. `heldPaths` is every file path the cluster repository holds at
the commit the block builds on, as the block lists them from its checkout, and
`reportBody` is the JSON body of the newest node report the platform received
from the cluster. `ReadBack` returns a `rollout.Reading`, which the end of this
section describes.

`Plane.Release` also sets `Release.ToolsImage` to the tools image the kit
release published, which each component's handover Job runs. `Release.Files`
refuses a release whose tools image is not named by digest.

`Plane.Release` takes the state because a hosting cluster's Traefik publishes
the ingress load balancer's address, which only Hetzner holds. Each call
renders Traefik with the address the state gives, so a load balancer that was
replaced changes Traefik's file, and the block's next commit carries the new
address. While the state holds no ingress load balancer with a public
address, `Plane.Release` returns an error that wraps `ErrNoIngressAddress` and
names the location and the value the state holds, for example:

```text
the release's Traefik has no ingress address to publish: the state holds "" as the public address of the ingress load balancer in fsn1
```

A block that gets this error commits nothing. Where the load balancer is
missing, the plane's next step builds it. A cluster that hosts no platform runs no
Traefik, so its release needs no address.

`Release.Files` returns two files per component, with every manifest file ahead of every Application file in the list, so Argo CD never syncs an Application whose directory is empty:

| File | What it holds |
|---|---|
| `components/<name>/manifest.yaml` | The component's documents, then its handover Job, then a marker ConfigMap in `argocd` named `component-<name>-<digest>` |
| `cluster/component-<name>.yaml` | The Argo CD Application `component-<name>`, which applies `components/<name>/` |

The marker ConfigMap is how the read-back tells which release Argo CD
compared. Argo CD's list of what an Application applied names objects and no
Git revision. The marker's name carries the digest of the component's
documents and its handover Job: the first 12 hexadecimal characters of the
SHA-256 of the bytes that precede the marker in `manifest.yaml`. The name
therefore changes whenever the component does. It also changes with every kit
release, since the Job names the release's tools image, so each Application
syncs once per release and runs the Job of that release.

Some documents stay with the build and never reach the repository. A Secret
holding data stays, since the repository must never hold a key. A batch Job
stays, since it runs once and no later sync could change its pod template. A
Helm or Argo CD hook stays, since Argo CD would run it again on every sync.
The one Job and the one hook a file holds is the carriage's own handover Job,
which is meant to run at every sync. A
component whose every document stays with the build gets no files at all.
A document that stays is created once by Talos at the cluster's first build,
as the start of this section says, and no later release changes it on a
running cluster: a Secret or Job a release changes reaches only a cluster
built from that release.

Each component has an Application of its own because Argo CD's repo server,
the Argo CD service that reads a repository and renders the manifests an
Application applies, reads at most 10 MB of manifests from one directory by
default, the
`reposerver.max.combined.directory.manifests.size` setting. The hosting set's
33 manifests come to 14,088,023 bytes, and its largest, Kyverno's, to
5,858,501.

The `cluster` Application creates each `component-` Application, and each of
those applies its directory. Every one syncs by server-side apply with
`prune` and `selfHeal` on, unlimited retries, and Argo CD's resources
finalizer. In Argo CD's terms, `prune` deletes a live object once the
repository no longer holds it, and `selfHeal` puts back an object someone
changed in the cluster by hand. Server-side apply has the Kubernetes API
server merge each object's fields, where the default client-side apply
rewrites a last-applied annotation. A finalizer is a marker that makes
Kubernetes hold an object's deletion until its controller has cleaned up;
Argo CD's resources finalizer has it delete every object the Application
applied before the Application itself goes. The effects:

- Server-side apply writes no last-applied annotation. A resource
  definition, here a CustomResourceDefinition (the CRD of the `components`
  row), can be large enough that the annotation would push its object past
  the 262,144 bytes Kubernetes allows for annotations.
- The retries wait out a resource definition, again a CRD, that another
  component's Application has yet to apply. A component that uses a custom
  resource fails to sync until its definition exists, and a retry succeeds
  once it does. A retry takes the newest commit, which `retry.refresh` sets,
  so a later release still reaches a component whose sync keeps failing.
- The finalizer deletes a component's objects once a release drops the
  component. Those objects include its resource definitions, and Kubernetes
  then deletes every custom resource of those definitions too. Deleting a
  `component-` Application by hand does the same.

Argo CD applies over an object Talos created and tracks it from then on. A
release that changes a field's value changes the live object, and a release
that drops an object leads Argo CD to delete it. A field that a release drops
from an object goes once the component's handover has run, as
[the section on the handover](#how-an-object-talos-created-comes-to-hold-only-what-a-release-states)
explains.

`Release.Retired` takes a list of the paths the repository holds and names
each one the carriage wrote for an earlier release that this one no longer
states. It names only paths of the two shapes in the table whose component
name passes the Application-name rule above, and the two files `LegacyFiles`
lists. Any other file, such as an app's own manifest under `components/`,
stays.

`LegacyFiles` are `hosting/policies.yaml` and `cluster/mitstack-hosting.yaml`.
The hosting policies are the Cilium network policies of a cluster that hosts
the platform, which `components.HostingPolicies` returns as the manifest
`mitstack-hosting-flows`: it names the only network flows a hosting namespace's pods make or take. Kit
release v0.32.0 gave a running cluster no way to change them, because Talos
alone applied them at the build. Kit release v0.33.0
added the `rollout` package, and releases through v0.46.0 committed those two
files: `hosting/policies.yaml` held every hosting policy and a marker, and
`cluster/mitstack-hosting.yaml` was the Application that applied it. Kit release v0.47.0 adds
`component-mitstack-hosting-flows`, and the commit that lands it takes both
legacy files out. The `cluster` Application then prunes the Application
`mitstack-hosting`, which carries no finalizer, so the policies stay running.
`component-mitstack-hosting-flows` applies the same policies and tracks them
from then on, so the cluster keeps one copy of each, applied by one
Application.

The read-back comes from the node report, which lists each Argo CD
Application with the objects it applied. `Release.ReadBack` reads the release
back only when each component's Application reads Synced, which means the cluster matches the repository, and Healthy, which means its objects run, and
lists exactly the objects the release states for it, each Synced. Until
Argo CD compares the new release, its Application still lists the marker of
the release before, so the report reads nothing back. A report that would
reach 256 KiB leaves out every Application's objects, and reads nothing back
either. Argo CD lists a component's handover Job among the Application's
objects until the Job succeeds, and `ReadBack` reads nothing back while a
report lists one, naming the Job in its `Problem`.

`Release.ReadBack` returns a `rollout.Reading`. Its `Problem` says why a report
does not read the release back, and is empty once it does. Its `Applied` then
names each object read back, as its kind and its namespace and name, for
example:

```text
CiliumClusterwideNetworkPolicy mitstack-hosting-flows
ConfigMap argocd/component-mitstack-hosting-flows-3f9a1c0d2b7e
```

The tests in `rollout/rollout_test.go` build two clusters and run each through an upgrade to the release under
test. One is built from the `mitstack-hosting-flows` manifest as kit release
v0.32.0 shipped it, as Talos would apply it. The other is built from the
files kit release v0.46.0 committed, as Talos and Argo CD would. The cluster in `testutil/argocd.go` stands in for
the cluster's API and for Argo CD. It applies each Application's directory,
prunes what the directory no longer holds, and deletes an Application's
objects when the Application carrying the finalizer goes. It replaces an
object whole and tracks no field manager, so it shows a cluster as it stands
once each handover has run. The next section names the test that runs a
handover through a real API server.

### How an object Talos created comes to hold only what a release states

Kubernetes records, for each field of an object, which field manager set it.
A field manager is the name a client gives when it writes. Talos v1.14.4
creates each object of a manifest by server-side apply under the field manager
`talos`, and never applies that object again, as `ManifestApplyController` in
Talos's
[`manifest_apply.go`](https://github.com/siderolabs/talos/blob/v1.14.4/internal/app/machined/pkg/controllers/k8s/manifest_apply.go)
shows. Argo CD v3.5.5 applies the same object under the field manager
`argocd-controller`. Both managers then own every field that both wrote.

The API server removes a field from an object only once no field manager owns
it. A field or a list entry that a later release drops therefore stays on the
object for as long as Talos owns it. Argo CD still reads the object as Synced,
because it compares the object with the result its own apply would have, and
that apply changes nothing. For example, a cluster built on kit release
v0.53.0 created Cilium's agent with the privileged init container
`mount-bpf-fs`. A later release states the agent without that container, and
the agent keeps it until Talos gives the field up.

The handover is the step that has Talos give up its fields on the objects
Argo CD applies. It runs once per sync of a component:

1. `Release.Files` ends each component's file with a Job named
   `component-<name>-handover` in namespace `argocd`. The Job is an Argo CD
   PostSync hook. Argo CD runs such a document once a sync has applied the
   Application's objects and they read Healthy, and does not keep it applied.
2. The Job runs `rollout/scripts/handover.sh` from the release's tools image,
   under the service account of Argo CD's application controller. That account
   already applies every component, so the cluster gains no other identity
   that may change these objects.
3. The script reads each object the component states, with its field
   managers. For every object that both `talos` and `argocd-controller`
   applied, it sends one server-side apply under the name `talos` that states
   no field. Talos then owns nothing on the object, and the API server drops
   each field that only Talos owned. Every field Argo CD applies stays.
4. The script reads those objects again and fails when one still names
   `talos`.
5. Argo CD deletes a Job that succeeded. A Job that failed fails the sync,
   which Argo CD retries, and the Application lists the Job until it succeeds.

The script refuses an object that Talos created and Argo CD has not applied,
since giving up Talos's fields there would take every field away. It then
changes nothing and fails, naming the object. A document that stays with the
build, such as a Secret holding data, is never named to the script.

A handover changes a running cluster in two ways. Each object Talos created
loses the annotation `config.k8s.io/owning-inventory`, which only Talos set.
Talos decides what to create from its inventory ConfigMap
`kube-system/talos-bootstrap-manifests-inventory` and never from that
annotation, so it creates no object again. Each field that a release dropped
since the cluster's build also goes. Where that field sits in a pod template,
the workload rolls by its own update strategy. Cilium's agent, for example,
restarts on two nodes at a time, as its `maxUnavailable: 2` sets.

`TestAPIServer_AnObjectTalosCreatedKeepsAFieldAReleaseDropsUntilItsComponentsHandoverGivesUpTalossFields`
in `apiserver_handover_test.go` runs these steps through a kube-apiserver at
the Kubernetes release the kit pins. It creates Cilium from
`testdata/v0.53.0/cilium.yaml` as Talos does, applies it and then this
release's Cilium as Argo CD does, and reads `mount-bpf-fs` still in place. It
then runs the handover Job as Argo CD's application controller and reads the
container gone. `rollout/rollout_test.go` covers the script's refusals with a
fake kubectl.

### What the rendered components must look like for Argo CD to read them Synced

Argo CD compares each rendered document with the live object. With
`selfHeal` on, which makes Argo CD apply the document again whenever they
differ, a document that the cluster changes after it applies, or that the API
server stores differently from how it is written, never matches. Its
Application then reads OutOfSync, meaning the cluster differs from the
repository, or Degraded, meaning an object does not run, and the read-back of
a release, described in the previous section, never completes. The committed
manifests therefore say what the cluster ends up holding, and the kit adds no
`ignoreDifferences` entry. Three rules keep them so:

- Both of Istio's validation webhooks, `istiod-default-validator` and
  `istio-validator-istio-system`, render `failurePolicy: Fail`, set by
  `base.validationFailurePolicy` in `components/render/istio-base-values.yaml`
  and `istiod-values.yaml`. Istio's install guide requires this of any tool
  that renders with `helm template` and applies server-side, because istiod
  rewrites the field to Fail once it is ready.
- istiod renders no HorizontalPodAutoscaler and runs one replica, set by
  `autoscaleEnabled: false` and `replicaCount: 1` in
  `components/render/istiod-values.yaml`. No cluster the kit builds serves the
  metrics API an autoscaler reads, so the autoscaler would read Degraded.
- No document declares an empty `labels` or `annotations` map under its
  top-level `metadata`. The API server stores no empty map, and the render
  drops each one from every chart, as `drop_empty_metadata_maps` in
  `scripts/lib/empty-maps.sh` does. A `labels` key anywhere deeper, such as in
  a CustomResourceDefinition's schema or a pod template, is left as it is.
  The Kyverno policy definitions and ztunnel's objects are the charts that
  emit such maps today.

`rendered_set_test.go` checks the three rules over every manifest the kit
renders, and the render's own step against documents that hold each kind of
map.

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

A token that logged in with a service account in namespace `app-shop` can then
read only `kv/data/apps/app-shop/*`. The script writes the policy
`app-secrets-writer` on the same template, which may create, read and update
that one path and its metadata. External Secrets' PushSecret reads both before
it writes them. Neither policy may delete, and neither reaches another
namespace's path.

The script then writes one Kubernetes auth role for each policy. Each role lets
the service account of the same name log in from any namespace named `app-*`,
which is every slice's namespace:

| Role | Who may log in | Its policy | Token lifetime |
|---|---|---|---|
| `app-secrets-reader` | The service account `app-secrets-reader` in any `app-*` namespace | `app-secrets-reader` | 15 minutes |
| `app-secrets-writer` | The service account `app-secrets-writer` in any `app-*` namespace | `app-secrets-writer` | 5 minutes |

A slice's SecretStore for reading names the role and service account
`app-secrets-reader`, and the one its PushSecret writes through names
`app-secrets-writer`. A slice therefore needs no policy or role written after
the init, and a new slice needs nothing written at all. Anyone who can create a
namespace named `app-*` and a service account of either name in it can log in
to that namespace's own path, so creating such namespaces stays with the block
that renders slices.

The root token lives in the script's memory alone. The script hands curl each
header that carries a token on curl's standard input, through the shell's
builtin `printf`, and never prints a token. The script retries each step while
its pod lives. A pod lost after the commit may leave the mount, the auth method,
its configuration, a policy or a role unwritten, and a new pod finds the file
and changes nothing. The owner then decrypts the root token from
`openbao-init.asc` and writes whichever is missing.

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
| [OpenBao: Kubernetes auth API](https://openbao.org/api-docs/auth/kubernetes/) and [its login check at v2.7.1](https://github.com/openbao/openbao/blob/v2.7.1/internal/builtin/credential/kubernetes/path_login.go#L361-L369) | A role binds service account names and namespaces, and a login matches each against the role's list as a glob, so `app-*` admits every slice namespace |
| [External Secrets: PushSecret to Vault at v2.11.0](https://github.com/external-secrets/external-secrets/blob/v2.11.0/providers/v1/vault/client_push.go) | A push reads the path and its metadata, then writes the metadata and the path |
| [OpenBao: `/sys/auth`](https://openbao.org/api-docs/system/auth/) and [`/sys/mounts`](https://openbao.org/api-docs/system/mounts/) | Each listing names a method by its path, with its `accessor`, and `options.version` set to `"2"` mounts KV v2 |
| [OpenBao: KV v2](https://openbao.org/docs/secrets/kv/kv-v2/) | A policy names a secret under `data/` and its listing under `metadata/` |
| [OpenBao: Kubernetes auth](https://openbao.org/docs/auth/kubernetes/) and [its API](https://openbao.org/api-docs/auth/kubernetes/) | `kubernetes_host` is required, a pod's own token and CA serve for the review, and a login's alias carries `service_account_namespace` |
| [OpenBao: templated policies](https://openbao.org/docs/concepts/policies/#templated-policies) and [`/sys/policies/acl`](https://openbao.org/api-docs/system/policies/) | A path may name `identity.entity.aliases.<mount accessor>.metadata.<key>` |
| [GitHub: managing deploy keys](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/managing-deploy-keys#deploy-keys) | A deploy key reaches one repository, is read-only unless granted write access, and goes with that repository |
| [GitHub: using SSH over the HTTPS port](https://docs.github.com/en/authentication/troubleshooting-ssh/using-ssh-over-the-https-port) | `ssh.github.com` takes SSH on port 443, at `ssh://git@ssh.github.com:443/<owner>/<repo>.git` |
| [Argo CD: private repositories](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/) and [declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#repositories) | A Secret labelled `argocd.argoproj.io/secret-type: repository` carries `type`, `url` and `sshPrivateKey`, and SSH host keys come from `argocd-ssh-known-hosts-cm` |
| [Argo CD: webhook notifications](https://argo-cd.readthedocs.io/en/stable/operator-manual/notifications/services/webhook/) and [notifications-engine's `replaceServiceConfigSecrets`](https://github.com/argoproj/notifications-engine/blob/0cff13b8a717/pkg/api/config.go) | A service's every string takes each `$key` from `argocd-notifications-secret`, which is how the webhook's URL and bearer come from the enrolment |
| [Argo CD Helm chart 10.10.5: `notifications`](https://github.com/argoproj/argo-helm/blob/argo-cd-10.10.5/charts/argo-cd/values.yaml) | `notifiers`, `templates`, `triggers` and `subscriptions` fill `argocd-notifications-cm`, and `secret.create: false` leaves `argocd-notifications-secret` to another writer |
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
| [cert-manager: Issuer configuration](https://cert-manager.io/docs/configuration/) | A `ClusterIssuer` reads its Secrets in the cluster resource namespace, `cert-manager` unless `--cluster-resource-namespace` names another |
| [trust-manager](https://cert-manager.io/docs/trust/trust-manager/) | A `Bundle` reads Secrets in its trust namespace and writes a ConfigMap to every namespace |
| [CloudNativePG: certificates](https://cloudnative-pg.io/documentation/current/certificates/) | `serverTLSSecret` and `serverCASecret` may name one cert-manager Secret, and the label `cnpg.io/reload` reloads it on renewal |
| [Kyverno: ValidatingPolicy](https://kyverno.io/docs/policy-types/validating-policy/) | A CEL validation reads the request's namespace as `namespaceObject` |
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
removes it, and only then deletes the Node. It renders the Job only when
block-kit's redundancy guard finds a majority of the members serving without
the lost one, by the etcd members `RemoveMember.Report` reads healthy. The block
passes the newest node report as a `report.Read` and the time the replacement
began as `StepStart`, and a report read before then counts for no member. A
refusal renders nothing and names each member with its reading. Its Talos identity holds
`os:admin`, the only role Talos accepts for a member removal, and its
Kubernetes identity may delete that one Node.

A restore onto new servers runs the gateway's script with `--recover-from`. It
bootstraps etcd from the snapshot, waits for the API server, and deletes every
Node object that `--keep` does not name. The fourth proof's restore stalled
until the old Node objects went, because Argo CD's controller stayed bound to
a dead server's Node.

## How a block takes node pools

A block is the unit of infrastructure an app subscribes to, and a Talos cluster
block builds on this module. The `pools` package, at `pools/` in this
repository, lets such a block offer node pools without writing its own pool
logic. A node pool is a set of a cluster's worker nodes that one group of
settings describes. A dedicated pool lists Hetzner Robot servers the org
already rents, each with the Robot account it is rented under, and it changes
only when the block is upgraded. A Cloud pool names Hetzner Cloud server types
in order of preference, locations, and a minimum and a maximum number of
servers. Until the pool scales itself, the two bounds must be equal, so a
Cloud pool has a fixed size, and `Parse` refuses unequal bounds.
[ADR 0118](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0118-a-talos-cluster-block-grows-by-node-pools-from-talos-cluster-kit-and-a-manifest-declares-a-pools-servers-as-one-resource-entry-marked-many.md)
sets these rules.

`pools.Parse` is what a block calls first. It takes the settings the platform
passes the block as strings: `dedicatedPool` is a JSON list of objects with a
`serverNumber` and a `robotAccount`, the name of the Robot account's instance
on the platform, and the four `cloudPool` settings are two comma-separated
lists and two numbers. `Parse` answers every setting that breaks its rule in
one error. It refuses a location other than `fsn1`, `nbg1` and `hel1`, a server
type outside `controlplane.ServerTypes`, a negative or missing size, and a
dedicated entry with no server number, no Robot account, or a server number
listed twice.

```go
p, err := pools.Parse(map[string]string{
	"dedicatedPool":        `[{"serverNumber": "3041839", "robotAccount": "inst_robot_main"}]`,
	"cloudPoolServerTypes": "cpx32, cx43",
	"cloudPoolLocations":   "fsn1, nbg1",
	"cloudPoolMin":         "2",
	"cloudPoolMax":         "2",
})
if err != nil {
	fmt.Println(err)
	return
}
fmt.Println(len(p.Dedicated), "dedicated server and", p.Cloud.Max, "Cloud servers over", p.Cloud.Locations)
// Output: 1 dedicated server and 2 Cloud servers over [fsn1 nbg1]
```

A block calls `pools.PlanCloud` on each subscribe or upgrade, and again on
each later run until the plan is empty. The function reads the servers Hetzner
lists with the subscription's labels and the role `cloud-worker`, and answers a
`CloudPlan` of what the pool lacks:

- `Create` lists servers to create. They boot from the worker image the install
  keeps, with no user data, on the cluster's network and firewall, each of the
  first listed server type its location offers. A server is created only while
  the pool, a member that is leaving included, holds fewer than its size, and
  in the listed location holding the fewest members.
- `Mark` lists the member whose removal begins, labelled with the time it
  began, and each member the pool keeps again with those labels taken off, so
  a later removal judges only readings from after it began. A member leaves when the pool shrinks, which takes the newest member
  of the location holding the most, or when the list drops its server type or
  location, which a new server then replaces. One member leaves at a time.
- `Drain` lists that member once block-kit's redundancy guard allows it, on the
  newest node report `Plane.ReadReport` returns from after its removal began.
  Every other member of the pool must read Ready, unless it is the last. When
  an OpenBao member's pod runs on it, a majority of OpenBao's members must read
  initialized and unsealed without that one. The block drains each such node
  itself.
- `Held` names why that member is not drained yet: the guard's refusal, naming
  each member it read, or that no node report came since the removal began.
  The block records it as an event, and its next call plans again.
- `Delete` lists the members whose node has left the cluster, which the block
  reports through the `drained` function it passes `PlanCloud`. A server is
  deleted only once its node has drained.

`CloudPlan.Apply` creates, labels and deletes through the `controlplane` API,
and no other server of the cluster is touched.

`Dedicated.RenderWorker` renders a dedicated worker's Talos machine
configuration from the cluster's secrets. The server boots Talos's image for
bare-metal machines, so the render uses the Metal profile, names the node
`robot-` and its server number, and joins it to the cluster's private network
through the server's network card on the vSwitch, Hetzner's private network for
dedicated servers, which the block read from Robot. A worker's configuration
holds no etcd authority key, the key that lets a node issue etcd certificates
and that only a control-plane node holds. The block makes the Robot calls that
adopt, install, firewall and release a server, since
[ADR 0103](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0103-mitstack-offers-five-hetzner-capacity-blocks-and-keeps-the-k3s-block.md)
keeps Robot API code out of this module.

## How a release is versioned

A block takes a change only by requiring a newer tag of this module in its
`go.mod`, as `require github.com/mitstack-eu/talos-cluster-kit v0.1.0`. The
file `.github/version` names the next release. Once a pull request merges and
every CI job passes on `main`, the `release` job tags that commit `v` followed
by the version, unless the tag exists already. The `record delivery` job then records on Plain, mitstack's board, the issues
the push's commits deliver. A commit delivers an issue when its subject opens
with the key and a colon, as in `MIT-123: fix the thing`. A subject that ends
with `(refs MIT-123)` only names the issue, and the job records nothing for it.

A release also publishes the tools image, which the kit's Jobs and CronJobs
run in. The image carries the report's, its outside probe's, the labeller's,
the cluster tokens' and the handover's scripts as the release's commit holds
them, so a cluster runs the scripts of the release it was built from. The `release` job
pushes it to the public package
`ghcr.io/mitstack-eu/talos-cluster-kit-tools`, signs its digest keyless as
this repository's CI on `main`, and refuses to tag unless a client with no
login can pull it. An image's digest is known only once it is built, so it
cannot sit in the commit the image is built from. The job therefore tags a
commit on top of the merged one, changing only the line of `tools/tools.go`
that sets `tools.Image` to the pushed reference. On `main`, `tools.Image`
names no published image, with the tag `unreleased` and a digest of zeros. A
block reads it from the tag its `go.mod` requires, so a `go.mod` bump alone
moves its tools image.

The tools image also carries every manifest a gateway serves, at the path
`tools.ManifestPath` gives for its repository path. Each gateway's mirror
copies the tools image by digest like any other image, and the gateway then
copies talosctl, kubectl and those manifests out of it. It refuses a binary
whose sha256 differs from `tools/tools.env`, and a manifest whose sha256
differs from the one the kit renders. A gateway's build therefore fetches
nothing from outside the registries its mirror copies from, so an outage of
GitHub's release downloads, of `raw.githubusercontent.com` or of `dl.k8s.io`
cannot stop it.

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
