#!/usr/bin/env bash
# Initialises a hosting cluster's OpenBao once, commits its root token and recovery key to the cluster repository
# as openbao-init.asc, each a PGP message to the recipient key, then sets up kv, Kubernetes auth and app-secrets-reader.
# Usage: openbao-init.sh, with the variables main names set as the Job mitstack-openbao-init sets them.
# It exists so OpenBao's keys leave the cluster only as PGP messages through Git, as ADR 0089 requires, while the root
# token stays in this process's memory.
set -euo pipefail

readonly INIT_FILE=openbao-init.asc
WAIT=${MITSTACK_RETRY_SECONDS:-10}
root=""
recovery_lost=""
work=""

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

# bao hands curl the root token, once there is one, as a header on stdin, and prints the body of a 2xx answer.
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

# use_deploy_key has Git reach GitHub only with the write deploy key and only a host key it already knows. ssh refuses
# a key file others may read, so the key is copied to a file of this user's own.
use_deploy_key() {
  install -m 0600 "$MITSTACK_DEPLOY_KEY_FILE" "$work/deploy-key"
  export GIT_SSH_COMMAND="ssh -i $work/deploy-key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$MITSTACK_KNOWN_HOSTS_FILE -o BatchMode=yes -o ConnectTimeout=10"
  export GIT_TERMINAL_PROMPT=0
}

# fetch clones the repository afresh, so each try reads what its default branch holds now.
fetch() {
  rm -rf "$work/repository"
  if ! git clone --quiet "$MITSTACK_REPOSITORY_URL" "$work/repository" 2> "$work/git.log"; then
    say "cloning the cluster repository failed: $(tail -n 3 "$work/git.log")"
    return 1
  fi
}

init_file_state() {
  fetch || return 1
  if [ -e "$work/repository/$INIT_FILE" ]; then
    echo committed
  else
    echo absent
  fi
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

# initialise sets root and messages; it stops if a failed init left OpenBao initialised, since that root token is lost.
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
  root=$(printf '%s' "$answer" | jq -er .root_token) ||
    fail "the answer to the init holds no root token, so the root token and recovery key are lost"
  token_message=$(retry "encrypting the root token" seal_root)
  messages="OpenBao root token:"$'\n'"$token_message"$'\n'
  if recovery_message=$(printf '%s' "$answer" | recovery_message); then
    messages+="OpenBao recovery key:"$'\n'"$recovery_message"$'\n'
  else
    recovery_lost=true
    messages+="OpenBao recovery key: lost, since the answer to the init held none that could be read"$'\n'
    say "the answer to the init holds no readable recovery key; committing the root token alone"
  fi
}

# recovery_message fails unless the answer holds one base64 PGP message, whose first octet has its top bit set (RFC 9580).
recovery_message() {
  local key
  key=$(jq -er '.recovery_keys_base64 | if type == "array" and length == 1 then .[0] else empty end
    | strings | select(test("^[A-Za-z0-9+/]+={0,2}$"))') || return 1
  [ "$(printf '%s' "$key" | openssl base64 -d -A | od -An -N1 -tu1 | tr -d ' ')" -ge 128 ] 2> /dev/null || return 1
  printf '%s' "$key" | openssl base64 -d -A | armor
}

seal_root() {
  printf '%s' "$root" | encrypt
}

# commit pushes openbao-init.asc holding messages, and succeeds too when a lost answer hid that an earlier push landed.
commit() {
  local messages=$1 repository=$work/repository
  fetch || return 1
  if [ -e "$repository/$INIT_FILE" ]; then
    [ "$(cat "$repository/$INIT_FILE")" = "$(printf '%s' "$messages")" ] && return 0
    say "the cluster repository holds another $INIT_FILE, which this Job does not replace"
    return 1
  fi
  printf '%s' "$messages" > "$repository/$INIT_FILE"
  git -C "$repository" add "$INIT_FILE"
  git -C "$repository" -c user.name="mitstack OpenBao init" -c user.email=openbao-init@mitstack.dev commit --quiet \
    -m "Record OpenBao's root token and recovery key, each a PGP message to the cluster's recipient key"
  if ! git -C "$repository" push --quiet origin HEAD 2> "$work/git.log"; then
    say "pushing $INIT_FILE failed: $(tail -n 3 "$work/git.log")"
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
  for name in MITSTACK_OPENBAO_ADDR MITSTACK_OPENBAO_CA MITSTACK_REPOSITORY_URL MITSTACK_RECIPIENT_FILE \
    MITSTACK_KNOWN_HOSTS_FILE MITSTACK_DEPLOY_KEY_FILE GNUPGHOME KUBERNETES_SERVICE_HOST; do
    [ -n "${!name:-}" ] || fail "$name is not set"
  done
  mkdir -p "$GNUPGHOME"
  chmod 0700 "$GNUPGHOME"
  printf probe | encrypt > /dev/null || fail "gpg cannot encrypt to the recipient key, so OpenBao was left uninitialised"
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  use_deploy_key

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
  say "committed $INIT_FILE to the cluster repository"
  set_up
  say "enabled kv and Kubernetes auth, and wrote the policy app-secrets-reader"
  [ -z "$recovery_lost" ] ||
    fail "the answer to the init holds no readable recovery key, so $INIT_FILE holds the root token alone and the recovery key is lost"
}

main "$@"
