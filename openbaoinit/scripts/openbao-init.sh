#!/usr/bin/env bash
# Initialises a hosting cluster's OpenBao once, commits its root token and
# recovery key to the cluster repository as openbao-init.asc, each a PGP
# message to the recipient key, and only then sets OpenBao up with the root
# token: the kv mount, Kubernetes auth, and the policy app-secrets-reader that
# lets each namespace read only its own apps path. A store already initialised
# is left as it is.
# Usage: openbao-init.sh, with the variables main names set, as the Job
#   mitstack-openbao-init sets them; MITSTACK_RETRY_SECONDS paces each retry.
# It exists so OpenBao's keys leave the cluster only as PGP messages, through
# Git, with no route in and no running platform, as ADR 0089 requires. The
# root token lives in this process's memory alone, and reaches no file,
# argument or log.
set -euo pipefail

readonly INIT_FILE=openbao-init.asc
WAIT=${MITSTACK_RETRY_SECONDS:-10}
root=""

say() {
  echo "openbao-init: $*" >&2
}

fail() {
  say "$*"
  exit 1
}

# retry runs its command until it succeeds, since this pod may hold the one copy of the root token.
retry() {
  local what=$1
  shift
  until "$@"; do
    say "$what failed; retrying in ${WAIT}s"
    sleep "$WAIT"
  done
}

# status and body split an answer that curl printed with -w '\n%{http_code}' after the body.
status() {
  printf '%s' "${1##*$'\n'}"
}

body() {
  printf '%s' "${1%$'\n'*}"
}

token_header() {
  if [ -n "$root" ]; then
    printf 'X-Vault-Token: %s\n' "$root"
  fi
}

# bao prints the body of OpenBao's 2xx answer to one request, verified against the cluster's authority, and hands curl
# the root token, once there is one, as a header on stdin.
bao() {
  local method=$1 path=$2 data=${3-} answer
  local args=(-sS --proto '=https' --tlsv1.2 --cacert "$MITSTACK_OPENBAO_CA" --connect-timeout 5 --max-time 30
    -X "$method" -H @- -w '\n%{http_code}')
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$data")
  fi
  if ! answer=$(token_header | curl "${args[@]}" "$MITSTACK_OPENBAO_ADDR/v1/$path"); then
    say "OpenBao did not answer $method $path"
    return 1
  fi
  case $(status "$answer") in
    2??) body "$answer" ;;
    *) say "OpenBao answered $(status "$answer") to $method $path"; return 1 ;;
  esac
}

# github sends one request to GitHub's API with the bearer token on stdin, and prints the body, a newline and the status.
github() {
  local method=$1 url=$2 token=$3
  shift 3
  printf 'Authorization: Bearer %s\n' "$token" | curl -sS --proto '=https' --tlsv1.2 --connect-timeout 5 --max-time 30 \
    -X "$method" -H @- -H 'X-GitHub-Api-Version: 2022-11-28' -w '\n%{http_code}' "$@" "$url"
}

b64url() {
  openssl base64 -A | tr '+/' '-_' | tr -d '='
}

# app_jwt prints the App's JWT, signed RS256 with its key, issued a minute in the past for clock drift, alive ten minutes.
app_jwt() {
  local now header claims signature
  now=$(date +%s)
  if ! header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url) ||
    ! claims=$(jq -cn --arg iss "$MITSTACK_GITHUB_APP_ID" --argjson now "$now" '{iat: ($now - 60), exp: ($now + 540), iss: $iss}' | b64url) ||
    ! signature=$(printf '%s.%s' "$header" "$claims" | openssl dgst -sha256 -sign "$MITSTACK_APP_KEY_FILE" -binary | b64url); then
    say "signing the App's JWT with $MITSTACK_APP_KEY_FILE failed"
    return 1
  fi
  printf '%s.%s.%s' "$header" "$claims" "$signature"
}

