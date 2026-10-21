#!/usr/bin/env bash
# Reads the cluster and posts the node report to the platform: each node with
# its Talos version and readiness, the etcd membership, and the health and
# sync status of every Argo CD Application.
# Usage: REPORT_URL=<https url> SUBSCRIPTION_ID=<id> HOST_IP=<this pod's node> report.sh
#   REPORT_TOKEN_FILE names the bearer token's file, /var/run/mitstack/token by default.
# It exists so the platform sees a cluster it never dials into, as ADR 0072
# requires, from reads that hold the Talos os:reader role and no Secret right.
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "report: $*" >&2
  exit 1
}

collect() {
  kubectl get nodes -o json > "$work/nodes.json"
  kubectl get applications.argoproj.io -n argocd -o json > "$work/applications.json"
  talosctl -n "$HOST_IP" etcd members > "$work/members.txt"
}

# body prints the report; a member line is NODE ID HOSTNAME PEER-URLS CLIENT-URLS LEARNER.
body() {
  jq -n \
    --arg subscription "$SUBSCRIPTION_ID" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --slurpfile nodes "$work/nodes.json" \
    --slurpfile applications "$work/applications.json" \
    --rawfile members "$work/members.txt" '
    def talos_version: (.status.nodeInfo.osImage // "") | (try capture("^Talos \\((?<v>[^)]+)\\)").v catch null);
    def ready: [.status.conditions[]? | select(.type == "Ready") | .status == "True"] | any;
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
      }]}
    }'
}

post() {
  local token_file=${REPORT_TOKEN_FILE:-/var/run/mitstack/token} status
  [ -s "$token_file" ] || fail "no report token at $token_file"
  printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" > "$work/headers"
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
  collect
  body > "$work/body.json"
  post
}

main "$@"
