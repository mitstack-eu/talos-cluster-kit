#!/usr/bin/env bash
# Moves each OpenBao member whose pod runs an older revision of its StatefulSet onto the current one, one member at a
# time, the standbys first and the active member last, and stops at the first member that does not return caught up.
# Usage: ROLLOUT_NAMESPACE=<namespace> ROLLOUT_STATEFULSET=<name> ROLLOUT_MEMBER_TIMEOUT=<seconds>s \
#   ROLLOUT_CALL_TIMEOUT=<seconds>s openbao-rollout.sh
# It exists because OpenBao's StatefulSet updates OnDelete, as OpenBao requires so that no member fails over to an
# older one, so nothing else replaces a member's pod once a release changes its configuration or its image.
set -euo pipefail

: "${ROLLOUT_NAMESPACE:?names the namespace OpenBao runs in}" "${ROLLOUT_STATEFULSET:?names the StatefulSet of the members}" \
  "${ROLLOUT_MEMBER_TIMEOUT:?bounds the return of each member after its eviction}" \
  "${ROLLOUT_CALL_TIMEOUT:?bounds each read inside a member and each eviction}"

readonly namespace=$ROLLOUT_NAMESPACE statefulset=$ROLLOUT_STATEFULSET call_timeout=$ROLLOUT_CALL_TIMEOUT
readonly timeout=$ROLLOUT_MEMBER_TIMEOUT member_seconds=${ROLLOUT_MEMBER_TIMEOUT%s}
[[ $member_seconds =~ ^[0-9]+$ ]] || { echo "openbao-rollout: ROLLOUT_MEMBER_TIMEOUT is $timeout, not a number of seconds such as 300s" >&2; exit 1; }
readonly outdated='.metadata.labels["controller-revision-hash"] != env.ROLLOUT_REVISION'
readonly serving='any(.status.conditions[]?; .type == "Ready" and .status == "True") and .metadata.labels["openbao-sealed"] == "false"'
readonly active='.metadata.labels["openbao-active"] == "true"'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "openbao-rollout: $1" >&2
  exit 1
}

# members <selection> prints the name of each member <selection> picks, in name order, one to a line.
members() {
  jq -r "[.items[] | select($1) | .metadata.name] | sort | .[]" "$work/pods.json"
}

# raft_index <member> <field> prints <field> of sys/leader as <member> answers it on its own listener, with the TLS
# handling of its readiness probe. The path needs no token, and the exec runs through the API server and the kubelet.
raft_index() {
  local answer index
  answer=$(timeout "$call_timeout" kubectl exec -n "$namespace" "$1" -c "$container" -- bao read -tls-skip-verify -format=json sys/leader) \
    || fail "cannot read sys/leader inside $1"
  index=$(jq -r --arg field "$2" '.data[$field] // empty' <<<"$answer" 2> /dev/null) || index=""
  [[ $index =~ ^[0-9]+$ ]] || fail "$1 answered sys/leader without $2"
  echo "$index"
}

# evict <member> asks the API server to evict the member's pod, which it refuses while OpenBao's disruption budget
# allows no disruption. kubectl's --request-timeout would take it off the in-cluster configuration, so timeout bounds it.
evict() {
  printf '{"apiVersion": "policy/v1", "kind": "Eviction", "metadata": {"name": "%s", "namespace": "%s"}}\n' "$1" "$namespace" > "$work/eviction.json"
  timeout "$call_timeout" kubectl create --raw "/api/v1/namespaces/$namespace/pods/$1/eviction" -f "$work/eviction.json" > /dev/null \
    || fail "the API server refused to evict $1"
}

# await <member> reads kubectl's watch of the member's pod, which prints its revision, readiness and seal label at each
# change, until the pod runs the update revision, Ready and unsealed. kubectl's own wait gives up at once while no pod
# holds the name, which is so between the old pod's deletion and the new one's creation, so one watch spans both.
await() {
  local path='{.metadata.labels.controller-revision-hash} {.status.conditions[?(@.type=="Ready")].status} {.metadata.labels.openbao-sealed}{"\n"}'
  local fd pid state left found=2 deadline=$((SECONDS + member_seconds))
  exec {fd}< <(exec kubectl get pods -n "$namespace" --field-selector="metadata.name=$1" --watch -o "jsonpath=$path")
  pid=$!
  while left=$((deadline - SECONDS)); [ "$left" -gt 0 ] || { found=1; false; }; do
    IFS= read -r -t "$left" -u "$fd" state || { [ $? -gt 128 ] && found=1; break; }
    [ "$state" != "$revision True false" ] || { found=0; break; }
  done
  kill "$pid" 2> /dev/null || true
  exec {fd}<&-
  case $found in
    1) fail "$1 did not run revision $revision Ready and unsealed within $timeout of its eviction, so no other member moves" ;;
    2) fail "kubectl's watch of $1 ended before it ran revision $revision Ready and unsealed, so no other member moves" ;;
  esac
}

kubectl get statefulset "$statefulset" -n "$namespace" -o json > "$work/statefulset.json" \
  || fail "cannot read the StatefulSet $namespace/$statefulset"
revision=$(jq -r '.status.updateRevision // empty' "$work/statefulset.json")
[ -n "$revision" ] || fail "the StatefulSet $namespace/$statefulset names no update revision"
export ROLLOUT_REVISION=$revision
replicas=$(jq -r '.spec.replicas' "$work/statefulset.json")
container=$(jq -r '.spec.template.spec.containers[0].name' "$work/statefulset.json")
selector=$(jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")' "$work/statefulset.json")
kubectl get pods -n "$namespace" -l "$selector" -o json > "$work/pods.json" || fail "cannot read the members of $namespace/$statefulset"

mapfile -t stale < <(members "$outdated")
if [ "${#stale[@]}" -eq 0 ]; then
  echo "openbao-rollout: every member runs revision $revision"
  exit 0
fi
mapfile -t all < <(members true)
[ "${#all[@]}" = "$replicas" ] || fail "${#all[@]} of $replicas members exist, so moving one now could cost the quorum"
down=$(members "($serving) | not" | paste -sd ' ' -)
[ -z "$down" ] || fail "$down is not Ready and unsealed, so moving another member now could cost the quorum"
mapfile -t leader < <(members "$active")
[ "${#leader[@]}" = 1 ] || fail "${#leader[@]} members are labelled active, want one, so the standbys cannot be told apart"
mapfile -t order < <(members "($outdated) and (($active) | not)")
[[ " ${stale[*]} " != *" ${leader[0]} "* ]] || order+=("${leader[0]}")
echo "openbao-rollout: moving ${order[*]} onto revision $revision one at a time, the active member ${leader[0]} last"

for member in "${order[@]}"; do
  committed=$(raft_index "${leader[0]}" raft_committed_index)
  evict "$member"
  echo "openbao-rollout: evicted $member after ${leader[0]} committed Raft index $committed, and waiting up to $timeout for its return"
  await "$member"
  applied=$(raft_index "$member" raft_applied_index)
  [ "$applied" -ge "$committed" ] \
    || fail "$member has applied Raft index $applied, short of the $committed ${leader[0]} committed before its eviction, so no other member moves"
  echo "openbao-rollout: $member runs revision $revision, unsealed, at Raft index $applied"
done
