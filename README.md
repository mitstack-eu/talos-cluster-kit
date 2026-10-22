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
| `components` | Holds the pinned component set: Cilium, the custom resource definition (CRD) of Cilium's cluster-wide policy, Argo CD, the policy that keeps pods from Hetzner's metadata service at `169.254.169.254`, Kyverno with its two admission policies, the CloudNativePG operator with the database-slice policy, and the node-local OpenTelemetry Collector. It also holds the boot set of a cluster that hosts the platform | `Default`, `Hosting`, `Set.Images`, `LogCollector`, `ParseImage` |
| `nodeconfig` | Renders a control-plane node's machine configuration with Talos's own `machinery` module and validates it against Talos's schema | `Render`, `Cluster`, `Node`, `NewSecrets`, `ParseSecrets`, `Secrets.AdminTalosconfig` |
| `image` | Runs imager, Sidero Labs' open-source image builder, with the `hcloud` or the `metal` profile, and discards its output | `Run`, `Build` |
| `mirror` | Lists every image the nodes pull, each by digest | `List`, `Render` |
| `gateway` | Renders the cloud-config of the gateway in each location, and mints the cluster's gateway CA and each gateway's certificate | `Render`, `Inputs`, `NewCA`, `CA.Issue`, `ReleaseURL` |
| `etcd` | Renders the Job that removes a lost node's etcd member and then its Node object, and holds the gateway's script that bootstraps etcd empty or from a snapshot | `RemoveMemberJob`, `BootstrapScript`, `Bootstrap.Args` |
| `report` | Renders the node report, a CronJob that posts each node, its Talos version, the etcd membership, Argo CD's health and CloudNativePG's clusters, databases and roles to the platform | `Manifests` |
| `openbaoinit` | Renders OpenBao's init Job, which the hosting set carries: it initialises a hosting cluster's OpenBao once, commits the root token and recovery key to the cluster repository as PGP messages, then writes the policy each slice's External Secrets reads with | `Manifests`, `Inputs`, `InitFile` |
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
| Components | Cilium, its policy CRD, Argo CD, Kyverno and the CloudNativePG operator come from the first gateway over TLS. The metadata policy, the two admission policies, the `cnpg-system` namespace, the database-slice policy and the Collector sit in the configuration | `manifestDocuments` |
| Logs | Talos sends service and kernel logs to the node-local Collector on `127.0.0.1`, which exports them over OTLP with TLS verified. A cluster whose `Logs` is the zero `LogExport` runs no Collector and keeps its logs on the node | `adaptV1Alpha1`, `kernelLogDocument`, `components.LogCollector` |
| Networking | Flannel and kube-proxy are off, since Cilium replaces both, and etcd and the kubelet bind to the private network | `adapt` |
| Default route | A Cloud node keeps DHCP on `eth0` and routes everything outside the network through Hetzner's router at the network's first address. The block's network route of `0.0.0.0/0` then carries that traffic to the gateway. Hetzner's DHCP gives the node no default route, and Talos runs no default DHCP on a link that has a `LinkConfig` | `routeDocuments` |
| Talos API from pods | Only pods in `mitstack-node` may hold a Talos identity, with `os:admin`, `os:etcd:backup` or `os:reader` | `talosAPIAccessDocument` |
| User volumes | Each user volume the set names, such as OpenBao's in the hosting set, is a directory Talos mounts at `/var/mnt/<name>` | `userVolumeDocuments` |

The configuration holds every key of the cluster, its certificate
authorities included, but no admin client certificate. Talos's etcd
bootstrap accepts only `os:admin`, its full-rights role, so the gateway runs
it once with a short-lived talosconfig from `Secrets.AdminTalosconfig` and
deletes that file on exit.

## How a cluster isolates each app

Every cluster runs Kyverno, an admission controller that checks each object
the API server is asked to create. Two of its policies apply to every
namespace outside the system namespaces, which are `kube-system`,
`kube-public`, `kube-node-lease`, `argocd`, `kyverno`, `cnpg-system` and
`mitstack-node`.

| Policy | What it does | File |
|---|---|---|
| `mitstack-require-app-label` | Refuses a pod without a non-empty `mitstack.dev/app` label, which an app's network policies select its pods by | `components/admission/policies/mitstack-require-app-label.yaml` |
| `mitstack-generate-default-deny` | Gives each namespace, new or existing, the NetworkPolicy `mitstack-default-deny`, which blocks every connection its pods make or take until an app's own policy allows it | `components/admission/policies/mitstack-generate-default-deny.yaml` |

Kyverno's webhooks skip the same system namespaces, so a Kyverno outage
cannot stop a system pod, such as the node report in `mitstack-node`.

The CloudNativePG operator in `cnpg-system` runs the Postgres clusters an app
keeps its data in. A namespace labelled `mitstack.dev/database-slice=true` is
a database slice. The Cilium policy `mitstack-database-slices` lets that
slice's Postgres instances reach the API server and each other, and admits
the operator on port 8000 and every app slice on port 5432. A pod in any other
namespace gains none of those allows.

