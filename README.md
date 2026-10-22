# talos-cluster-kit

This repository holds the Talos install that mitstack's two Hetzner cluster
blocks share. It is for an engineer who changes what a Talos node boots, what
its cluster starts with, or how etcd recovers. A block is a unit of
infrastructure an app subscribes to, as
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines. Two blocks import this module: hcloud-talos-cluster on Hetzner Cloud
servers and hrobot-talos-cluster on Hetzner dedicated servers.

Talos Linux is an operating system that runs only Kubernetes and is managed
through an API, with no shell and no SSH. The module follows the fourth Talos
proof of 19 October 2026, recorded in the workspace note
[Talos on Hetzner Cloud from an embedded configuration](https://github.com/mitstack-eu/mitstack-workspace/blob/main/product/knowledge/2026-10-19-talos-on-hetzner-cloud-from-an-embedded-configuration.md),
and the design that
[ADR 0095](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0095-mitstack-clusters-run-talos-across-fsn1-nbg1-and-hel1-with-a-load-balancer-per-location.md)
accepts.

The module renders and the block acts. Nothing here calls Hetzner, GitHub or
the platform: each function takes its inputs as values and returns bytes, or
runs one local command. The block decides when to render, mints the secrets
that go in, and creates the servers. Keeping the install in one module means a
fix to the boot or to etcd recovery lands once and reaches both blocks.

## What a cluster is built from

A node boots from a disk image that embeds its whole machine configuration, so
the server needs no user data. The nodes have no public address. A gateway in
each location is the cluster's only server with one: it forwards the nodes'
DNS, NTP and HTTPS, records that traffic, and serves the image mirror and the
larger manifests over TLS on the private network. The table names the parts in
the order a block uses them.

| Package | What it does | Main entry points |
|---|---|---|
| `components` | Holds the pinned component set: Cilium, the custom resource definition (CRD) of Cilium's cluster-wide policy, Argo CD, the policy that keeps pods from Hetzner's metadata service at `169.254.169.254`, and the node-local OpenTelemetry Collector | `Default`, `Set.Images`, `LogCollector`, `ParseImage` |
| `nodeconfig` | Renders a control-plane node's machine configuration with Talos's own `machinery` module and validates it against Talos's schema | `Render`, `Cluster`, `Node`, `NewSecrets`, `ParseSecrets`, `Secrets.AdminTalosconfig` |
| `image` | Runs imager, Sidero Labs' open-source image builder, with the `hcloud` or the `metal` profile, and discards its output | `Run`, `Build` |
| `mirror` | Lists every image the nodes pull, each by digest | `List`, `Render` |
| `gateway` | Renders the cloud-config of the gateway in each location, and mints the cluster's gateway CA and each gateway's certificate | `Render`, `Inputs`, `NewCA`, `CA.Issue`, `ReleaseURL` |
| `etcd` | Renders the Job that removes a lost node's etcd member and then its Node object, and holds the gateway's script that bootstraps etcd empty or from a snapshot | `RemoveMemberJob`, `BootstrapScript`, `Bootstrap.Args` |
| `report` | Renders the node report, a CronJob that posts each node, its Talos version, the etcd membership and Argo CD's health to the platform | `Manifests` |
| `tools` | Holds `tools/tools.env`, which pins the talosctl and kubectl that the tools image and the gateway both install, and names the tools image a release published | `Env`, `Pin`, `Image` |
| `testutil` | Holds the fixtures, fakes and decoders this module's tests and a block's tests share | `NewCluster`, `NewTools`, `Golden` |

For example, this renders the configuration a Cloud cluster's image embeds,
for a cluster with one location:

```go
cfg, err := nodeconfig.Render(nodeconfig.Cluster{
	Name:           "shop",
	SubscriptionID: "sub_0123456789ab",
	Profile:        nodeconfig.HCloud,
	Components:     components.Default(),
	Secrets:        secrets, // from nodeconfig.NewSecrets(time.Now())
	Network:        netip.MustParsePrefix("10.0.0.0/16"),
	APIAddresses:   []netip.Addr{netip.MustParseAddr("10.0.0.3")},
	Gateways:       []netip.Addr{netip.MustParseAddr("10.0.0.2")},
	GatewayCA:      gatewayCA.Cert, // from gateway.NewCA
	Logs:           components.LogExport{Endpoint: "otlp.mitstack.dev:4317", CA: receiverCA}, // the log receiver's CA
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

## What the configuration holds

`nodeconfig.Render` starts from the configuration Talos itself generates for a
control-plane node and changes it in these ways:

| Concern | What the rendered configuration does | Where |
|---|---|---|
| DNS and time | Every node asks Hetzner's resolvers `185.12.64.1` and `185.12.64.2` and syncs from `ntp1.hetzner.de`, `ntp2.hetzner.com` and `ntp3.hetzner.net`, and Sidero's discovery service is off | `resolverDocument`, `timeSyncDocument` in `nodeconfig/documents.go` |
| Images | `docker.io`, `ghcr.io`, `public.ecr.aws`, `quay.io` and `registry.k8s.io` go to every gateway's mirror in turn, with no fallback to the public registry | `mirrorDocuments` |
| Components | Cilium, its policy CRD and Argo CD come from the first gateway over TLS, and the metadata policy and the Collector sit in the configuration | `manifestDocuments` |
| Logs | Talos sends service and kernel logs to the node-local Collector on `127.0.0.1`, which exports them over OTLP with TLS verified. A cluster whose `Logs` is the zero `LogExport` runs no Collector and keeps its logs on the node | `adaptV1Alpha1`, `kernelLogDocument`, `components.LogCollector` |
| Networking | Flannel and kube-proxy are off, since Cilium replaces both, and etcd and the kubelet bind to the private network | `adapt` |
| Default route | A Cloud node keeps DHCP on `eth0` and routes everything outside the network through Hetzner's router at the network's first address. The block's network route of `0.0.0.0/0` then carries that traffic to the gateway. Hetzner's DHCP gives the node no default route, and Talos runs no default DHCP on a link that has a `LinkConfig` | `routeDocuments` |
| Talos API from pods | Only pods in `mitstack-node` may hold a Talos identity, with `os:admin`, `os:etcd:backup` or `os:reader` | `talosAPIAccessDocument` |

The configuration holds every key of the cluster, its certificate
authorities included, but no admin client certificate. Talos's etcd
bootstrap accepts only `os:admin`, its full-rights role, so the gateway runs
it once with a short-lived talosconfig from `Secrets.AdminTalosconfig` and
deletes that file on exit.

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
