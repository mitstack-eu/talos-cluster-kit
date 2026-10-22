# Developing the Talos cluster kit

This page is for anyone changing talos-cluster-kit: where the component set
comes from, where each script lives, how its tests run it, and what CI proves
before a release is tagged. Every path below is relative to the repository
root.

Back to [the README](../README.md).

## Where the component set comes from

The component set is the manifests every Talos cluster starts with.
The table lists its files. `scripts/render-components.sh` writes the first
four from pinned sources, each checked against a published sha256, and refuses
any image that is not named by digest. The CloudNativePG files are copied from
a release, and the rest are written by hand. The hosting set's files follow in
a second table.

| File | Source | What it runs |
|---|---|---|
| `components/cilium/cilium.yaml` | The cilium 1.19.9 chart with `components/render/cilium-values.yaml` | Cilium as the network plugin and in place of kube-proxy, with Hubble off so the chart mints no key |
| `components/cilium/policy-crd.yaml` | Cilium v1.19.9's `CiliumClusterwideNetworkPolicy` CRD | The CRD the metadata policy needs before Cilium's operator starts |
| `components/argocd/argocd.yaml` | The argo-cd 10.10.5 chart with `components/render/argocd-values.yaml`, Dex off | Argo CD v3.5.5 in namespace `argocd`, with a health check that reads a CloudNativePG Cluster passing through a switchover, failover, upgrade or configuration change as Progressing |
| `components/kyverno/kyverno.yaml` | The kyverno 3.9.1 chart with `components/render/kyverno-values.yaml`, its tests and Helm hooks off | Kyverno v1.19.1 in namespace `kyverno`, its images from `ghcr.io` and its webhooks skipping the system namespaces |
| `components/cloudnative-pg/cloudnative-pg.yaml`, `components/cloudnative-pg/namespace.yaml` | Copied from mitstack-eu/cluster-bases v0.4.0 | The CloudNativePG 1.30.1 operator in namespace `cnpg-system`, with its default Postgres and PgBouncer images by digest |
| `components/admission/policies/` | Written by hand | The Kyverno policies that require the `mitstack.dev/app` label and give each app namespace `mitstack-default-deny` |
| `components/network-policies/mitstack-database-slices.yaml` | Written by hand | The Cilium policy that opens a database slice's Postgres instances to each other, the operator and the app slices |
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
| `components/traefik/traefik.yaml` | The traefik 41.6.1 chart with `components/render/traefik-values.yaml` | Traefik on every node in namespace `traefik`, on NodePorts 30080 and 30081 |
| `components/hosting/tls.yaml`, `components/hosting/flows.yaml` | Written by hand | OpenBao's authority and certificate, its trust bundle, and the flows the hosting namespaces allow |
| `components/hosting/seal-key.yaml`, `sealed-secrets-key.yaml`, `sealed-app-key.yaml`, `storage.yaml` | Written by hand, as templates `components.Hosting` fills | The inline manifests that carry the block's secrets and OpenBao's volumes |
| `openbaoinit/openbao-init.yaml` | Written by hand, as a template `openbaoinit.Manifests` fills | OpenBao's init Job in `mitstack-node`, and the ConfigMap with the recipient's public key |

mitstack-eu/cluster-bases is the repository of pinned third-party manifests
the k3s blocks copy from. It holds CloudNativePG, which the kit copies, and
none of the charts the script renders. The kit renders those the way
cluster-bases renders its own, with a pinned chart, a values file and a check.
To change one, edit its values file or the script's pin, then render again:

```bash
scripts/render-components.sh
```

CI then runs the same script with `--check`, which fails when a committed file
differs from a fresh render.

Talos applies each manifest of the set once, creating every object that does
not exist yet and leaving an existing one as it is. A change to a component or
a policy therefore reaches only a cluster built from the release that carries
it. On a new cluster, the first pass skips each object whose CRD is not yet
registered, such as the Kyverno policies, and Talos applies it on a later pass.

cluster-bases is private, so CI cannot fetch from it. A newer CloudNativePG
comes in by copying both files from a newer cluster-bases release and setting
the blob ids that `git rev-parse <tag>:cloudnative-pg/<file>` answers there in
`TestCloudNativePG_IsClusterBasesV040sComponentByteForByte`.

Each admission policy has a `kyverno test` case under
`components/admission/tests/`, which CI runs with the Kyverno CLI v1.19.1:

