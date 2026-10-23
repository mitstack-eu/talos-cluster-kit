#!/usr/bin/env bash
# Posts the node report: each node's Talos version and readiness, the etcd members, each Argo CD Application's health,
# CloudNativePG's objects, the API server's key set as jwks, and a checks member naming none of the verdict's checks.
# When a read or the body fails or times out, it posts a report whose error names it instead, then exits non-zero.
# Usage: REPORT_URL=<https url> SUBSCRIPTION_ID=<id> HOST_IP=<this pod's node> [REPORT_TOKEN_FILE=/var/run/mitstack/token] [REPORT_READ_SECONDS=25] report.sh
# It exists so the platform sees a cluster it never dials into, as ADR 0072 requires, with os:reader and no Secret right.
set -euo pipefail

# Six limits of 25 seconds, their 5-second kill grace and the post's 20 fit the Job's activeDeadlineSeconds of 240.
limit=${REPORT_READ_SECONDS:-25}
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
  read_cloudnative_pg
}

# read_cloudnative_pg keeps its exit status and stderr, so a failed read is reported beside the reads above.
read_cloudnative_pg() {
  local status=0
  limited kubectl get clusters.postgresql.cnpg.io,databases.postgresql.cnpg.io,databaseroles.postgresql.cnpg.io -A -o json \
    > "$work/cloudnative-pg.json" 2> "$work/cloudnative-pg.err" || status=$?
  printf '%s' "$status" > "$work/cloudnative-pg.status"
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
    --rawfile cnpg "$work/cloudnative-pg.json" \
    --rawfile cnpg_err "$work/cloudnative-pg.err" \
    --argjson cnpg_status "$(cat "$work/cloudnative-pg.status")" \
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
    def cloudnative_pg_read:
      if $cnpg_status != 0 then {exitStatus: $cnpg_status, stderr: $cnpg_err[-1024:]}
      else try ($cnpg | fromjson | [.items[] | cloudnative_pg])
        catch {exitStatus: 0, stderr: "kubectl exited 0, and its output is not the CloudNativePG objects this report reads"}
      end;
    {
      subscriptionId: $subscription,
      reportedAt: $at,
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
      argocd: {applications: [$applications[0].items[] | {
        name: .metadata.name,
        health: (.status.health.status // "Unknown"),
        sync: (.status.sync.status // "Unknown")
      }]},
      cloudNativePG: cloudnative_pg_read,
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
  collect
  body > "$work/body.json" 2> "$work/read.err" || status=$?
  [ "$status" -eq 0 ] || report_failure body "$status"
  post
}

main "$@"
