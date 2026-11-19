#!/usr/bin/env bash
# Gives each node the machine configuration this release renders for it, without a reboot, one node at a time and the
# control-plane nodes first, and leaves each node whose machine-config annotation names the release as it is.
# Usage: RELEASE=<the release's digest> [NODE_WAIT_SECONDS=300] apply-machine-config.sh
# It exists because a node keeps the configuration it was installed with until something applies a new one, and a
# release that changes a setting a node takes only at reboot must stop before any node changes.
set -euo pipefail

annotation=mitstack.dev/machine-config
node_wait=${NODE_WAIT_SECONDS:-300}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "apply-machine-config: $*" >&2
  exit 1
}

# refuse exits with the code the Job's pod failure policy fails the Job on, since a retry renders the same refusal.
refuse() {
  echo "apply-machine-config: $*" >&2
  exit 3
}

# pending prints "<name> <address> <control|worker>" for each node not at the release, the control-plane nodes first,
# with "-" for a node that lists no address, since read would take an empty field from the next one.
pending() {
  kubectl get nodes -o json >"$work/nodes.json"
  jq -r --arg annotation "$annotation" --arg release "$RELEASE" '
    [.items[] | {
      name: .metadata.name,
      address: ([.status.addresses[]? | select(.type == "InternalIP") | .address | select(. != "")] | first // "-"),
      role: (if .metadata.labels | has("node-role.kubernetes.io/control-plane") then "control" else "worker" end),
      release: (.metadata.annotations[$annotation] // "")
    }] | sort_by(.role != "control", .name)[] | select(.release != $release) | "\(.name) \(.address) \(.role)"
  ' "$work/nodes.json"
}

# control_plane_addresses prints the address of every control-plane node, comma-separated as talosctl takes them, and
# fails on a node that lists none, since etcd read without its member would read healthy.
control_plane_addresses() {
  jq -r '[.items | sort_by(.metadata.name)[] | select(.metadata.labels | has("node-role.kubernetes.io/control-plane"))
    | ([.status.addresses[]? | select(.type == "InternalIP") | .address | select(. != "")] | first) as $address
    | $address // error("the control-plane node \(.metadata.name) lists no internal address to read etcd at")]
    | join(",")' "$work/nodes.json"
}

# render writes the configuration the release gives the node at address into the file name, refusing a reboot-only
# change.
render() {
  local name=$1 address=$2 status=0
  talosctl -n "$address" get machineconfig v1alpha1 -o 'jsonpath={.spec}' >"$work/$name.running" ||
    fail "reading the configuration $name runs failed"
  machine-config --release "$RELEASE" <"$work/$name.running" >"$work/$name.yaml" 2>"$work/$name.err" || status=$?
  case $status in
    0) ;;
    3) refuse "$name at $address: $(cat "$work/$name.err"), so no node changes" ;;
    *) fail "rendering the configuration of $name failed: $(cat "$work/$name.err")" ;;
  esac
  rm -f "$work/$name.running"
}

# etcd_healthy reads Talos's watch of the etcd service on every control-plane node, which prints each node's health
# at once and again at each change, until each reads healthy or NODE_WAIT_SECONDS pass, naming each node that does not.
etcd_healthy() {
  local fd pid node healthy left
  local -A unhealthy=()
  for node in ${1//,/ }; do unhealthy[$node]=1; done
  exec {fd}< <(exec talosctl -n "$1" get service etcd --watch -o 'jsonpath={.node} {.spec.healthy}')
  pid=$!
  local deadline=$((SECONDS + node_wait))
  while [ "${#unhealthy[@]}" -gt 0 ] && left=$((deadline - SECONDS)) && [ "$left" -gt 0 ] &&
    read -r -t "$left" -u "$fd" node healthy; do
    if [ "$healthy" = true ]; then unset "unhealthy[$node]"; else unhealthy[$node]=1; fi
  done
  kill "$pid" 2>/dev/null || true
  exec {fd}<&-
  printf '%s\n' "${!unhealthy[@]}" >"$work/unhealthy.txt"
  [ "${#unhealthy[@]}" -eq 0 ]
}

# at_release waits on Kubernetes' watch of the node until its annotation names the release and it reads Ready.
at_release() {
  kubectl wait --for="jsonpath={.metadata.annotations.mitstack\.dev/machine-config}=$RELEASE" --timeout="${node_wait}s" "node/$1" &&
    kubectl wait --for=condition=Ready --timeout="${node_wait}s" "node/$1"
}

main() {
  : "${RELEASE:?names the release each node takes}"
  local nodes name address role control_planes
  nodes=$(pending)
  if [ -z "$nodes" ]; then
    echo "apply-machine-config: every node runs $RELEASE"
    return
  fi
  control_planes=$(control_plane_addresses) || fail "no node changes"
  [ -n "$control_planes" ] || fail "the cluster lists no control-plane node to read etcd from"
  while read -r -u 3 name address role; do
    [ "$address" != - ] || fail "$name lists no internal address to reach its Talos API at"
    render "$name" "$address"
  done 3<<<"$nodes"
  while read -r -u 3 name address role; do
    etcd_healthy "$control_planes" ||
      fail "etcd does not read healthy on $(sort "$work/unhealthy.txt" | paste -sd,), so $name keeps its configuration"
    talosctl -n "$address" apply-config --mode no-reboot --file "$work/$name.yaml"
    rm -f "$work/$name.yaml"
    at_release "$name" >/dev/null || fail "$name does not run $RELEASE and read Ready within ${node_wait}s of taking the configuration"
    echo "apply-machine-config: the $role node $name runs $RELEASE"
  done 3<<<"$nodes"
}

main "$@"
