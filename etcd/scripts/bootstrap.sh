#!/usr/bin/env bash
# Starts etcd on a cluster's first control-plane node from the gateway, either
# empty or restored from a snapshot. After a restore it deletes every Node
# object outside --keep, and on every exit it deletes the admin talosconfig and
# the kubeconfig derived from it.
# Usage: bootstrap.sh --node <address> --talosconfig <file> [--recover-from <snapshot> --keep <hostname>...]
#   BOOTSTRAP_WAIT_ATTEMPTS and BOOTSTRAP_WAIT_SECONDS bound the wait for the
#   restored API server, 60 attempts 5 seconds apart by default.
# It exists because the bootstrap call accepts only the os:admin role, so the
# credential lives on the gateway for this one call and never on a node, and
# because a restore onto new servers stalls until the old Node objects go.
set -euo pipefail

node="" talosconfig="" snapshot="" keep=()
work=""

fail() {
  echo "bootstrap: $*" >&2
  exit 1
}

usage() {
  echo "usage: bootstrap.sh --node <address> --talosconfig <file> [--recover-from <snapshot> --keep <hostname>...]" >&2
  exit 2
}

cleanup() {
  [ -z "$talosconfig" ] || rm -f "$talosconfig"
  [ -z "$work" ] || rm -rf "$work"
}

parse_args() {
  while [ "$#" -gt 0 ]; do
    case $1 in
      --node) node=${2:?}; shift 2 ;;
      --talosconfig) talosconfig=${2:?}; shift 2 ;;
      --recover-from) snapshot=${2:?}; shift 2 ;;
      --keep) keep+=("${2:?}"); shift 2 ;;
      *) usage ;;
    esac
  done
  if [ -z "$node" ] || [ -z "$talosconfig" ]; then
    usage
  fi
  [ -z "$snapshot" ] || [ "${#keep[@]}" -gt 0 ] || fail "a restore needs --keep for each node of the new control plane"
}

talos() {
  talosctl --talosconfig "$talosconfig" -e "$node" -n "$node" "$@"
}

wait_for_api() {
  local attempts=${BOOTSTRAP_WAIT_ATTEMPTS:-60} attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if kubectl --kubeconfig "$work/kubeconfig" get nodes -o name > "$work/nodes"; then
      return 0
    fi
    sleep "${BOOTSTRAP_WAIT_SECONDS:-5}"
  done
  fail "the restored API server did not answer after $attempts attempts"
}

kept() {
  local name
  for name in "${keep[@]}"; do
    [ "$name" = "$1" ] && return 0
  done
  return 1
}

delete_old_nodes() {
  local name
  while read -r name; do
    name=${name#node/}
    if kept "$name"; then
      continue
    fi
    kubectl --kubeconfig "$work/kubeconfig" delete node "$name"
    echo "bootstrap: deleted the old Node $name"
  done < "$work/nodes"
}

main() {
  trap cleanup EXIT
  parse_args "$@"
  [ -f "$talosconfig" ] || fail "no talosconfig at $talosconfig"

  if [ -z "$snapshot" ]; then
    talos bootstrap
    echo "bootstrap: started etcd on $node"
    return 0
  fi

  [ -f "$snapshot" ] || fail "no snapshot at $snapshot"
  talos bootstrap --recover-from="$snapshot"
  echo "bootstrap: restored etcd on $node from $snapshot"
  work=$(mktemp -d)
  talos kubeconfig "$work/kubeconfig" --force
  wait_for_api
  delete_old_nodes
}

main "$@"
