# Runner services

A cluster with runner services runs the GitHub Actions jobs of one
organisation as pods. ARC starts a pod for each job a repository queues and
removes the pod once the job ends. This page is for whoever writes the block's
operator, the program that builds and upgrades a block's cluster with this kit.
It explains what the kit adds, how to switch the services on, and how a token
and a release reach the cluster.

Back to [the README](../README.md).

## The terms this page uses

| Term | What it means here |
|---|---|
| Plane | The kit's `controlplane.Plane`, which describes one cluster to build |
| Runner inputs | The `components.RunnerInputs` a plane names to switch the runner services on |
| The platform | mitstack's API, which issues leases and receives the node report |
| Lease | A short-lived credential the platform grants a cluster, here a GitHub token |
| ARC | actions-runner-controller, GitHub's operator for self-hosted runners |
| Runner group | A group of an organisation's self-hosted runners, which decides the repositories they serve |
| dind | Docker in Docker: a Docker daemon in a sidecar container of the job's pod |
| emptyDir | A Kubernetes volume that starts empty with its pod and is deleted with it |
| Gateway | The kit's server in each location, which mirrors every image the cluster's nodes pull |
| Node report | The kit's job that posts each node's state to the platform |
| Report token | The token the node report and the lease job present to the platform |
| Burst member | A Cloud worker added for a burst of load, labelled `mitstack.dev/burst` |
| Rerender | The kit's render of a running node's configuration for a new release, which keeps the settings that node already has |

A block and the other workspace terms are defined in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md).

## What a cluster with runner services runs

A cluster runs runner services when its plane names runner inputs. It then
holds the following parts, beside every component a cluster runs anyway:

- The namespace `mitstack-runners`, at the privileged Pod Security level,
  because each job's Docker daemon runs privileged.
- ARC's controller, which watches that namespace alone. The kit renders it
  from the gha-runner-scale-set-controller 0.15.1 chart.
- One scale set, an AutoscalingRunnerSet named for the label a workflow's
  `runs-on` names. It registers in the organisation's runner group, starts no
  runner while no job waits, and starts at most the number of runners the
  operator sets.
- The scale set's listener, a pod ARC's controller starts, which holds a long
  poll open to GitHub and asks the controller for a runner pod when a job
  waits.
- A runner pod for each job. It runs the actions-runner image beside a Docker
  daemon as a sidecar, so a job can build and run containers. Every volume it
  mounts is an emptyDir, it mounts no service account token, and it may run on
  a burst member.
- The CronJob `mitstack-runner-lease` in `mitstack-node`, which keeps the
  GitHub token ARC reads from the Secret `mitstack-runner-token` fresh.