```bash
kyverno test components/admission/tests
```

The Talos images, such as the kubelet and the API server, are not in any
manifest. `pinnedTalosImages` in `components/image.go` lists them with the
digests their registries served for Talos v1.14.4 and Kubernetes 1.37.1.

## Where the scripts live

Each script is a file you can read and lint as the shell it is, and the Go
code embeds it with `go:embed`.

| Script | Where it runs | What it does | Test |
|---|---|---|---|
| `etcd/scripts/remove-member.sh` | The member removal Job, inline in its command | Looks up the lost node's etcd member, removes it, then deletes the Node | `etcd/etcd_test.go` |
| `etcd/scripts/bootstrap.sh` | The gateway, as `mitstack-talos-bootstrap` | Bootstraps etcd, or restores it and deletes the old Node objects, then deletes the admin talosconfig | `etcd/etcd_test.go` |
| `gateway/scripts/prepare.sh` | The gateway, once at first boot | Installs talosctl and kubectl, copies each image into the local registry by digest, and fetches each served manifest | `gateway/gateway_test.go` |
| `report/scripts/report.sh` | The node report's CronJob, inline in its command | Reads the nodes, the etcd members, the Argo CD Applications and CloudNativePG's objects, and posts the report | `report/report_test.go` |
| `openbaoinit/scripts/openbao-init.sh` | OpenBao's init Job, inline in its command | Initialises OpenBao once, commits `openbao-init.asc`, then enables kv and Kubernetes auth and writes `app-secrets-reader` | `openbaoinit/openbaoinit_test.go` |
| `scripts/render-components.sh` | A workstation and CI | Renders the component set | The `components` CI job |

