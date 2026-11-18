#!/usr/bin/env bash
# Labels each joined node TOPOLOGY_NODES names with its zone and TOPOLOGY_REGION under the field manager
# TOPOLOGY_FIELD_MANAGER, labels each of TOPOLOGY_BURST_NODES a burst node and takes the label off every other, and
# leaves a node that has not joined to a later run. Once every node is labelled, it evicts from each burst node every
# pod whose placement refuses a burst node. An eviction a disruption budget holds is named and retried by the next run,
# which this one fails for; a pod already gone counts as moved; any other failure ends the run.
# Usage: TOPOLOGY_NODES="<node>=<zone> ..." TOPOLOGY_REGION=<region> TOPOLOGY_FIELD_MANAGER=<manager> \
#   [TOPOLOGY_BURST_NODES="<node> ..."] label-nodes.sh
# It exists because every Cloud node boots one shared Talos configuration, which cannot name each node's own zone or
# whether the Cloud pool counts it among its minimum.
set -euo pipefail

: "${TOPOLOGY_NODES:?names each node and its zone}" "${TOPOLOGY_REGION:?names the region}" \
  "${TOPOLOGY_FIELD_MANAGER:?names the field manager}"

readonly burst_label=mitstack.dev/burst
held=0

is_burst() {
  case " ${TOPOLOGY_BURST_NODES:-} " in *" $1 "*) return 0 ;; esac
  return 1
}

# refusing_pods prints the namespace and name of each pod in the pod list on stdin that runs on, and every required
# node affinity term of which keeps it off, a burst node.
refusing_pods() {
  jq -r --arg key "$burst_label" '.items[]
    | select(.metadata.deletionTimestamp == null and (.status.phase | IN("Succeeded", "Failed") | not))
    | select((.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // []) as $terms
      | ($terms | length) > 0 and all($terms[]; any(.matchExpressions[]?; .key == $key and .operator == "DoesNotExist")))
    | "\(.metadata.namespace) \(.metadata.name)"'
}

evict() {
  local node=$1 namespace=$2 name=$3 body err
  body=$(jq -n --arg namespace "$namespace" --arg name "$name" \
    '{apiVersion: "policy/v1", kind: "Eviction", metadata: {name: $name, namespace: $namespace}}')
  if err=$(kubectl create --raw "/api/v1/namespaces/$namespace/pods/$name/eviction" -f - <<<"$body" 2>&1 >/dev/null); then
    echo "label-nodes: evicted $namespace/$name from burst node $node"
    return
  fi
  case $err in
    *"(NotFound): pods \"$name\" not found"*)
      echo "label-nodes: $namespace/$name left burst node $node before its eviction" ;;
    *TooManyRequests*)
      echo "label-nodes: $namespace/$name stays on burst node $node until its disruption budget allows: $err" >&2
      held=1 ;;
    *)
      echo "label-nodes: cannot evict $namespace/$name from burst node $node: $err" >&2
      exit 1 ;;
  esac
}

evict_refusing() {
  local node=$1 pods namespace name
  if ! pods=$(kubectl get pods -A --field-selector "spec.nodeName=$node" -o json); then
    echo "label-nodes: cannot read the pods on burst node $node" >&2
    exit 1
  fi
  pods=$(refusing_pods <<<"$pods")
  while read -r namespace name; do
    [ -n "$namespace" ] && evict "$node" "$namespace" "$name"
  done <<<"$pods"
}

joined_burst=()
for pair in $TOPOLOGY_NODES; do
  node=${pair%%=*}
  zone=${pair#*=}
  if ! found=$(kubectl get node "$node" --ignore-not-found -o name); then
    echo "label-nodes: cannot read node $node" >&2
    exit 1
  fi
  if [ -z "$found" ]; then
    echo "label-nodes: $node has not joined, so a later run labels it"
    continue
  fi
  burst="$burst_label-"
  is_burst "$node" && burst="$burst_label=true"
  kubectl label node "$node" --overwrite --field-manager="$TOPOLOGY_FIELD_MANAGER" \
    "topology.kubernetes.io/zone=$zone" "topology.kubernetes.io/region=$TOPOLOGY_REGION" "$burst"
  if is_burst "$node"; then
    joined_burst+=("$node")
  fi
done

for node in "${joined_burst[@]}"; do
  evict_refusing "$node"
done

if [ "$held" -ne 0 ]; then
  echo "label-nodes: a disruption budget holds a pod on a burst node, so a later run evicts it" >&2
  exit 1
fi
