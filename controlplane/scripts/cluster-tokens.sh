#!/usr/bin/env bash
# Fetches the cluster's own credentials from the platform. enrol spends the
# one-time enrolment token for the report token, Argo CD's notification token
# and the cluster repository's token; renew trades the report token for fresh
# tokens for the cluster repository and each app slice's repository; lease
# trades it, in one call, for the telemetry Collector's push token whenever
# the one its Secret holds has 11 hours or fewer left. Each token goes into
# the Secret that uses it. A cluster whose REPOSITORY_CREDENTIAL is deploy-key
# reads its repository with a sealed deploy key, so enrol writes no
# repository token.
# Usage: PLATFORM_URL=<https origin> SUBSCRIPTION_ID=<id> REPO_URL=<url>
#   REPOSITORY_CREDENTIAL=platform-token|deploy-key cluster-tokens.sh enrol|renew
#   PLATFORM_URL=<https origin> LEASE_RESOURCE=<resource type> cluster-tokens.sh lease
#   TOKEN_FILE names the bearer token's file, /var/run/mitstack/token by
#   default, and CALL_ATTEMPTS how often enrol and renew retry a 429, a 503 or
#   no answer, 20.
# It exists because nothing may dial into the cluster, so the cluster asks for
# what it needs.
set -euo pipefail

readonly token_file=${TOKEN_FILE:-/var/run/mitstack/token}
readonly attempts=${CALL_ATTEMPTS:-20}
readonly node_namespace=mitstack-node report_secret=mitstack-report-token
readonly argocd_namespace=argocd repository_secret=cluster-repository notifications_secret=argocd-notifications-secret
readonly kube_api=${KUBE_API:-https://kubernetes.default.svc}
readonly kube_account=${KUBE_ACCOUNT_DIR:-/var/run/secrets/kubernetes.io/serviceaccount}
readonly push_namespace=kube-system push_secret=mitstack-telemetry-token lease_expiry=mitstack.dev/lease-expires-at
readonly renew_within=$((11 * 3600)) longest_lease=$((24 * 3600))

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "cluster-tokens: $*" >&2
  exit 1
}

# post <path> <body file> prints the platform's status and writes its answer to $work/answer, retrying a passing refusal.
post() {
  local attempt status
  [ -s "$token_file" ] || fail "no bearer token at $token_file"
  printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" > "$work/headers"
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    status=$(curl -sS --proto '=https' --tlsv1.2 --connect-timeout 5 --max-time 30 -X POST \
      -H @"$work/headers" -H 'Content-Type: application/json' --data-binary @"$2" \
      -o "$work/answer" -w '%{http_code}' "$PLATFORM_URL$1" || echo 000)
    case $status in
      429 | 503 | 000) sleep 15 ;;
      *) echo "$status"; return 0 ;;
    esac
  done
  echo "$status"
}

# post_once <path> <body file> prints the platform's status for one call, 000 for no answer, and writes its answer to $work/answer.
post_once() {
  [ -s "$token_file" ] || fail "no bearer token at $token_file"
  printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" > "$work/headers"
  curl -sS --proto '=https' --tlsv1.2 --connect-timeout 5 --max-time 30 -X POST \
    -H @"$work/headers" -H 'Content-Type: application/json' --data-binary @"$2" \
    -o "$work/answer" -w '%{http_code}' "$PLATFORM_URL$1" || true
}

# kube_secret <method> [patch file] prints the Kubernetes API's status for one call on the push token's Secret under
# the job's own account, and writes its answer to $work/secret.
kube_secret() {
  local body=()
  [ -z "${2:-}" ] || body=(-H 'Content-Type: application/merge-patch+json' --data-binary @"$2")
  printf 'Authorization: Bearer %s\n' "$(cat "$kube_account/token")" > "$work/kube-headers"
  curl -sS --proto '=https' --cacert "$kube_account/ca.crt" --max-time 10 -X "$1" -H @"$work/kube-headers" "${body[@]}" \
    -o "$work/secret" -w '%{http_code}' "$kube_api/api/v1/namespaces/$push_namespace/secrets/$push_secret" || true
}