The member removal Job, the node report and OpenBao's init Job run in the
tools image that `tools/Dockerfile` builds: bash, curl, jq, gpg, openssl,
talosctl and kubectl on Alpine 3.22. `tools/tools.env` pins both binaries with their sha256, and the
Dockerfile and the gateway's `prepare.sh` both install from it, so the image
and the gateway run the same binaries. Each release pushes and signs that
image and names it in `tools.Image`, which a block passes to
`etcd.RemoveMemberJob`, `report.Manifests`, `openbaoinit.Inputs` and the
gateway's `ExtraImages`.
[The README](../README.md#how-a-release-is-versioned) says how the release
pins it.

## How the tests run the scripts

The script tests run the script a rendered manifest carries, so they test what
a node would run. `testutil.NewTools` writes fake `talosctl`, `kubectl`,
`curl`, `skopeo` and `docker` commands that log each call, and each test reads
that log to check the order of the calls.

| Test | What it proves | Against what |
|---|---|---|
| `TestRender_MatchesTheGoldenConfiguration` | The configuration for one and three control-plane nodes on the `hcloud` and the `metal` profile matches its recording | `nodeconfig/testdata/golden/` |
| `TestRender_PullsEveryImageOfTheComponentSetFromEveryGatewaysMirror` | Every image the set names, and every image the configuration names, comes from a mirror with no fallback | The mirror list |
| `TestRender_UsesHetznersResolversAndTimeServersInPlaceOfTalosDefaults` | Hetzner's resolvers and time servers replace Talos's | The rendered documents |
| `TestRender_GivesACloudNodeADefaultRouteThroughHetznersRouterAndKeepsDHCPOnItsLink` | A Cloud node routes through `10.0.0.1` and keeps its DHCP address, and a dedicated node gets neither document | The rendered documents |
| `TestRender_LeavesTheFirewallToNftablesAndRunsEveryContainerOnTheHostNetwork` | Docker writes no firewall rule, and the mirror and Caddy publish no port through Docker | The rendered cloud-config |
| `TestRender_SendsMirrorPullsToTheRegistryAndRefusesEveryOtherMethod` | The Caddyfile tries the registry for a GET or HEAD before it answers 405 | The rendered Caddyfile |
| `TestRun_KeepsEveryKeyOfTheEmbeddedConfigurationOutOfTheBuildOutput` | No key of the fixture secrets reaches the build's log or error, on success or failure | A fake imager that prints the whole configuration, as imager does |
| `TestRemoveMemberJob_RemovesTheEtcdMemberBeforeDeletingItsNode` | The member goes before its Node, and a failed removal keeps the Node | Fake talosctl and kubectl |
| `TestBootstrapScript_RestoresFromTheSnapshotThenDeletesTheOldNodes` | The restore bootstraps from the snapshot, deletes every Node outside `--keep`, and deletes the talosconfig | Fake talosctl and kubectl |
| `TestReport_PostsEachNodeWithItsTalosVersionTheEtcdMembershipAndArgoCDsHealth` | The report carries each node, its Talos version, the members and each Application's health | Fake talosctl and kubectl, and a local HTTPS server playing the platform |
| `TestReport_WhoseCloudNativePGReadFailsStillCarriesItsNodesAndApplications` | A failed CloudNativePG read is reported with its exit status and stderr, beside the nodes and Applications | A fake kubectl that refuses only that read |
| `TestDefault_RunsKyvernoWithBothPoliciesAndCloudNativePGWithTheDatabaseSlicePolicy` | The set carries Kyverno, both policies, CloudNativePG and the slice policy, and the mirror holds each of their images by digest | The default set and its mirror list |
| `TestDatabaseSlicesPolicy_GrantsEachPodExactlyItsAllows` | A slice's Postgres instance, a CloudNativePG Job, an app pod, a pod outside a database slice and a system pod each get exactly their allows | Cilium's view of each pod's labels |
| `TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressingAndAStuckOneAsDegraded` | The `argocd-cm` health check reads each phase CloudNativePG passes through as Progressing, and a cluster waiting for its user as Degraded | The check run in gopher-lua, the Lua runtime Argo CD embeds |
| `TestInit_CommitsTheKeysAsPGPMessagesBeforeAnyRequestCarriesTheRootTokenThenWritesTheReadPolicyOnTheAccessorItRead` | One init with `recovery_pgp_keys`, both keys only as PGP messages gpg reads, the commit before any request with the root token, no plain key in a log, and the policy on the accessor read from `sys/auth` | Fake OpenBao and GitHub APIs over TLS, and the real gpg and openssl |
| `TestInit_OnAStoreInitialisedWithItsInitFileCommittedWritesNothing` | An initialised store whose file is committed gets no write, and the check's token may only read | The same fakes |
| `TestInit_StopsBeforeInitialisingWhenItCannotHandTheKeysBack` | A recipient gpg cannot encrypt to, a store initialised without the file, and a file already committed for an uninitialised store each stop the script with that state named | The same fakes |
| `TestHosting_LetsTheInitJobReachOnlyClusterDNSOpenBaoAndGitHubsAPIAndOpenBaoTakeItsCalls` | The init Job's pod may reach cluster DNS, OpenBao on 8200 and `api.github.com` on 443 and nothing else, and OpenBao takes its calls | Cilium's view of each pod's labels |
| `TestPrepareScript_RefusesAManifestWhoseBytesDifferFromItsPin` | The gateway installs no manifest whose sha256 differs from the list, and stops | Fake curl and skopeo |

The fixtures in `testutil/testdata/` are throwaway keys made for these tests:
a Talos secrets bundle, a gateway CA with its certificate and key, and a log
receiver's CA. The script tests need `bash`, `curl`, `jq`, `gpg` and `openssl` on `PATH`.

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
| `test` | `go vet ./...` and `go test -race ./...` pass on Go 1.26.10 |
| `components` | Each rendered component matches a fresh render, and shellcheck passes on every script |
| `tools image` | The tools image builds and each of its commands runs |
| `pr key gate` | A pull request's title or commits name a board key |
| `release` | On a push to `main` with a new version in `.github/version`, pushes and signs the tools image, pulls it with no login, and tags a commit on top that pins it in `tools.Image` |
| `record delivery` | On a push to `main`, records the push's board keys on Plain |

## What the kit leaves to its blocks

The kit renders bytes, so each of these stays with the block that calls it:

| Step | Why the kit does not do it |
|---|---|
| Delivering the admin talosconfig to the gateway and running the bootstrap there | It needs the block's command channel to the gateway |
| Taking an etcd snapshot and storing it | Where a snapshot lives is the block's choice |
| Renewing a gateway's certificate | `CA.Issue` takes the validity, and the block re-renders the gateway before it ends |
| Writing the report token into `mitstack-report-token` | The token comes from the platform at enrolment |
| A dedicated node's addresses and its vSwitch, Hetzner's private network between dedicated servers | The Talos trial on a dedicated test server decides them |
