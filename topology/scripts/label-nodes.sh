#!/usr/bin/env bash
# Labels each joined node TOPOLOGY_NODES names with its zone and TOPOLOGY_REGION under the field manager
# TOPOLOGY_FIELD_MANAGER, and leaves a node that has not joined to a later run.
# Usage: TOPOLOGY_NODES="<node>=<zone> ..." TOPOLOGY_REGION=<region> TOPOLOGY_FIELD_MANAGER=<manager> label-nodes.sh
# It exists because every Cloud node boots one shared Talos configuration, which cannot name each node's own zone.
set -euo pipefail

: "${TOPOLOGY_NODES:?names each node and its zone}" "${TOPOLOGY_REGION:?names the region}" \
  "${TOPOLOGY_FIELD_MANAGER:?names the field manager}"

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
  kubectl label node "$node" --overwrite --field-manager="$TOPOLOGY_FIELD_MANAGER" \
    "topology.kubernetes.io/zone=$zone" "topology.kubernetes.io/region=$TOPOLOGY_REGION"
done