write_report_token() {
  jq -j .reportToken "$work/answer" > "$work/report-token"
  kubectl create secret generic "$report_secret" -n "$node_namespace" --from-file=token="$work/report-token" \
    --dry-run=client -o yaml | kubectl apply -f - > /dev/null
}

# write_repository_token gives Argo CD the cluster repository with the token as its password.
write_repository_token() {
  jq -j .repositoryToken "$work/answer" > "$work/repository-token"
  kubectl create secret generic "$repository_secret" -n "$argocd_namespace" \
    --from-literal=type=git --from-literal=url="$REPO_URL" --from-literal=username=x-access-token \
    --from-file=password="$work/repository-token" --dry-run=client -o yaml |
    kubectl label --local -f - argocd.argoproj.io/secret-type=repository -o yaml | kubectl apply -f - > /dev/null
}

# write_notification_token gives Argo CD's webhook the platform it posts to as $platform-url and its bearer as $platform-token.
write_notification_token() {
  jq -j .notificationToken "$work/answer" > "$work/notification-token"
  kubectl create secret generic "$notifications_secret" -n "$argocd_namespace" --from-literal=platform-url="$PLATFORM_URL" \
    --from-file=platform-token="$work/notification-token" --dry-run=client -o yaml | kubectl apply -f - > /dev/null
}

platform_token() {
  [ "$REPOSITORY_CREDENTIAL" = platform-token ]
}

# require_tokens fails before any Secret is written unless the answer holds every token this cluster keeps.
require_tokens() {
  local field fields=(reportToken notificationToken)
  if platform_token; then
    fields+=(repositoryToken)
  fi
  for field in "${fields[@]}"; do
    jq -e --arg field "$field" '.[$field] | type == "string" and length > 0' "$work/answer" > /dev/null ||
      fail "the platform answered the enrolment without a $field"
  done
}

# require_secrets fails unless every Secret an enrolment writes exists, since a spent token can never write them again.
require_secrets() {
  if ! kubectl get secret "$report_secret" -n "$node_namespace" > /dev/null 2>&1; then
    fail "the enrolment token is spent but the Secret $report_secret in $node_namespace is missing, so the cluster cannot report until it is built again"
  fi
  if platform_token && ! kubectl get secret "$repository_secret" -n "$argocd_namespace" > /dev/null 2>&1; then
    fail "the enrolment token is spent but the Secret $repository_secret in $argocd_namespace is missing, so Argo CD cannot read the cluster repository until it is built again"
  fi
  if ! kubectl get secret "$notifications_secret" -n "$argocd_namespace" > /dev/null 2>&1; then
    fail "the enrolment token is spent but the Secret $notifications_secret in $argocd_namespace is missing, so Argo CD cannot tell the platform its verdicts until the cluster is built again"
  fi
}

enrol() {
  local status
  jq -n --arg id "$SUBSCRIPTION_ID" '{subscriptionId: $id}' > "$work/body"
  status=$(post /v1/cluster-enrolments "$work/body")
  case $status in
    200)
      require_tokens
      write_report_token
      if platform_token; then
        write_repository_token
      fi
      write_notification_token
      echo "cluster-tokens: enrolled subscription $SUBSCRIPTION_ID"
      ;;
    409)
      require_secrets
      echo "cluster-tokens: subscription $SUBSCRIPTION_ID enrolled already"
      ;;
    *) fail "the platform answered $status to the enrolment: $(jq -r '.error // empty' "$work/answer" 2> /dev/null)" ;;
  esac
}

# patch_app_repository sends the password alone as a merge patch, since a slice's Role grants patch and no read.
patch_app_repository() {
  local name=$1 token=$2 status
  jq -n --arg password "$token" '{stringData: {password: $password}}' > "$work/patch"
  printf 'Authorization: Bearer %s\n' "$(cat "$kube_account/token")" > "$work/kube-headers"
  status=$(curl -sS --proto '=https' --cacert "$kube_account/ca.crt" --max-time 10 -X PATCH \
    -H @"$work/kube-headers" -H 'Content-Type: application/merge-patch+json' --data-binary @"$work/patch" \
    -o /dev/null -w '%{http_code}' "$kube_api/api/v1/namespaces/$argocd_namespace/secrets/$name" || echo 000)
  if [ "$status" != 200 ]; then
    echo "cluster-tokens: could not patch $name in $argocd_namespace: HTTP $status; the next run tries again" >&2
  fi
}

