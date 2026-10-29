# Developing the Talos cluster kit

This page is for anyone changing talos-cluster-kit: where the component set
comes from, where each script lives, how its tests run it, and what CI proves
before a release is tagged. Every path below is relative to the repository
root.

Back to [the README](../README.md).

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
| `components/cilium/cilium.yaml` | The cilium 1.19.9 chart with `components/render/cilium-values.yaml` | Cilium as the network plugin and in place of kube-proxy, with Hubble off so the chart mints no key |
| `components/cilium/policy-crd.yaml` | Cilium v1.19.9's `CiliumClusterwideNetworkPolicy` CRD | The CRD the metadata policy needs before Cilium's operator starts |
| `components/argocd/argocd.yaml` | The argo-cd 10.10.5 chart with `components/render/argocd-values.yaml`, Dex off | Argo CD v3.5.5 in namespace `argocd`, with a health check that reads a CloudNativePG Cluster passing through a switchover, failover, upgrade or configuration change as Progressing |
| `components/kyverno/kyverno.yaml` | The kyverno 3.9.1 chart with `components/render/kyverno-values.yaml`, its tests and Helm hooks off | Kyverno v1.19.1 in namespace `kyverno`, its images from `ghcr.io` and its webhooks skipping the system namespaces |
| `components/local-path-provisioner/local-path-provisioner.yaml` | The local-path-provisioner 0.0.37 chart with `components/render/local-path-provisioner-values.yaml`, taken from the source archive of the commit the project's v0.0.37 tag names, since the project publishes no chart repository | Rancher's local path provisioner in namespace `local-path-storage`, behind the default StorageClass `local-path`, keeping each claim on the user volume `local-path-provisioner` |
| `components/istio/istio-base.yaml`, `istiod.yaml`, `istio-cni.yaml`, `ztunnel.yaml` | Istio's base, istiod, cni and ztunnel 1.30.7 charts, each with its values file in `components/render/` | Istio in ambient mode in namespace `istio-system`: its CRDs, istiod with no sidecar injector, the CNI agent that puts every pod outside `kube-system`, `istio-system` and `kyverno` in the mesh, and ztunnel on every node |
| `components/cloudnative-pg/cloudnative-pg.yaml`, `components/cloudnative-pg/namespace.yaml` | Copied from mitstack-eu/cluster-bases v0.4.0 | The CloudNativePG 1.30.1 operator in namespace `cnpg-system`, with its default Postgres and PgBouncer images by digest |
| `components/admission/policies/` | Written by hand | The Kyverno policies that require the `mitstack.dev/app` label, give each app namespace `mitstack-default-deny`, refuse a pod the mesh leaves out, and refuse a PeerAuthentication that takes a call with no workload identity |
| `components/mesh/strict-mtls.yaml`, `probes.yaml`, `cloudnative-pg.yaml`, `argocd.yaml` | Written by hand | The mesh-wide `STRICT` PeerAuthentication, the Cilium policy that admits the kubelet's probes from `169.254.7.127`, the CloudNativePG operator's webhook exception and the ports a Postgres instance takes, and the network policies that admit the mesh's port 15008 between Argo CD's pods |
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
| `components/traefik/traefik.yaml` | The traefik 41.6.1 chart with `components/render/traefik-values.yaml` | Traefik on every node in namespace `traefik`, on NodePorts 30080 and 30081 |
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
| `report/scripts/report.sh` | The node report's CronJob, inline in its command | Reads the nodes, the etcd members, every node's volume statuses, the Argo CD Applications with the objects each one applied, CloudNativePG's objects, the objects of thirteen Kubernetes object types, such as Namespace and Deployment, labelled `mitstack.dev/subscription` and the API server's key set, and posts the report on one line with a `checks` member that names none of the platform verdict's checks. An applied object carries its API group, object type, namespace, name and sync status alone, and no Application carries its applied objects when they would take the body past the platform's 256 KiB. Each read and the body build runs under `REPORT_READ_SECONDS`, 25 by default, and the thirteen labelled-object reads share one such limit. When the nodes, Applications, key set or etcd members read or the body build fails or times out, the script posts a report holding only an `error` naming it, in place of the full report, and exits non-zero. A failed volume or CloudNativePG read stays inline in a full report, and an object type whose read fails is left out of `subscriptionResourceKinds`, the list of object types the report read whole | `report/report_test.go` |
| `openbaoinit/scripts/openbao-init.sh` | OpenBao's init Job, inline in its command | Initialises OpenBao once, commits `openbao-init.asc`, then enables kv and Kubernetes auth and writes `app-secrets-reader` | `openbaoinit/openbaoinit_test.go` |
| `controlplane/scripts/build-image.sh` | A Cloud control plane's builder server, from its user data | Builds the cluster's Talos disk image with imager, writes it over the builder's own disk from a tmpfs and powers the builder off, so the block can snapshot that disk | `controlplane/scripts_test.go` |
| `controlplane/scripts/bootstrap-first-node.sh` | A Cloud control plane's gateway, once at first boot | Finds the first node among the addresses the gateway has seen, proves it is the cluster's by its Talos API's certificate, bootstraps etcd on it and deletes the admin talosconfig | `controlplane/scripts_test.go` |
| `controlplane/scripts/cluster-tokens.sh` | A Job and a CronJob in the Cloud control plane's cluster | Spends the enrolment token for the report token and the cluster repository's token, and renews them with each app slice's repository token | `controlplane/scripts_test.go` |
| `scripts/render-components.sh` | A workstation and CI | Renders the component set | The `components` CI job |