# app_token prints a one-hour installation token that may only read, or only write, the cluster repository's contents.
app_token() {
  local access=$1 jwt request answer
  jwt=$(app_jwt) || return 1
  request=$(jq -cn --arg repo "${MITSTACK_REPOSITORY#*/}" --arg access "$access" '{repositories: [$repo], permissions: {contents: $access}}')
  answer=$(github POST "$MITSTACK_GITHUB_API_URL/app/installations/$MITSTACK_GITHUB_INSTALLATION_ID/access_tokens" "$jwt" \
    -H 'Accept: application/vnd.github+json' -H 'Content-Type: application/json' --data-binary "$request") || return 1
  if [ "$(status "$answer")" != 201 ]; then
    say "GitHub answered $(status "$answer") to the App's request for a token that may $access the repository"
    return 1
  fi
  body "$answer" | jq -er .token
}

# init_file prints the cluster repository's openbao-init.asc as it is stored, a newline and the status of the read.
init_file() {
  github GET "$MITSTACK_GITHUB_API_URL/repos/$MITSTACK_REPOSITORY/contents/$INIT_FILE" "$1" -H 'Accept: application/vnd.github.raw+json'
}

# init_file_state prints committed or absent, read with a token that may only read the repository.
init_file_state() {
  local token answer
  token=$(app_token read) && answer=$(init_file "$token") || return 1
  case $(status "$answer") in
    200) echo committed ;;
    404) echo absent ;;
    *) say "GitHub answered $(status "$answer") to reading $INIT_FILE"; return 1 ;;
  esac
}

init_state() {
  bao GET sys/init | jq -er '.initialized | tostring'
}

encrypt() {
  gpg --batch --quiet --armor --recipient-file "$MITSTACK_RECIPIENT_FILE" --encrypt
}

# armor wraps the binary PGP message on stdin in ASCII armor without the CRC24 footer RFC 9580 section 6.1 says not to add.
armor() {
  printf -- '-----BEGIN PGP MESSAGE-----\n\n'
  openssl base64
  printf -- '-----END PGP MESSAGE-----\n'
}

# initialise initialises OpenBao with one recovery share encrypted to the recipient key, and sets root and messages, the
# contents of openbao-init.asc; it stops on finding OpenBao initialised after a failed init, whose root token is lost.
initialise() {
  local recipient request answer token_message recovery_message
  recipient=$(gpg --batch --quiet --dearmor < "$MITSTACK_RECIPIENT_FILE" | openssl base64 -A) ||
    fail "gpg cannot read the recipient key, so OpenBao was left uninitialised"
  request=$(jq -cn --arg key "$recipient" '{recovery_shares: 1, recovery_threshold: 1, recovery_pgp_keys: [$key]}')
  until answer=$(bao PUT sys/init "$request"); do
    if [ "$(retry "reading OpenBao's init state" init_state)" = true ]; then
      fail "OpenBao is initialised, and the answer to its init was lost, so its root token and recovery key are lost"
    fi
    say "retrying the init in ${WAIT}s"
    sleep "$WAIT"
  done
  root=$(printf '%s' "$answer" | jq -er .root_token)
  token_message=$(retry "encrypting the root token" seal_root)
  recovery_message=$(printf '%s' "$answer" | jq -er '.recovery_keys_base64[0]' | openssl base64 -d -A | armor)
  messages="OpenBao root token:"$'\n'"$token_message"$'\n'"OpenBao recovery key:"$'\n'"$recovery_message"$'\n'
}

seal_root() {
  printf '%s' "$root" | encrypt
}

# commit creates openbao-init.asc holding messages, and succeeds too when a lost answer hid that an earlier try did.
commit() {
  local messages=$1 token answer request
  token=$(app_token write) && answer=$(init_file "$token") || return 1
  case $(status "$answer") in
    200)
      [ "$(body "$answer")" = "$(printf '%s' "$messages")" ] && return 0
      say "the cluster repository holds another $INIT_FILE, which this Job does not replace"
      return 1
      ;;
    404) ;;
    *) say "GitHub answered $(status "$answer") to reading $INIT_FILE"; return 1 ;;
  esac
  request=$(printf '%s' "$messages" | openssl base64 -A |
    jq -cR '{message: "Record OpenBao'"'"'s root token and recovery key, each a PGP message to the cluster'"'"'s recipient key", content: .}')
  answer=$(github PUT "$MITSTACK_GITHUB_API_URL/repos/$MITSTACK_REPOSITORY/contents/$INIT_FILE" "$token" -o /dev/null \
    -H 'Accept: application/vnd.github+json' -H 'Content-Type: application/json' --data-binary "$request") || return 1
  if [ "$(status "$answer")" != 201 ]; then
    say "GitHub answered $(status "$answer") to committing $INIT_FILE"
    return 1
  fi
}

