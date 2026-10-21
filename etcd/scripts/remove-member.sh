#!/usr/bin/env bash
# Removes the etcd member a lost control-plane node held, then deletes that
# node's Node object, in that order and only once the removal succeeded.
# Usage: MEMBER_HOSTNAME=<lost node> HOST_IP=<this pod's node> NODE_NAME=<this pod's node name> remove-member.sh
# It exists because a replacement or a restore stalls while a dead node's
# Node object stays, and the etcd member must go first so the cluster never
# counts a member whose node is already forgotten.
set -euo pipefail

fail() {
  echo "remove-member: $*" >&2
  exit 1
}

# member_id <hostname> prints the etcd member id that hostname holds, or nothing.
member_id() {
  talosctl -n "$HOST_IP" etcd members | awk -v host="$1" 'NR > 1 && $3 == host { print $2 }'
}

main() {
  : "${MEMBER_HOSTNAME:?names the lost node}" "${HOST_IP:?names the node this pod runs on}" "${NODE_NAME:?names the node this pod runs on}"
  [ "$MEMBER_HOSTNAME" != "$NODE_NAME" ] || fail "refusing to remove $MEMBER_HOSTNAME, the node this Job runs on"

  local id
  id=$(member_id "$MEMBER_HOSTNAME")
  if [ -z "$id" ]; then
    echo "remove-member: no etcd member is named $MEMBER_HOSTNAME"
  else
    talosctl -n "$HOST_IP" etcd remove-member "$id"
    echo "remove-member: removed etcd member $id of $MEMBER_HOSTNAME"
  fi

  kubectl delete node "$MEMBER_HOSTNAME" --ignore-not-found
  echo "remove-member: deleted the Node $MEMBER_HOSTNAME"
}

main "$@"