The member removal Job, the node report and OpenBao's init Job run in the
tools image that `tools/Dockerfile` builds: bash, curl, jq, gpg, openssl,
talosctl and kubectl on Alpine 3.22. `tools/tools.env` pins both binaries with
their sha256, and the Dockerfile and the gateway's `prepare.sh` both install
from it, so the image and the gateway run the same binaries. `tools/jq.env`
pins the image's jq the same way, and the report's tests fetch that jq, since
jq-1.8 parses a number more strictly than the jq a runner may have. Each release
pushes and signs that image and names it in `tools.Image`, which a block
passes to `etcd.RemoveMemberJob`, `report.Manifests`, `openbaoinit.Inputs` and
the gateway's `ExtraImages`.
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
| `TestBootstrapScript_RestoresFromTheSnapshotThenDeletesTheOldNodes` | The restore bootstraps from the snapshot, deletes every Node outside `--keep`, and deletes the talosconfig | Fake talosctl and kubectl |
| `TestReport_PostsEachNodeWithItsTalosVersionTheEtcdMembershipAndEachApplicationsHealthAndAppliedObjects` | The report carries each node, its Talos version, the members, and each Application's health, sync and applied objects, and makes its reads in order. An applied object, a Secret among them, carries its API group, object type, namespace, name and sync status and no other field | Fake talosctl and kubectl answering `report/testdata/applications.json`, `report/testdata/golden/argocd.json`, and a local HTTPS server playing the platform |
| `TestReport_LeavesOutEveryApplicationsAppliedObjectsWhenCarryingThemWouldPassThePlatformsLimit` | When the applied objects would take the body past the platform's 256 KiB, no Application carries them, and the rest of the report still posts | A fake kubectl whose Application applies 4000 objects, and `report/testdata/golden/argocd-without-applied.json` |
| `TestReport_CarriesTheKeySetTheAPIServerSignsServiceAccountTokensWithUnderJwks` | The report posts the key set the API server serves at `/openid/v1/jwks`, unchanged, as its `jwks` member | A fake kubectl answering `report/testdata/jwks.json`, the key set of the fixture secrets' service-account key |
| `TestReport_DeclaresItsNodeReportContractAndCarriesNoneOfTheVerdictsChecks` | The report's `checks` member names contract `node-report/v1` and an empty `names`, so the platform judges none of its checks and reads each as `not-reported` | Fake talosctl and kubectl, and a local HTTPS server playing the platform |
| `TestReport_NamesAFailedVolumeOrCloudNativePGReadInlineWithItsExitStatusAsANumberInAFullReport` | A CloudNativePG or volume read that exits 1 or times out is named inline with its exit status, a number to the tools image's jq, and its stderr, and a volume read that prints no JSON or no `STATE` or `EPHEMERAL` is refused inline. The nodes, Applications and `checks` member still post | Fake reads, the jq `tools/jq.env` pins, and `REPORT_READ_SECONDS=1` |
| `TestReport_ThatCannotReadPostsOneReportNamingTheReadAndExitsNonZero` | When the nodes, Applications, key set or etcd members read fails, the key set read times out, or jq cannot build the body, exactly one report is posted. Its `error` names the read or `body`, its exit status or `timedOut`, and the last 1 KiB of its stderr with no NUL, report token, bearer token or JWT | Fake reads that write those to stderr, and `REPORT_READ_SECONDS=1` |
| `TestReport_ThatReadsEverythingPostsAHealthyBodyByteForByte` | A healthy report's body is byte for byte the recorded one | `report/testdata/golden/healthy-body.json` and a fake `date` |
| `TestReport_NamesEachNodesStateAndEphemeralVolumeWithItsEncryption` | `systemVolumes` holds each node's `STATE` and `EPHEMERAL` with its phase, encryption provider, configured keys and whether its key is locked to `STATE`, and `none` for a volume left plain | A fake talosctl answering `report/testdata/volumes.json`, and `report/testdata/golden/system-volumes.json` |
| `TestReport_ListsTheObjectsOfEachKindItReadWholeAndLeavesOutAKindWhoseReadFailsOrIsCutShort` | `subscriptionResources` lists each labelled object with its namespace, its object type as `kubectl --show-kind` names it, its name and its subscription, and `subscriptionResourceKinds` names the object types whose read finished whole. An object type whose read fails, prints no list, is cut short by the time limit, or holds more than 1000 objects is left out of both, the report stays whole, `madeAt` is no later than `reportedAt`, and the body carries no `subscriptionResourcesComplete` | A fake kubectl answering `report/testdata/subscription-resources/`, and `report/testdata/golden/subscription-resources.json` |
| `TestManifests_LetsTheReportListEachKindItReadsAndCloudNativePGsObjectsAndGrantsNoVerbOnSecrets` | The report's ClusterRole lists each object type the report reads, the ExternalSecret among them, and CloudNativePG's clusters, databases and roles, and names neither `secrets` nor `*` | The rendered ClusterRole |
| `TestRender_EncryptsStateAndEphemeralWithLUKS2UnderTheNodeIDKeyWithEphemeralsKeyLockedToState` | For one and three nodes on the `hcloud` and the `metal` profile, `STATE` and `EPHEMERAL` are LUKS2 under a `nodeID` key, `EPHEMERAL`'s locked to `STATE` and mounted secure | The rendered `VolumeConfig` documents |
| `TestDefault_RunsKyvernoWithBothPoliciesAndCloudNativePGWithTheDatabaseSlicePolicy` | The set carries Kyverno, its app-label and default-deny policies, CloudNativePG and the slice policy, and the mirror holds each of their images by digest | The default set and its mirror list |
| `TestRender_GivesEveryClusterADefaultStorageClassThatProvisionsOnItsNodesUserVolume` | For one node and for three, the configuration carries the user volume `local-path-provisioner` and fetches the provisioner. The set has one default class, the provisioner's. That class waits for its first consumer, makes `local` volumes and deletes a claim's directory with the claim. The provisioner keeps each claim on the user volume and runs with its hardened security context. The mirror holds both of its images by digest. Its manifests hold no Secret, no reference to one, no variable named for a credential and no rule on Secrets | The rendered documents and the set's manifests |
| `TestLocalPathProvisioner_LabelsEveryPodItStartsAndOpensTheAPIServerToItsNamespaceAlone` | The provisioner's pods and its helper pods carry `mitstack.dev/app`. Neither admission policy exempts its namespace, which admits a `hostPath` volume. Its pods may reach the API server and nothing else, and an app pod gains nothing from its policy | Both admission policies, and the flow policy's selectors evaluated in Go against the labels Cilium gives a pod in the namespace and an app pod |
| `TestDatabaseSlicesPolicy_GrantsEachPodExactlyItsAllows` | A slice's Postgres instance, a CloudNativePG Job, an app pod, a pod outside a database slice and a system pod each get exactly their allows, each beside port 15008 | Cilium's view of each pod's labels |
| `TestRender_RunsTheMeshAndItsAdmissionPoliciesOnEveryProfileAndSize` | For one and three nodes on the `hcloud` and the `metal` profile, the configuration fetches Istio's four charts and carries the mesh-wide `STRICT` policy, the probe policy and both mesh admission policies | The rendered documents |
| `TestRender_PutsEveryPodButKubeSystemsIstiosAndKyvernosInTheMeshFromTheMirror` | The CNI agent selects every pod whatever its labels, skips only `kube-system`, `istio-system` and `kyverno`, which Kyverno's mesh policy exempts too, and leaves DNS to Cilium. The mirror holds Istio's three images, `istio-system` is privileged, istiod renders no injector templates, and Cilium keeps other CNI configuration and its socket load balancing to the host | The CNI agent's and Cilium's ConfigMaps and the mirror list |
| `TestRender_RefusesACallWithoutAWorkloadIdentityButOnTheWebhookAndWebPorts` | Every PeerAuthentication is `STRICT`, the only ports that take a call with no identity are the five exceptions, the CloudNativePG operator's in the default set and four in the hosting set. `mitstack-require-strict-mtls` admits the four hosting exceptions by name, and the operator's sits in `cnpg-system`, which the policy skips | Every PeerAuthentication of the hosting set |
| `TestMesh_KeepsTheOperatorAloneOnAnInstancesManagerPortAndEveryoneOffItsOtherPorts` | A Postgres instance takes 5432 from any caller Cilium admits, 8000 from the operator alone, and nothing else | The AuthorizationPolicy's rules, read by `testutil.Denies` |
| `TestRender_AdmitsZtunnelsPortWhereverArgoCDsChartAdmitsACallBetweenItsPods` | Each Argo CD chart policy that admits a call from its own pods has a policy admitting 15008 from the same pods | Both sets of NetworkPolicies |
| `TestHosting_KeepsTheMembersAloneOnOpenBaosClusterPortAndEveryoneOffItsOtherPorts` | OpenBao takes 8200 from any caller Cilium admits, 8201 from its members alone, and nothing else | The AuthorizationPolicy's rules, read by `testutil.Denies` |
| `TestArgoCD_ReadsACloudNativePGClusterPassingThroughAPhaseAsProgressingAndAStuckOneAsDegraded` | The `argocd-cm` health check reads each phase CloudNativePG passes through as Progressing, and a cluster waiting for its user as Degraded | The check run in gopher-lua, the Lua runtime Argo CD embeds |
| `TestInit_CommitsTheKeysAsPGPMessagesBeforeAnyRequestCarriesTheRootTokenThenWritesTheReadPolicyOnTheAccessorItRead` | One init with `recovery_pgp_keys`, both keys only as PGP messages gpg reads, committed beside another writer's commit and before any request with the root token, no plain key in a log, and the policy on the accessor read from `sys/auth` | A fake OpenBao over TLS, a bare Git repository behind a fake ssh, and the real git, gpg and openssl |
| `TestInit_ReachesTheRepositoryOverSSHOn443WithTheDeployKeyAndGitHubsPinnedHostKeysAlone` | Every Git call goes through ssh to port 443 with the deploy key alone, strict host key checking and the Job's known hosts file | The same fakes, with the fake ssh recording its arguments |
| `TestInit_ChangesNothingOnAStoreItCannotInitialiseOrThatHoldsItsFileAlready` | A recipient gpg cannot encrypt to, a store initialised without the file, and a file already committed for an uninitialised store each stop the script with that state named. A store initialised with its file committed exits 0. None of them gets a write or a push | The same fakes |
| `TestHosting_GivesArgoCDTheClusterRepositoryOverSSHOnPort443FromTheSealedReadKey` | What `components.Hosting` returns holds one SealedSecret that opens into a Secret in `argocd` labelled `argocd.argoproj.io/secret-type: repository`. Its data is exactly `type` `git`, the cluster repository's `ssh://git@ssh.github.com:443/` URL and the read-only deploy key. The README section "How Argo CD reads the cluster repository" explains each term | The test helper `testutil.Unseal`, which decrypts a SealedSecret the way the controller does, with the throwaway key pair `testutil.HostingInputs` returns, both in `testutil/hosting.go` |
| `TestHosting_GivesTheInitJobTheWriteKeyInTheNamespaceItRunsIn` | The write deploy key opens as `github-deploy-key` in `mitstack-node`, where the init Job runs | `testutil.Unseal` and the same fixture |
| `TestSeal_SealsAValueOnlyTheKeyPairOpensInTheStrictScopeOfItsSecret` | `components.Seal` writes a SealedSecret that its key pair opens, under strict scope, into exactly the named Secret and value, and `components.Hosting` accepts what it seals | `testutil.Unseal` and a throwaway key pair from `testutil.SealedSecretsKeyPair` |
| `TestSealValue_SealsAValueTheControllerOpensAndOpenReadsOnlyUnderItsSecret` | `components.SealValue` returns a value that `testutil.Unseal` opens, `components.Open` reads it back under its own namespace and name, and refuses it under any other | A throwaway key pair from `testutil.SealedSecretsKeyPair` |
| `TestNewDeployKey_MintsAFreshEd25519KeyWhosePrivateHalfMatchesThePublicHalfARepositoryLists` | `components.NewDeployKey` returns an `ssh-ed25519` public half that matches its OpenSSH private half, and a second call mints another key | `testutil.AuthorizedKey`, which reads a private key's public half |
| `TestHosting_RendersNoExternalSecretGeneratorOrOtherWriterOfArgoCDRepositoryCredentials` | What `components.Hosting` returns holds no ExternalSecret or ClusterExternalSecret, the namespaced and cluster-wide External Secrets objects that write a Secret. It holds no object of the API group `generators.external-secrets.io`, whose objects mint values such as GitHub tokens. Argo CD reads a Secret labelled `argocd.argoproj.io/secret-type` as a credential, and the only object so labelled is the SealedSecret `TestHosting_GivesArgoCDTheClusterRepositoryOverSSHOnPort443FromTheSealedReadKey` checks | Every document of every manifest `components.Hosting` returns |
| `TestHosting_TheInitJobTrustsTheSSHHostKeysArgoCDTrustsForGitHubOn443` | The init Job's known hosts are exactly the `[ssh.github.com]:443` entries of `argocd-ssh-known-hosts-cm` | The vendored Argo CD manifest |
| `TestHosting_LetsTheInitJobReachOnlyClusterDNSOpenBaoAndGitHubsSSHOn443AndOpenBaoTakeItsCalls` | The init Job's pod may reach cluster DNS, OpenBao on 8200 or 15008 and `ssh.github.com` on 443 and nothing else, and OpenBao takes its calls | Cilium's view of each pod's labels |
| `TestPrepareScript_RefusesAManifestWhoseBytesDifferFromItsPin` | The gateway installs no manifest whose sha256 differs from the list, and stops | Fake curl and skopeo |
| `TestPlane_BuildsANodeInEachOfFsn1Nbg1AndHel1OnOneNetwork` | Calling `NextStep` until it returns none builds a node in each location on one network, a gateway and a builder in `fsn1`, and deletes the builder once its snapshot exists | `controlplane/cloudtest`, a fake of the Hetzner Cloud API |
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
| `TestHosting_InstallsEveryNodeWithTheBuilderFetchingTheHostingSetFromTheServingServer` | A hosting install creates a node in each location, with the serving server at `10.0.0.251` serving to the builder alone and the builder pinning its key, and deletes the serving server after the build | The same fake |
| `TestHosting_TheBuildersUserDataStaysWithinHetznersLimitAndHoldsNoHostingManifest` | With a 4096-bit Sealed Secrets key the builder's user data, whose size the test logs, fits in 32,768 bytes and carries none of the hosting set's inline manifests | The same fake |
| `TestHosting_TheServingServerGoesWithABuildThatFails` | A build past its deadline takes the serving server with the builder | The same fake |
| `TestBuildImage_HandsImagerTheConfigurationWithTheHostingSetTheServingServerServed` | The build script fetches the hosting set from the real serving program over pinned TLS, imager gets it appended to the configuration, and the served file is gone | `serve/scripts/serve.py` on a local port, and fake build commands |
| `TestBuildImage_AServerThatFailsThePinGetsNoBuildAndTheConfigurationIsRemoved` | A server whose key fails the pin gets no build, and the script removes the configuration | The same |
| `TestClusterTokens_EnrolOnAClusterThatReadsItsRepositoryWithADeployKeyWritesTheReportTokenAlone` | With `REPOSITORY_CREDENTIAL=deploy-key` the enrolment writes only `mitstack-report-token`, and `renew` refuses | Fake kubectl and a local HTTPS server playing the platform |
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
| `test` | `go vet ./...` and `go test -race ./...` pass on Go 1.26.10 |
| `components` | Each rendered component matches a fresh render, and shellcheck passes on every script |
| `tools image` | The tools image builds and each of its commands runs |
| `pr key gate` | A pull request's title or commits name a board key |
| `release` | On a push to `main` with a new version in `.github/version`, pushes and signs the tools image, pulls it with no login, and tags a commit on top that pins it in `tools.Image` |
| `record delivery` | On a push to `main`, records the push's board keys on Plain |

## What the kit leaves to its blocks

Every package but `controlplane` renders bytes, so each of these stays with
the block that calls it:

| Step | Why the kit does not do it |
|---|---|
| Delivering the admin talosconfig to the gateway and running the bootstrap there | It needs the block's command channel to the gateway |
| Taking an etcd snapshot and storing it | Where a snapshot lives is the block's choice |
| Renewing a gateway's certificate | `CA.Issue` takes the validity, and the block re-renders the gateway before it ends |
| Writing the report token into `mitstack-report-token` | The token comes from the platform at enrolment |
| A dedicated node's address and its vSwitch, Hetzner's private network between dedicated servers | Which dedicated servers a cluster adopts, and over which vSwitch, is the block's choice |