The three pod policies `mitstack-disallow-host-path`,
`mitstack-require-resource-limits` and `mitstack-restrict-image-registries`
skip `mitstack-runners`, as
[the README's table of skipped namespaces](../README.md#which-namespaces-the-pod-policies-skip)
explains. The cluster runs each one under its usual name, so nothing that
names a policy changes.

The Cilium policy `mitstack-runners-flows` lets every pod of the namespace
reach the API server and the cluster's DNS. Each pod also reaches GitHub on
port 443, at `github.com`, `api.github.com` and `*.actions.githubusercontent.com`,
which ARC's controller and listener need. Only a job's pod, which ARC labels
`app.kubernetes.io/component: runner`, reaches the rest of the internet, since
a job reaches whatever its workflow pulls or calls.

Every node's kubelet starts collecting unused images once 80% of a disk is in
use, and evicts pods once less than 15% of a disk is free for a minute. A
job's images and build caches fill a disk quickly, and an eviction stops a pod
gracefully where a full disk would stop the node. A node the kubelet marks with the
`node.kubernetes.io/disk-pressure` taint reads as not serving in the node
report, so the redundancy guard counts it as down.

## How the parts fit together

```mermaid
C4Container
  title Runner services on a Talos cluster
  Person(developer, "Developer", "Pushes a commit to a repository of the organisation")
  System_Ext(github, "GitHub", "Queues each workflow job and hands it to a registered runner")
  System_Ext(platform, "mitstack platform", "Leases a GitHub token for the cluster's runner resource")
  System_Boundary(cluster, "Talos cluster") {
    Container(lease, "mitstack-runner-lease", "CronJob in mitstack-node", "Leases a GitHub token every 30 minutes")
    ContainerDb(secret, "mitstack-runner-token", "Secret in mitstack-runners", "Holds the token under github_token")
    Container(controller, "ARC controller", "Deployment in mitstack-runners", "Registers the scale set and starts its listener and runner pods")
    Container(listener, "Listener", "Pod in mitstack-runners", "Holds a long poll open to GitHub for the scale set")
    Container(runner, "Runner pod", "Pod in mitstack-runners", "Runs one job beside a Docker daemon sidecar")
  }
  System_Ext(gateway, "Gateways", "Mirror every image the cluster pulls")
  Rel(developer, github, "Pushes a commit")
  Rel(lease, platform, "POST /v1/cluster-leases", "HTTPS, report token")
  Rel(lease, secret, "Writes the token", "Kubernetes API")
  Rel(controller, secret, "Reads the token")
  Rel(controller, listener, "Starts")
  Rel(listener, github, "Waits for a job", "HTTPS")
  Rel(controller, runner, "Starts one per job")
  Rel(runner, github, "Runs the job", "HTTPS")
  Rel(runner, gateway, "Pulls images")
```

## How an operator switches the services on

A block switches the services on by setting `Plane.Runners` to a
`components.RunnerInputs`. Both live in this repository,
`github.com/mitstack-eu/talos-cluster-kit`: `Plane` in the package
`controlplane`, in `controlplane/plane.go`, and `RunnerInputs` in the package
`components`. A plane whose `Runners` is nil runs no runner, and its cluster
renders as it did before.

| Field | What it holds | What the kit refuses |
|---|---|---|
| `Organization` | The GitHub organisation login, which gives the scale set its URL `https://github.com/<Organization>` | A value that is no GitHub login |
| `Group` | The runner group the scale set registers into | A value that is no DNS label |
| `ScaleSet` | The scale set's name, which is also the label a job's `runs-on` names | A value that is no DNS label |
| `RunnerImage` | The actions-runner image each job runs | An image without its digest |
| `DindImage` | The Docker daemon image each job's sidecar runs | An image without its digest |
| `MaxRunners` | The most jobs that run at once | A value below 1 |
| `LeaseResource` | The resource type whose GitHub token the lease job asks the platform for | An empty value |

The block's operator names the resource it requires from the platform as
`LeaseResource`, so the platform knows which GitHub App mints the token. The
kit holds no GitHub key and renders no token. The scale set names the Secret
`mitstack-runner-token`, and the lease job fills it.

The kit builds two sets from the inputs. `Set.WithRunnerServices` adds only
the served services: the namespace, ARC's controller and the three pod
policies that skip the namespace. A gateway mirrors that set, since it needs
no scale set. `Set.WithRunners` adds the same services and the scale set
rendered from the inputs, which the build and every release carry.

A build of a plane with runner inputs carries the runner services from its
first boot. A plane that gains them on an upgrade replaces each gateway first,
as the last section of this page shows, and its next release carries the rest.

The kit renders the disk eviction settings into a node's configuration when it
builds the node. A rerender keeps a running node's kubelet settings, so a
cluster that gains the runner services by an upgrade keeps its nodes' kubelet
settings. Each node takes the disk eviction settings only once it is rebuilt.

## How the GitHub token stays fresh

The platform leases a GitHub installation token, narrowed to the
organisation's self-hosted runners, which lives an hour. ARC's controller
reads it from the Secret to register the scale set. The CronJob
`mitstack-runner-lease` runs
`cluster-tokens.sh runner-lease` every 30 minutes, so a token stays valid
through one failed run. Each run asks for a new token, whatever the Secret
holds, and makes three calls in turn.

The Secret starts empty, so ARC registers the scale set only after the
CronJob's first run. That run comes at the next full or half hour, up to 30
minutes after the build. ARC's controller retries the registration with a
growing delay until the Secret holds a token. The calls of each run are these:

```mermaid
sequenceDiagram
  participant Job as mitstack-runner-lease
  participant Platform as mitstack platform
  participant API as Kubernetes API
  participant ARC as ARC controller
  Job->>Platform: POST /v1/cluster-leases with the report token
  Platform-->>Job: 200 with credential.token and expiresAt
  Job->>Job: Check the token is there and expiresAt is within 65 minutes
  Job->>API: PATCH Secret mitstack-runner-token with github_token and its expiry
  API-->>Job: 200
  Job->>API: DELETE the listener's pods by their label
  API-->>Job: 200
  ARC->>API: Start a new listener
  ARC->>API: Read mitstack-runner-token
```

The listener keeps the token it started with, so the job deletes the
listener's pods once the new token is in the Secret. ARC's controller then
starts a listener that reads it. The job deletes only the listener's pods,
selected by their label, and no runner pod.

A run fails, and writes nothing, in each of these cases:

- The platform answers anything but 200. The run names the status and the
  resource, and the next run is the retry.
- The answer holds no token, or an expiry that is not after now and within 65
  minutes of now.
- The Secret refuses the patch. The run names the Secret and the status and
  leaves the listener running on the token it holds.

The job's account may patch the one Secret and delete pods by collection in
`mitstack-runners`. It may read, list or create nothing. Kubernetes lets that
account delete any pod of the namespace, so the label selector of the job's
call limits the deletion to the listener's pods. The job never prints the
token or passes it on a command line.

## How a runner release reaches the cluster

Every image a node pulls comes through the gateways' mirror, and a gateway
never changes once it runs. A new runner image, a new dind image or a new ARC
release therefore changes the gateways' list digest, and the upgrade replaces
each gateway before the release reaches the cluster. A change to `MaxRunners`,
`Group` or the organisation names no image, so it leaves the gateways alone.

```mermaid
sequenceDiagram
  participant Block as Block
  participant Plane as Control plane
  participant Gateway as Gateway in each location
  participant Repo as Cluster repository
  participant Argo as Argo CD
  Block->>Plane: Upgrade with the new RunnerImage
  Plane->>Plane: Compute the list digest from the set and its images
  Plane->>Gateway: Build a replacement from the new list
  Gateway-->>Plane: The node report reads it serving
  Plane->>Repo: Commit the release with the new scale set
  Argo->>Repo: Read the release
  Argo->>Argo: Apply the AutoscalingRunnerSet
  Note over Argo: ARC's controller starts the next job's pod on the new image
```

A job that runs during the change ends on the image it started with. ARC's
controller starts each later job on the new image.

## Where the parts live

| File | What it holds |
|---|---|
| `components/runners.go` | `RunnerInputs`, its checks, and `Set.WithRunnerServices` and `Set.WithRunners` |
| `components/runners/runners.yaml` | The namespace and its Cilium policy |
| `components/runners/scale-set.yaml` | The template of the scale set, its runner pod, and the Role and RoleBinding that let ARC's controller create each listener's Role |
| `components/runners/policies/` | The three pod policies that skip `mitstack-runners`, which `scripts/render-components.sh` writes |
| `components/gha-runner-scale-set-controller/gha-runner-scale-set-controller.yaml` | ARC's controller and its CRDs, which `scripts/render-components.sh` renders |
| `controlplane/runners.go` and `controlplane/manifests/runner-token.yaml` | The runner token's Secret and the lease CronJob |
| `controlplane/scripts/cluster-tokens.sh` | The `runner-lease` verb the CronJob runs |
| `nodeconfig/documents.go` | The kubelet's disk eviction settings |
| `report/members.go` | The reading of a node under disk pressure |

[The development page](development.md) explains how to render the
controller and the policies again.
