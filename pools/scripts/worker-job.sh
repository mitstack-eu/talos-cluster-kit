#!/usr/bin/env bash
# Takes the Cloud worker NODE out of the cluster for its replacement, one STEP at a time. "drain" evicts every pod but
# a DaemonSet's under the pods' disruption budgets and ends naming the budget that holds one. "remove-node" releases
# each claim on a local volume of NODE, whose data goes with its server, then deletes the Node object.
# Usage: STEP=drain|remove-node NODE=<worker> NODE_NAME=<this pod's node> JOB=<this Job> JOB_NAMESPACE=<its namespace>
#   ATTEMPTS=<the Job's attempts> DRAIN_TIMEOUT=<one attempt's drain> SERVICE_ACCOUNT_DIR=<the pod's token> worker-job.sh
# It exists because a replacement must not strand a pod: a released claim lets its workload make a new one on another
# node and fill it from its own copies, where a claim left bound would hold its pod to a volume that is gone.
set -euo pipefail

fail() {
  echo "$STEP: $*" >&2
  exit 1
}

# budget_of <namespace> <pod> prints the cause the API gives for refusing a dry-run eviction of the pod, nothing when
# it allows the eviction or the pod is gone, and fails on any other answer, so no refusal reads as a budget's absence.
budget_of() {
  local answer verdict
  answer=$(curl -sS --connect-timeout 5 --max-time 30 --cacert "$SERVICE_ACCOUNT_DIR/ca.crt" \
    -H "Authorization: Bearer $(cat "$SERVICE_ACCOUNT_DIR/token")" -H 'Content-Type: application/json' -X POST \
    --data "{\"apiVersion\":\"policy/v1\",\"kind\":\"Eviction\",\"metadata\":{\"name\":\"$2\",\"namespace\":\"$1\"}}" \
    "https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT/api/v1/namespaces/$1/pods/$2/eviction?dryRun=All") ||
    fail "the dry-run eviction of pod $1/$2 got no answer"
  verdict=$(jq -r --arg pod "$1/$2" 'if .code == 429 then [.details.causes[]? | select(.reason == "DisruptionBudget") | .message] | first // empty
    elif .status == "Success" or .code == 404 then empty
    else error("the dry-run eviction of pod \($pod) was answered \(.code // "with no code"): \(.message // "")") end' <<<"$answer" 2>&1) ||
    fail "${verdict#jq: error (at <stdin>:*): }"
  printf '%s\n' "$verdict"
}

drain() {
  if kubectl drain "$NODE" --ignore-daemonsets --delete-emptydir-data --force --timeout="$DRAIN_TIMEOUT"; then
    return 0
  fi
  local failed pods pod cause named=""
  # An attempt that cannot read its Job's count takes itself for the last, since a cordon left on would outlive the Job.
  failed=$(kubectl get job "$JOB" -n "$JOB_NAMESPACE" -o jsonpath='{.status.failed}') || failed=$((ATTEMPTS - 1))
  if [ $((${failed:-0} + 1)) -ge "$ATTEMPTS" ]; then
    kubectl uncordon "$NODE" >/dev/null
  fi
  pods=$(kubectl get pods -A --field-selector "spec.nodeName=$NODE" -o json |
    jq -r '.items[] | select(all(.metadata.ownerReferences[]?; .kind != "DaemonSet")) | "\(.metadata.namespace)/\(.metadata.name)"')
  for pod in $pods; do
    cause=$(budget_of "${pod%%/*}" "${pod#*/}")
    if [ -n "$cause" ]; then
      echo "drain: pod $pod waits on a disruption budget in namespace ${pod%%/*}: $cause" >&2
      named=yes
    fi
  done
  [ -n "$named" ] || fail "kubectl drain of $NODE failed, and no disruption budget refuses a pod left on it"
  exit 1
}

release_local_claims() {
  local volumes namespace claim uid current pods pod
  volumes=$(kubectl get pv -o json | jq -r --arg node "$NODE" '.items[]
    | select(.spec.claimRef != null and (.spec.local != null or .spec.hostPath != null))
    | select([.spec.nodeAffinity.required.nodeSelectorTerms[]?.matchExpressions[]?
        | select(.key == "kubernetes.io/hostname") | .values[]] == [$node])
    | "\(.spec.claimRef.namespace) \(.spec.claimRef.name) \(.spec.claimRef.uid)"')
  while read -r namespace claim uid; do
    [ -n "$namespace" ] || continue
    current=$(kubectl get pvc "$claim" -n "$namespace" -o jsonpath='{.metadata.uid}' --ignore-not-found)
    # A claim made anew under the same name belongs to the workload's new copy, which this step leaves alone.
    [ -z "$current" ] || [ "$current" = "$uid" ] || continue
    [ -z "$current" ] || kubectl delete pvc "$claim" -n "$namespace" --wait=false
    pods=$(kubectl get pods -n "$namespace" -o json | jq -r --arg claim "$claim" --arg node "$NODE" '.items[]
      | select((.spec.nodeName // $node) == $node and any(.spec.volumes[]?; .persistentVolumeClaim.claimName == $claim))
      | .metadata.name')
    for pod in $pods; do
      kubectl delete pod "$pod" -n "$namespace" --wait=false
    done
  done <<<"$volumes"
}

[ "$NODE" != "$NODE_NAME" ] || fail "refused, since this pod runs on $NODE, the node it acts on"
case "$STEP" in
  drain) drain ;;
  remove-node)
    release_local_claims
    kubectl delete node "$NODE" --ignore-not-found
    ;;
  *) fail "no such step" ;;
esac