The CloudNativePG manifests are a byte-for-byte copy of release v0.4.0 of
mitstack-eu/cluster-bases, the repository of pinned third-party manifests.
Argo CD's built-in health check reads a Postgres switchover, failover,
upgrade or configuration change as Degraded. The kit's `argocd-cm` reads each
of those as Progressing, since CloudNativePG passes through them on its own.

## How a cluster that hosts the platform boots

A cluster that runs mitstack's own control plane must start its secret store,
External Secrets and its web entry point before Argo CD or the platform runs.
`components.Hosting` returns that boot set. It is the default set with six
more charts and the manifests that join them, and a block opts in by passing
it as `Cluster.Components`. A cluster built from `components.Default` renders
the same bytes it always did, and the golden files in
`nodeconfig/testdata/golden/` and `gateway/testdata/golden/` prove it.

The block mints the inputs and the kit checks them. `Hosting` refuses a seal
key that is not 32 bytes, a certificate of another key, an App key not sealed
to that key pair, and anything but three distinct node names. It also refuses
init inputs that `openbaoinit.Manifests` refuses: a tools image without a
digest, a repository that is not `owner/name`, an App or installation id that
is not a number, and a recipient that is not one armored public key able to
encrypt.

```go
set, err := components.Hosting(components.HostingInputs{
	SealKey:              sealKey,      // 32 random bytes
	SealedSecretsKeyPEM:  keyPEM,       // the RSA key Sealed Secrets opens with, PKCS #1 PEM
	SealedSecretsCertPEM: certPEM,      // its self-signed certificate, which the block seals to
	SealedAppKey:         sealedAppKey, // the SealedSecret github-app-key in mitstack-node
	Nodes:                []string{"shop-node-fsn1", "shop-node-nbg1", "shop-node-hel1"},
	Init: openbaoinit.Inputs{
		ToolsImage:     tools.Image, // the tools image this release published
		Repository:     "mitstack-eu/shop-cluster",
		AppID:          "1234567",   // the hosting GitHub App
		InstallationID: "89012345",  // its installation on the repository
		Recipient:      recipient,   // the armored public key behind openbaoSealKeyRecipient
	},
})
```

### What the hosting set runs

Each chart renders from a version pinned with the sha256 its repository's
`index.yaml` publishes. Every image is named by digest, and `mirror.List`
returns each one for the gateway's mirror.

| Component | What it does here | Values |
|---|---|---|
| OpenBao 2.7.1 | Three Raft members, one per node. The listener serves TLS 1.3 only, with the certificate cert-manager issues, and reloads it when it is renewed. The static seal reads its key from `/openbao/seal/key` | `components/render/openbao-values.yaml` |
| cert-manager v1.21.2 | Issues OpenBao's serving certificate from OpenBao's own authority, and the External Secrets webhook's certificate | `components/render/cert-manager-values.yaml` |
| trust-manager v0.25.0 | Copies that authority into the ConfigMap `openbao-ca`, under `ca.crt`, in every namespace | `components/render/trust-manager-values.yaml` |
| External Secrets v2.11.0 | Copies values from OpenBao into Kubernetes Secrets | `components/render/external-secrets-values.yaml` |
| Sealed Secrets 0.40.0 | Opens a `SealedSecret` with the block's key pair, and never mints or renews a key | `components/render/sealed-secrets-values.yaml` |
| Traefik v3.7.13 | Serves web traffic on NodePorts 30080 and 30081, with one pod on every node | `components/render/traefik-values.yaml` |

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
| `mitstack-sealed-app-key` | The `SealedSecret` that opens to the hosting GitHub App's key | `components/hosting/sealed-app-key.yaml` |
| `mitstack-openbao-storage` | OpenBao's StorageClass and the local volume of each member | `components/hosting/storage.yaml` |
| `mitstack-openbao-init` | OpenBao's init Job, and the ConfigMap with the recipient's public key. It holds no secret, and sits inline because it names this cluster's repository | `openbaoinit/openbao-init.yaml` |

Talos creates each object of a manifest once and never edits it afterwards. A
new seal key or key pair therefore reaches a running cluster only through
OpenBao's and Sealed Secrets' own rotation, and never through a new render.

### Where OpenBao keeps its data

Each node keeps its member's Raft data on the Talos user volume `openbao`,
which Talos mounts at `/var/mnt/openbao`. A Cloud server has one disk, and the
partition Talos calls `EPHEMERAL` spans all of it. The volume is therefore a
directory on `EPHEMERAL`, which needs no free space on the disk.