# enable enables a secrets engine or auth method unless sys/<kind> lists it, since a lost answer may hide an earlier try.
enable() {
  local kind=$1 path=$2 request=$3 listed
  listed=$(bao GET "sys/$kind") || return 1
  if printf '%s' "$listed" | jq -e --arg path "$path/" '(.data // .) | has($path)' > /dev/null; then
    return 0
  fi
  bao POST "sys/$kind/$path" "$request" > /dev/null
}

kubernetes_accessor() {
  bao GET sys/auth | jq -er '(.data // .)["kubernetes/"].accessor // empty | select(test("^[A-Za-z0-9_-]+$"))'
}

# reader_policy prints app-secrets-reader, which lets a token read only the apps path named for its service account's namespace.
reader_policy() {
  local namespace="{{identity.entity.aliases.$1.metadata.service_account_namespace}}" policy
  printf -v policy 'path "kv/data/apps/%s/*" {\n  capabilities = ["read"]\n}\npath "kv/metadata/apps/%s/*" {\n  capabilities = ["read", "list"]\n}\n' \
    "$namespace" "$namespace"
  jq -cn --arg policy "$policy" '{policy: $policy}'
}

set_up() {
  local host=$KUBERNETES_SERVICE_HOST accessor
  case $host in *:*) host="[$host]" ;; esac
  retry "enabling kv" enable mounts kv '{"type":"kv","options":{"version":"2"}}'
  retry "enabling Kubernetes auth" enable auth kubernetes '{"type":"kubernetes"}'
  retry "configuring Kubernetes auth" bao POST auth/kubernetes/config \
    "$(jq -cn --arg host "https://$host:${KUBERNETES_SERVICE_PORT:-443}" '{kubernetes_host: $host}')" > /dev/null
  accessor=$(retry "reading the Kubernetes auth accessor" kubernetes_accessor)
  retry "writing the policy app-secrets-reader" bao POST sys/policies/acl/app-secrets-reader "$(reader_policy "$accessor")" > /dev/null
}

main() {
  local name initialised file
  for name in MITSTACK_OPENBAO_ADDR MITSTACK_OPENBAO_CA MITSTACK_GITHUB_API_URL MITSTACK_REPOSITORY MITSTACK_GITHUB_APP_ID \
    MITSTACK_GITHUB_INSTALLATION_ID MITSTACK_RECIPIENT_FILE MITSTACK_APP_KEY_FILE GNUPGHOME KUBERNETES_SERVICE_HOST; do
    [ -n "${!name:-}" ] || fail "$name is not set"
  done
  mkdir -p "$GNUPGHOME"
  chmod 0700 "$GNUPGHOME"
  printf probe | encrypt > /dev/null || fail "gpg cannot encrypt to the recipient key, so OpenBao was left uninitialised"

  initialised=$(retry "reading OpenBao's init state" init_state)
  file=$(retry "reading $INIT_FILE" init_file_state)
  if [ "$initialised" = true ]; then
    [ "$file" = committed ] ||
      fail "OpenBao is initialised, and the cluster repository holds no $INIT_FILE, so its root token and recovery key are lost"
    say "OpenBao is initialised, and the cluster repository holds $INIT_FILE, and this Job no longer holds the root token, so it changes nothing"
    return 0
  fi
  [ "$file" = absent ] ||
    fail "OpenBao is uninitialised, and the cluster repository already holds $INIT_FILE, so OpenBao was left uninitialised rather than lose the store that file opens"

  initialise
  retry "committing $INIT_FILE" commit "$messages"
  say "committed $INIT_FILE to $MITSTACK_REPOSITORY"
  set_up
  say "enabled kv and Kubernetes auth, and wrote the policy app-secrets-reader"
}

main "$@"
