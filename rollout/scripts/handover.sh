#!/usr/bin/env bash
# Gives up Talos's fields on each object HANDOVER_OBJECTS names, so the object holds only what Argo CD applies.
# Usage: HANDOVER_OBJECTS='<a v1 List naming each object by apiVersion, kind, name and namespace>' \
#   HANDOVER_TALOS_MANAGER=<manager> HANDOVER_ARGOCD_MANAGER=<manager> handover.sh
# It exists because Talos creates each object by server-side apply under its own field manager and never applies it
# again, so a field a later release drops stays on the object for as long as that manager owns it.
set -euo pipefail

: "${HANDOVER_OBJECTS:?names the objects}" "${HANDOVER_TALOS_MANAGER:?names the field manager Talos creates under}" \
  "${HANDOVER_ARGOCD_MANAGER:?names the field manager Argo CD applies under}"
export HANDOVER_TALOS_MANAGER HANDOVER_ARGOCD_MANAGER

readonly talos=$HANDOVER_TALOS_MANAGER
readonly filters='
  def objects: if .kind == "List" then .items[] else . end;
  def applied_by(manager): any(.metadata.managedFields[]?; .manager == manager and .operation == "Apply");
  def held: objects | select(applied_by(env.HANDOVER_TALOS_MANAGER));
  def unapplied: held | select(applied_by(env.HANDOVER_ARGOCD_MANAGER) | not);
  def written: objects
    | select(any(.metadata.managedFields[]?; .manager == env.HANDOVER_TALOS_MANAGER and .operation != "Apply"));
  def named: "\(.kind) \(if .metadata.namespace then .metadata.namespace + "/" else "" end)\(.metadata.name)";
  def stub: {apiVersion, kind, metadata: (.metadata | {name} + if .namespace then {namespace} else {} end)};
'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "handover: $1" >&2
  exit 1
}

# read_objects <list> <out> writes each object <list> names to <out>, with its field managers.
read_objects() {
  kubectl get -f "$1" --show-managed-fields -o json > "$2" || fail "cannot read the objects the component states"
}

# query <objects> <filter> prints what <filter> reads of the objects kubectl wrote to <objects>.
query() {
  jq -r "$filters $2" "$1"
}

# names <objects> <selection> prints the objects <selection> picks, by kind and name on one line.
names() {
  query "$1" "[$2 | named] | join(\", \")"
}

printf '%s' "$HANDOVER_OBJECTS" > "$work/stated.json"
read_objects "$work/stated.json" "$work/live.json"

unapplied=$(names "$work/live.json" unapplied)
[ -z "$unapplied" ] || fail "Argo CD has not applied $unapplied, so giving up $talos's fields would take every field away"

written=$(names "$work/live.json" written)
[ -z "$written" ] || fail "$written carried $talos as a field manager for a write that is no server-side apply, which an apply cannot give up"

held=$(names "$work/live.json" held)
if [ -z "$held" ]; then
  echo "handover: no object names $talos as a field manager"
  exit 0
fi

# An apply that states no field gives up each field its manager owns, and the API server then drops every field no
# other manager owns. kubectl's own validation would refuse a document without its kind's required fields.
query "$work/live.json" '{apiVersion: "v1", kind: "List", items: [held | stub]}' > "$work/held.json"
kubectl apply --server-side --validate=false --field-manager="$talos" -f "$work/held.json" \
  || fail "the API server refused the apply that gives up $talos's fields on $held"

read_objects "$work/held.json" "$work/after.json"
still=$(names "$work/after.json" held)
[ -z "$still" ] || fail "$still still carried $talos as a field manager after the apply"
echo "handover: $talos no longer owns a field of $held"
