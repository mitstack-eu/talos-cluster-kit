# Developing the Talos cluster kit

This page is for anyone changing talos-cluster-kit: where the component set
comes from, where each script lives, how its tests run it, and what CI proves
before a release is tagged. Every path below is relative to the repository
root.

Back to [the README](../README.md).

## Where the component set comes from

The component set is the manifests every Talos cluster starts with.
The table lists its five files. `scripts/render-components.sh` writes the
first three from pinned sources, each checked against a published sha256, and
refuses any image that is not named by digest. The last two are written by
hand.

| File | Source | What it runs |
|---|---|---|
| `components/cilium/cilium.yaml` | The cilium 1.19.9 chart with `components/render/cilium-values.yaml` | Cilium as the network plugin and in place of kube-proxy, with Hubble off so the chart mints no key |
| `components/cilium/policy-crd.yaml` | Cilium v1.19.9's `CiliumClusterwideNetworkPolicy` CRD | The CRD the metadata policy needs before Cilium's operator starts |
| `components/argocd/argocd.yaml` | The argo-cd 10.10.5 chart with `components/render/argocd-values.yaml`, Dex off | Argo CD v3.5.5 in namespace `argocd` |
| `components/metadata/metadata-block.yaml` | Written by hand | The policy that drops pod traffic to `169.254.169.254` and isolates nothing else |
| `components/logs/collector.yaml` | Written by hand, as a template `components.LogCollector` fills | The node-local OpenTelemetry Collector on the host network |

mitstack-eu/cluster-bases, the repository of pinned third-party manifests the
k3s blocks copy from, holds none of the first three today. The kit renders
them the way cluster-bases renders its own charts, with a pinned chart, a
values file and a check. To change one, edit its values file or the script's
pin, then render again:

```bash
scripts/render-components.sh
```

CI then runs the same script with `--check`, which fails when a committed file
differs from a fresh render.

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
| `report/scripts/report.sh` | The node report's CronJob, inline in its command | Reads the nodes, the etcd members and the Argo CD Applications, and posts the report | `report/report_test.go` |
| `scripts/render-components.sh` | A workstation and CI | Renders the component set | The `components` CI job |

The member removal Job and the node report run in the tools image that
`tools/Dockerfile` builds: bash, curl, jq, talosctl and kubectl on Alpine
3.22. `tools/tools.env` pins both binaries with their sha256, and the
Dockerfile and the gateway's `prepare.sh` both install from it, so the image
and the gateway run the same binaries. The block
builds, signs and pushes that image and passes its digest to
`etcd.RemoveMemberJob` and `report.Manifests`.

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
| `TestRun_KeepsEveryKeyOfTheEmbeddedConfigurationOutOfTheBuildOutput` | No key of the fixture secrets reaches the build's log or error, on success or failure | A fake imager that prints the whole configuration, as imager does |
| `TestRemoveMemberJob_RemovesTheEtcdMemberBeforeDeletingItsNode` | The member goes before its Node, and a failed removal keeps the Node | Fake talosctl and kubectl |
| `TestBootstrapScript_RestoresFromTheSnapshotThenDeletesTheOldNodes` | The restore bootstraps from the snapshot, deletes every Node outside `--keep`, and deletes the talosconfig | Fake talosctl and kubectl |
| `TestReport_PostsEachNodeWithItsTalosVersionTheEtcdMembershipAndArgoCDsHealth` | The report carries each node, its Talos version, the members and each Application's health | Fake talosctl and kubectl, and a local HTTPS server playing the platform |
| `TestPrepareScript_RefusesAManifestWhoseBytesDifferFromItsPin` | The gateway installs no manifest whose sha256 differs from the list, and stops | Fake curl and skopeo |

The fixtures in `testutil/testdata/` are throwaway keys made for these tests:
a Talos secrets bundle, a gateway CA with its certificate and key, and a log
receiver's CA. The script tests need `bash`, `curl` and `jq` on `PATH`.

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
| `release` | On a push to `main`, tags the commit with the version `.github/version` names |
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