OpenBao starts before any storage provisioner could run. The StorageClass
`openbao-local` provisions nothing, and each member's claim binds a local
volume that is pinned to one named node, so a member keeps its node and its
data. Talos documents that a node's local data is lost with its machine, which
is why OpenBao runs three members and Raft carries the data.

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
| OpenBao's init Job | Lookups through Cilium's DNS proxy, calls to OpenBao on 8200, and calls to `api.github.com` on 443, which the gateway forwards |
| Traefik | Web traffic from outside the cluster on its two ports, calls to the API server, and calls to the app slices |

### How OpenBao is initialised

OpenBao starts uninitialised, and its root token and recovery key must reach
the owner without anyone typing them or opening a route into the cluster. The
init Job does that through Git. It runs `openbaoinit/scripts/openbao-init.sh`
in the tools image, in `mitstack-node`, because that is where the App key's
`SealedSecret` opens. The pod mounts the Secret that opens to, and Kubernetes
starts none of its containers until that Secret exists.

The script talks to `openbao-0`, the member OpenBao's Helm guide initialises,
so a member that has not yet joined Raft is never initialised as a second
store. Each request verifies OpenBao against the cluster's own authority. The
script first encrypts a probe to the recipient key, and stops before it sends
anything when gpg cannot encrypt to it. It then reads the store's state and the
repository's `openbao-init.asc`, with an App token that may only read.

| OpenBao | `openbao-init.asc` | What the Job does |
|---|---|---|
| Uninitialised | Absent | Initialises, commits the file, then sets OpenBao up |
| Initialised | Committed | Changes nothing and exits 0 |
| Initialised | Absent | Exits 1 and says the root token and recovery key are lost |
| Uninitialised | Committed | Exits 1 before the init, since the file may open another store |

The init asks for one recovery share with a threshold of one, and passes the
recipient's binary key as `recovery_pgp_keys`. OpenBao returns the recovery key
as a binary PGP message, which the script armors without the CRC24 footer that
RFC 9580 says not to add. The script encrypts the root token to the same
recipient with gpg. It commits both as `openbao-init.asc` with an App token
that may only write contents, before any request carries the root token:

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

With the root token, the script enables the KV v2 mount `kv` and Kubernetes
auth, and points that auth method at the API server. It then reads the method's
accessor from `GET /v1/sys/auth`, because the accessor exists only once the
method is enabled. It writes the read-only policy `app-secrets-reader` with
that accessor in place, as in this example:

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

The root token lives in the script's memory alone. curl reads each header that
carries a token from its standard input, so no token appears in a process's
arguments, and the script never prints one. The script retries each step while
its pod lives. A pod lost after the commit leaves the setup unfinished, and a
new pod finds the file and changes nothing. The owner then holds the root
token in `openbao-init.asc` to finish it.

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
| [GitHub: create or update file contents](https://docs.github.com/en/rest/repos/contents#create-or-update-file-contents) | `PUT` with a base64 `content` creates a file, 201 answers a new one, and a `GET` with `application/vnd.github.raw+json` returns the file as it is |
| [GitHub: create an installation access token](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app) and [the App's JWT](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app) | A token lasts one hour, `repositories` and `permissions` narrow it, and the RS256 JWT lives at most ten minutes |
| [Kubernetes: Secrets](https://kubernetes.io/docs/concepts/configuration/secret/#restriction-secret-must-exist) | No container of a pod starts until every Secret it requires is available |
| [RFC 9580 section 6.1](https://www.rfc-editor.org/rfc/rfc9580#section-6.1) | ASCII armor should carry no CRC24 footer |
| [GnuPG: key-related options](https://www.gnupg.org/documentation/manuals/gnupg/GPG-Key-related-Options.html) and [commands](https://www.gnupg.org/documentation/manuals/gnupg/Operational-GPG-Commands.html) | `--recipient-file` encrypts to the one key in a file, and `--dearmor` unpacks an armored key |
| [Cilium: DNS-based policy](https://docs.cilium.io/en/v1.19/security/policy/layer3/#dns-based) | `toFQDNs` learns a name's addresses from DNS answers that pass Cilium's DNS proxy |
| [curl: `--header`](https://curl.se/docs/manpage.html#-H) | `-H @-` reads headers from standard input |
| [cert-manager: CA](https://cert-manager.io/docs/configuration/ca/) and [SelfSigned](https://cert-manager.io/docs/configuration/selfsigned/) issuers | A self-signed root bootstraps the CA issuer |
| [trust-manager](https://cert-manager.io/docs/trust/trust-manager/) | A `Bundle` reads Secrets in its trust namespace and writes a ConfigMap to every namespace |
| [External Secrets: HashiCorp Vault provider](https://external-secrets.io/latest/provider/hashicorp-vault/) | A private CA is named through `caProvider` or `caBundle` |
| [Sealed Secrets: bring your own certificates](https://github.com/bitnami/sealed-secrets/blob/main/docs/bring-your-own-certificates.md) | The controller uses each key pair in its namespace that carries the active label |
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
