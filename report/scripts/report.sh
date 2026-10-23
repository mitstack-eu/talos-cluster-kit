#!/usr/bin/env bash
# Posts the node report: each node's Talos version and readiness, the etcd members, how each node's STATE and EPHEMERAL
# are encrypted, each Argo CD Application's health, CloudNativePG's objects, the objects labelled mitstack.dev/subscription with the kinds it read whole, the API server's
# key set as jwks, and a checks member naming none of the verdict's checks.
# When a read or the body fails or times out, it posts a report whose error names it instead, then exits non-zero.
# Usage: REPORT_URL=<https url> SUBSCRIPTION_ID=<id> HOST_IP=<this pod's node> [REPORT_TOKEN_FILE=/var/run/mitstack/token] [REPORT_READ_SECONDS=25] report.sh
# It exists so the platform sees a cluster it never dials into, as ADR 0072 requires, with os:reader and no Secret right.
set -euo pipefail

# Eight limits of 25 seconds, their 5-second kill grace and the post's 20 fit the Job's activeDeadlineSeconds of 270.
limit=${REPORT_READ_SECONDS:-25}
# Each kind as kubectl --show-kind names it, which is how a block names the objects it creates. The report holds no
# Secret right, so secret is never one of them.
subscription_kinds=(namespace serviceaccount service resourcequota limitrange deployment.apps
  ingress.networking.k8s.io networkpolicy.networking.k8s.io role.rbac.authorization.k8s.io
  rolebinding.rbac.authorization.k8s.io application.argoproj.io appproject.argoproj.io
  externalsecret.external-secrets.io)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "report: $*" >&2
  exit 1
}

limited() {
  timeout -k 5 "$limit" "$@"
}

# read_into NAME OUT COMMAND... runs one read under the time limit and reports it as failed when it does not exit 0.
read_into() {
  local name=$1 out=$2 status=0
  shift 2
  limited "$@" > "$out" 2> "$work/read.err" || status=$?
  [ "$status" -eq 0 ] || report_failure "$name" "$status"
}

collect() {
  read_into "kubectl get nodes" "$work/nodes.json" kubectl get nodes -o json
  read_into "kubectl get applications" "$work/applications.json" kubectl get applications.argoproj.io -n argocd -o json
  read_into "kubectl get --raw /openid/v1/jwks" "$work/jwks.json" kubectl get --raw /openid/v1/jwks
  read_into "talosctl etcd members" "$work/members.txt" talosctl -n "$HOST_IP" etcd members
  read_system_volumes
  read_beside cloudnative-pg kubectl get clusters.postgresql.cnpg.io,databases.postgresql.cnpg.io,databaseroles.postgresql.cnpg.io -A -o json
  read_subscription_resources
}

# read_beside NAME COMMAND... keeps the read's output, stderr and exit status, so a failed read is reported beside the
# others in a full report.
read_beside() {
  local name=$1 status=0
  shift
  limited "$@" > "$work/$name.out" 2> "$work/$name.err" || status=$?
  printf '%s' "$status" > "$work/$name.status"
}

# read_system_volumes reads every node's volume statuses through the Talos API and slurps talosctl's stream of objects
# into one array.
read_system_volumes() {
  local nodes
  nodes=$(jq -r '[.items[].status.addresses[]? | select(.type == "InternalIP") | .address] | join(",")' "$work/nodes.json") || nodes=
  read_beside system-volumes talosctl -n "$nodes" get volumestatus -o json
  jq -s . "$work/system-volumes.out" > "$work/system-volumes.json" 2> /dev/null || true
}

# read_subscription_resources reads each kind on its own, all of them within one time limit, and keeps a kind only when
# its read exits 0 with a list, so a read that fails or is cut short leaves its kind out of the report.
read_subscription_resources() {
  local kind
  mkdir "$work/kinds"
  # shellcheck disable=SC2016 # the inner script expands its own arguments
  limited bash -c 'dir=$1; shift; for kind; do
      kubectl get "$kind" -A -l mitstack.dev/subscription -o json > "$dir/$kind.read" && mv "$dir/$kind.read" "$dir/$kind.json"
    done' _ "$work/kinds" "${subscription_kinds[@]}" || true
  : > "$work/subscription-resources.json"
  for kind in "${subscription_kinds[@]}"; do
    [ -f "$work/kinds/$kind.json" ] || continue
    if jq -c --arg kind "$kind" '{kind: $kind, objects: [.items[] | {
        namespace: (.metadata.namespace // null), kind: $kind, name: .metadata.name,
        subscription: .metadata.labels["mitstack.dev/subscription"]
      }]}' "$work/kinds/$kind.json" > "$work/kind.read" 2>/dev/null; then
      cat "$work/kind.read" >> "$work/subscription-resources.json"
    fi
  done
}

# report_failure NAME STATUS posts a report whose error names the read, how it ended and the last 1 KiB of its
# stderr, keeping no NUL and no line that holds the report token or looks like a bearer token or a JWT.
report_failure() {
  local name=$1 status=$2
  tr -d '\000' < "$work/read.err" \
    | { grep -avF -- "$token" || true; } \
    | { grep -avE '[Bb]earer|eyJ[A-Za-z0-9_-]+\.' || true; } \
    | tail -c 1024 > "$work/read.tail"
  cat "$work/read.tail" >&2
  jq -n \
    --arg subscription "$SUBSCRIPTION_ID" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg read "$name" \
    --argjson status "$status" \
    --rawfile stderr "$work/read.tail" '{
      subscriptionId: $subscription,
      reportedAt: $at,
      error: ({read: $read, stderr: $stderr} + if $status == 124 then {timedOut: true} else {exitStatus: $status} end)
    }' > "$work/body.json"
  post
  fail "$name ended with status $status, and the report names it"
}

