#!/usr/bin/env bash
# Gives up Talos's fields on each object HANDOVER_OBJECTS names, so the object holds only what Argo CD applies.
# Usage: HANDOVER_OBJECTS='<a v1 List naming each object by apiVersion, kind, name and namespace>' \
#   HANDOVER_TALOS_MANAGER=<manager> HANDOVER_ARGOCD_MANAGER=<manager> \
#   HANDOVER_ARGOCD_ANNOTATION=<annotation> HANDOVER_ADOPTION_TIMEOUT=<seconds>s handover.sh
# It exists because Talos creates each object by server-side apply under its own field manager and never applies it
# again, so a field a later release drops stays on the object for as long as that manager owns it.
set -euo pipefail

: "${HANDOVER_OBJECTS:?names the objects}" "${HANDOVER_TALOS_MANAGER:?names the field manager Talos creates under}" \
  "${HANDOVER_ARGOCD_MANAGER:?names the field manager Argo CD applies under}" \
  "${HANDOVER_ARGOCD_ANNOTATION:?names the annotation Argo CD gives each object it applies}" \
  "${HANDOVER_ADOPTION_TIMEOUT:?bounds the wait for Argo CD to apply each object}"
export HANDOVER_TALOS_MANAGER HANDOVER_ARGOCD_MANAGER HANDOVER_ARGOCD_ANNOTATION

readonly talos=$HANDOVER_TALOS_MANAGER timeout=$HANDOVER_ADOPTION_TIMEOUT budget=${HANDOVER_ADOPTION_TIMEOUT%s}
[[ $budget =~ ^[0-9]+$ ]] || { echo "handover: HANDOVER_ADOPTION_TIMEOUT is $timeout, not a number of seconds such as 120s" >&2; exit 1; }
readonly filters='
  def objects: if .kind == "List" then .items[] else . end;
  def applied_by(manager): any(.metadata.managedFields[]?; .manager == manager and .operation == "Apply");
  def held: objects | select(applied_by(env.HANDOVER_TALOS_MANAGER));
  def unapplied: held | select(applied_by(env.HANDOVER_ARGOCD_MANAGER) | not);
  def tracked: .metadata.annotations[env.HANDOVER_ARGOCD_ANNOTATION] != null;
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

# await_argocd <list> waits until each object <list> names exists and Argo CD has annotated it, since the Job runs in
# the wave of the component's last object and Argo CD's sync creates and applies the rest of that wave beside it. Each
# object gets a wait of its own, because kubectl's wait for creation gives up at once when two objects of one file are
# absent, and every wait spends what is left of the one timeout.
await_argocd() {
  local path="{.metadata.annotations.${HANDOVER_ARGOCD_ANNOTATION//./\\.}}" deadline=$((SECONDS + budget)) object i=0
  while IFS= read -r object; do
    i=$((i + 1))
    printf '%s' "$object" > "$work/awaited-$i.json"
    kubectl wait -f "$work/awaited-$i.json" --for=create --for="jsonpath=$path" --timeout="$(remaining "$deadline")s" \
      || refuse_pending "$1"
  done < <(query "$1" 'objects | tojson')
}

# remaining <deadline> prints the seconds left until <deadline>, and 1 at the least, since kubectl reads 0 as no wait.
remaining() {
  local left=$(($1 - SECONDS))
  echo $((left > 0 ? left : 1))
}

# refuse_pending <list> fails naming each object <list> names that does not exist or lacks Argo CD's annotation.
refuse_pending() {
  kubectl get -f "$1" --ignore-not-found -o json > "$work/waited.json" || fail "cannot read the objects the component states"
  local pending
  pending=$(jq -r --slurpfile live "$work/waited.json" \
    "$filters [objects | named] - [\$live[0] | objects | select(tracked) | named] | join(\", \")" "$1")
  [ -n "$pending" ] || fail "the wait for Argo CD's annotation failed although each object carries it"
  fail "Argo CD has not applied $pending within $timeout, so giving up $talos's fields could take every field away"
}

printf '%s' "$HANDOVER_OBJECTS" > "$work/stated.json"
await_argocd "$work/stated.json"
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