# patch_app_repositories names each slice's Secret app-repository-<app> after its project app-<app>, as the slices commit it.
patch_app_repositories() {
  local project token
  while IFS=$'\t' read -r project token; do
    [[ $project =~ ^app-([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)$ ]] || fail "the renewal names the unusable project $project"
    patch_app_repository "app-repository-${BASH_REMATCH[1]}" "$token"
  done < <(jq -r '.appRepositories // [] | .[] | [.project, .repositoryToken] | @tsv' "$work/answer")
}

renew() {
  local status
  platform_token || fail "a cluster that reads its repository with a deploy key renews no repository token"
  echo '{}' > "$work/body"
  status=$(post /v1/cluster-repository-tokens "$work/body")
  [ "$status" = 200 ] || fail "the platform answered $status to the repository token renewal"
  write_repository_token
  patch_app_repositories
  echo "cluster-tokens: renewed the repository tokens"
}

# token_lasts <now> succeeds when the Secret holds a token whose recorded expiry is more than 11 hours after now.
token_lasts() {
  jq -e --argjson now "$1" --argjson within "$renew_within" --arg key "$lease_expiry" \
    '(.data.token // "") != "" and ((.metadata.annotations[$key] // "") | try fromdateiso8601 catch 0) > $now + $within' \
    "$work/secret" > /dev/null
}

# require_lease <now> fails before the Secret is written unless the answer holds a token and an expiry within 24 hours of now.
require_lease() {
  jq -e '.credential.token | type == "string" and length > 0' "$work/answer" > /dev/null ||
    fail "the platform answered the lease of $LEASE_RESOURCE without a credential.token"
  jq -e --argjson now "$1" --argjson longest "$longest_lease" \
    '.expiresAt | type == "string" and ((try fromdateiso8601 catch null) as $at | $at != null and $at > $now and $at < $now + $longest)' \
    "$work/answer" > /dev/null ||
    fail "the platform answered the lease of $LEASE_RESOURCE with an expiresAt not within the next 24 hours: $(jq -c .expiresAt "$work/answer")"
}

lease() {
  local status now
  status=$(kube_secret GET)
  [ "$status" = 200 ] || fail "could not read the Secret $push_secret in $push_namespace: HTTP $status"
  now=$(date -u +%s)
  if token_lasts "$now"; then
    echo "cluster-tokens: the push token for $LEASE_RESOURCE has more than 11 hours left"
    return 0
  fi
  jq -n --arg resource "$LEASE_RESOURCE" '{resource: $resource, want: {signals: "metrics,logs"}}' > "$work/body"
  status=$(post_once /v1/cluster-leases "$work/body")
  [ "$status" = 200 ] ||
    fail "the platform answered $status to the lease of $LEASE_RESOURCE: $(jq -r '.error // empty' "$work/answer" 2> /dev/null)"
  require_lease "$now"
  jq --arg key "$lease_expiry" '{metadata: {annotations: {($key): .expiresAt}}, stringData: {token: .credential.token}}' \
    "$work/answer" > "$work/patch"
  status=$(kube_secret PATCH "$work/patch")
  [ "$status" = 200 ] || fail "could not write the push token into the Secret $push_secret in $push_namespace: HTTP $status"
  echo "cluster-tokens: leased a push token for $LEASE_RESOURCE until $(jq -r .expiresAt "$work/answer")"
}

main() {
  : "${PLATFORM_URL:?}"
  case ${1:-} in
    enrol | renew)
      : "${SUBSCRIPTION_ID:?}" "${REPO_URL:?}" "${REPOSITORY_CREDENTIAL:?}"
      case $REPOSITORY_CREDENTIAL in
        platform-token | deploy-key) ;;
        *) fail "REPOSITORY_CREDENTIAL is $REPOSITORY_CREDENTIAL, want platform-token or deploy-key" ;;
      esac
      "$1"
      ;;
    lease)
      : "${LEASE_RESOURCE:?}"
      lease
      ;;
    *) fail "usage: cluster-tokens.sh enrol|renew|lease" ;;
  esac
}

main "$@"