# body prints the report; a member line is NODE ID HOSTNAME PEER-URLS CLIENT-URLS LEARNER.
body() {
  limited jq -n \
    --arg subscription "$SUBSCRIPTION_ID" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --slurpfile nodes "$work/nodes.json" \
    --slurpfile applications "$work/applications.json" \
    --slurpfile jwks "$work/jwks.json" \
    --rawfile members "$work/members.txt" \
    --rawfile cnpg "$work/cloudnative-pg.out" \
    --rawfile cnpg_err "$work/cloudnative-pg.err" \
    --argjson cnpg_status "$(cat "$work/cloudnative-pg.status")" \
    --rawfile volumes "$work/system-volumes.json" \
    --rawfile volumes_err "$work/system-volumes.err" \
    --argjson volumes_status "$(cat "$work/system-volumes.status")" \
    --arg made_at "$made_at" \
    --slurpfile kinds "$work/subscription-resources.json" \
    --from-file /dev/stdin <<'JQ'
    def talos_version: (.status.nodeInfo.osImage // "") | (try capture("^Talos \\((?<v>[^)]+)\\)").v catch null);
    def ready: [.status.conditions[]? | select(.type == "Ready") | .status == "True"] | any;
    def cloudnative_pg: {
      namespace: .metadata.namespace, kind: .kind, name: .metadata.name,
      subscription: (.metadata.labels["mitstack.dev/subscription"] // null)
    } + if .kind == "Cluster" then
      {phase: .status.phase, instances: .status.instances, readyInstances: .status.readyInstances}
    else
      {applied: .status.applied, message: (.status.message // null | if . then .[0:256] else . end)}
    end;
    def system_volume: {
      node: .node, name: .metadata.id, phase: .spec.phase,
      encryptionProvider: (.spec.encryptionProvider // "none"),
      encryptionKeys: (.spec.configuredEncryptionKeys // []),
      lockedToState: (.spec.encryptionLockedToState // false)
    };
    # beside(status; stderr; refusal; read) is a read_beside read's result, or how it failed.
    def beside($status; $stderr; $refusal; read):
      if $status != 0 then {exitStatus: $status, stderr: $stderr[-1024:]}
      else try read catch {exitStatus: 0, stderr: $refusal}
      end;
    # A kind is listed whole or not at all, and 1000 objects keep the body inside the platform's 256 KiB.
    def subscription_resources:
      reduce $kinds[] as $read ({objects: [], kinds: []};
        if (.objects | length) + ($read.objects | length) <= 1000 then
          .objects += $read.objects | .kinds += [$read.kind]
        else . end);
    subscription_resources as $listed | {
      subscriptionId: $subscription,
      reportedAt: $at,
      madeAt: $made_at,
      nodes: [$nodes[0].items[] | {
        name: .metadata.name,
        ready: ready,
        talosVersion: talos_version,
        kubeletVersion: .status.nodeInfo.kubeletVersion,
        internalIP: ([.status.addresses[]? | select(.type == "InternalIP") | .address] | first)
      }],
      etcd: {members: [$members | split("\n") | .[1:][] | select(test("\\S")) | [splits("\\s+")] | {
        id: .[1], hostname: .[2], peerURLs: (.[3] | split(",")), learner: (.[5] == "true")
      }]},
      systemVolumes: beside($volumes_status; $volumes_err;
        "talosctl exited 0, and its output is not the volume statuses this report reads";
        $volumes | fromjson | [.[] | select(.metadata.id == "STATE" or .metadata.id == "EPHEMERAL") | system_volume]),
      argocd: {applications: [$applications[0].items[] | {
        name: .metadata.name,
        health: (.status.health.status // "Unknown"),
        sync: (.status.sync.status // "Unknown")
      }]},
      cloudNativePG: beside($cnpg_status; $cnpg_err;
        "kubectl exited 0, and its output is not the CloudNativePG objects this report reads";
        $cnpg | fromjson | [.items[] | cloudnative_pg]),
      subscriptionResources: $listed.objects,
      subscriptionResourceKinds: $listed.kinds,
      jwks: $jwks[0],
      checks: {contract: "node-report/v1", names: []}
    }
JQ
}

post() {
  local status
  printf 'Authorization: Bearer %s\n' "$token" > "$work/headers"
  status=$(curl -sS --proto '=https' --tlsv1.2 --connect-timeout 5 --max-time 20 \
    -X POST -H @"$work/headers" -H 'Content-Type: application/json' \
    --data-binary @"$work/body.json" -o /dev/null -w '%{http_code}' \
    "$REPORT_URL") || status=000
  case $status in
    2[0-9][0-9]) echo "report: the platform took the report" ;;
    *) fail "the platform did not take the report: HTTP $status" ;;
  esac
}

main() {
  : "${REPORT_URL:?names the report endpoint of the platform}" "${SUBSCRIPTION_ID:?names the subscription}" "${HOST_IP:?names the node this pod runs on}"
  local token_file=${REPORT_TOKEN_FILE:-/var/run/mitstack/token} status=0
  [ -s "$token_file" ] || fail "no report token at $token_file"
  token=$(cat "$token_file")
  made_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  collect
  body > "$work/body.json" 2> "$work/read.err" || status=$?
  [ "$status" -eq 0 ] || report_failure body "$status"
  post
}

main "$@"
