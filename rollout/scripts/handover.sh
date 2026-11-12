#!/usr/bin/env bash
# Gives up Talos's fields on each object HANDOVER_OBJECTS names, so the object holds only what Argo CD applies.
# Usage: HANDOVER_OBJECTS='<a v1 List naming each object by apiVersion, kind, name and namespace>' \
#   HANDOVER_TALOS_MANAGER=<manager> HANDOVER_ARGOCD_MANAGER=<manager> HANDOVER_ADOPTION_TIMEOUT=<seconds>s handover.sh
# It exists because Talos creates each object by server-side apply under its own field manager and never applies it
# again, so a field a later release drops stays on the object for as long as that manager owns it.
set -euo pipefail

: "${HANDOVER_OBJECTS:?names the objects}" "${HANDOVER_TALOS_MANAGER:?names the field manager Talos creates under}" \
  "${HANDOVER_ARGOCD_MANAGER:?names the field manager Argo CD applies under}" \
  "${HANDOVER_ADOPTION_TIMEOUT:?bounds the wait for Argo CD to apply each object}"
export HANDOVER_TALOS_MANAGER HANDOVER_ARGOCD_MANAGER

readonly talos=$HANDOVER_TALOS_MANAGER timeout=$HANDOVER_ADOPTION_TIMEOUT budget=${HANDOVER_ADOPTION_TIMEOUT%s}
[[ $budget =~ ^[0-9]+$ ]] || { echo "handover: HANDOVER_ADOPTION_TIMEOUT is $timeout, not a number of seconds such as 120s" >&2; exit 1; }
readonly filters='
  def objects: if .kind == "List" then .items[] else . end;
  def applied_by(manager): any(.metadata.managedFields[]?; .manager == manager and .operation == "Apply");
  def held: objects | select(applied_by(env.HANDOVER_TALOS_MANAGER));
  def adopted: applied_by(env.HANDOVER_ARGOCD_MANAGER);
  def unapplied: held | select(adopted | not);
  def written: objects
    | select(any(.metadata.managedFields[]?; .manager == env.HANDOVER_TALOS_MANAGER and .operation != "Apply"));
  def resource: "\(.kind | ascii_downcase).\(.apiVersion | split("/") | "\(.[-1]).\(.[:-1] | join(""))")";
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

# await_argocd <list> watches each object <list> names until Argo CD has applied it, since the Job runs in the wave of
# the component's last object and Argo CD's sync creates and applies the rest of that wave beside it. Argo CD's field
# manager marks an apply on every kind, where its tracking annotation is on no CustomResourceDefinition. kubectl's wait
# gives up at once on an object that does not exist yet, and its wait for creation polls, so a watch the API server
# wakes on each change of the object stands in for it. Every watch spends what is left of the one timeout.
await_argocd() {
  local deadline=$((SECONDS + budget)) resource name namespace
  echo "handover: waiting up to $timeout for Argo CD to apply each object the component states"
  while IFS=$'\t' read -r resource name namespace; do
    watch_apply "$resource" "$name" "$namespace" "$deadline" || case $? in
      1) refuse_pending "$1" ;;
      *) fail "kubectl's watch of $resource $name ended before Argo CD applied it" ;;
    esac
  done < <(query "$1" 'objects | [resource, .metadata.name, .metadata.namespace // ""] | @tsv')
}

# watch_apply <resource> <name> <namespace> <deadline> reads kubectl's watch of one object, which prints the operations
# Argo CD's field manager names at each change, until one is an apply. It returns 1 when <deadline> passes first and 2
# when the watch ends first.
watch_apply() {
  local path="{.metadata.managedFields[?(@.manager==\"$HANDOVER_ARGOCD_MANAGER\")].operation}"
  local args fd pid operations left found=2
  args=(get "$1" --field-selector="metadata.name=$2" --watch -o "jsonpath=$path{\"\\n\"}")
  [ -z "$3" ] || args+=(-n "$3")
  exec {fd}< <(exec kubectl "${args[@]}")
  pid=$!
  while left=$(($4 - SECONDS)); [ "$left" -gt 0 ] || { found=1; false; }; do
    IFS= read -r -t "$left" -u "$fd" operations || { [ $? -gt 128 ] && found=1; break; }
    [[ " $operations " != *" Apply "* ]] || { found=0; break; }
  done
  kill "$pid" 2> /dev/null || true
  exec {fd}<&-
  return "$found"
}

# refuse_pending <list> fails naming each object <list> names that does not exist or that Argo CD has not applied.
refuse_pending() {
  kubectl get -f "$1" --ignore-not-found --show-managed-fields -o json > "$work/waited.json" \
    || fail "cannot read the objects the component states"
  local pending
  pending=$(jq -r --slurpfile live "$work/waited.json" \
    "$filters [objects | named] - [\$live[0] | objects | select(adopted) | named] | join(\", \")" "$1")
  [ -n "$pending" ] || fail "the wait for Argo CD's apply failed although Argo CD has applied each object"
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
