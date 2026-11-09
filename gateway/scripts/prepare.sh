#!/usr/bin/env bash
# Prepares a gateway once its packages are installed: copies every image of the mirror list into the local registry by
# digest with the skopeo image tools.env pins, then copies talosctl, kubectl and every served manifest out of the tools
# image tools.env names, read from that registry by its digest, refusing a file whose sha256 differs from tools.env or
# the list. Only then does it write the completion file, which Caddy waits for. Each step runs within its tools.env
# bound. The gateway has no login, so a failure writes its stage and error to GATEWAY_CONSOLE and GATEWAY_ISSUE, then
# powers the gateway off.
# Usage: prepare.sh <tools.env> <images.txt> <manifests.txt> <manifest dir> <completion file>
#   images.txt holds one name:tag@sha256:digest per line, and manifests.txt
#   one "<file name> <sha256> <path in the tools image>" per line.
#   PREPARE_BIN_DIR names where the binaries go, /usr/local/bin by default.
# It exists so the nodes pull and fetch only from the gateway, while the
# gateway alone reaches the public registries, and only for pinned bytes.
set -euo pipefail

readonly registry=127.0.0.1:5000
# The tools container is created and never started, so nothing in the tools image runs on the gateway.
readonly tools_container=mitstack-gateway-tools
stage="its start"
completion=
errors=$(mktemp)
GATEWAY_CONSOLE=/dev/tty1
GATEWAY_ISSUE=/etc/issue.d/mitstack-gateway.issue

fail() {
  echo "prepare: $*" >> "$errors"
  exit 1
}

power_off_unless_complete() {
  local status=$?
  set +e
  cat "$errors" >&2
  if [ -z "$completion" ] || [ ! -e "$completion" ]; then
    {
      echo "mitstack gateway: the build failed at $stage (exit status $status), so the gateway powers itself off."
      tail -n 5 "$errors"
    } | fold -w 80 | head -n 20 > "$errors.screen"
    mkdir -p "$(dirname "$GATEWAY_ISSUE")" 2> /dev/null || true
    cat "$errors.screen" >> "$GATEWAY_ISSUE" 2> /dev/null || true
    cat "$errors.screen" > "$GATEWAY_CONSOLE" 2> /dev/null || true
    cat "$errors.screen" >&2
    systemctl poweroff
  fi
  rm -f "$errors" "$errors.screen"
}
trap power_off_unless_complete EXIT
trap 'exit 143' TERM

# check_bounded <status> <what did not happen> <bound> fails on a step's non-zero status, naming the bound it overran.
check_bounded() {
  case $1 in
    0) ;;
    124) fail "$2 within ${3}s" ;;
    *) fail "$2" ;;
  esac
}

# mirror_image <name:tag@digest> copies one image, its digest preserved, under its name and tag, within COPY_SECONDS.
mirror_image() {
  local ref=$1 name_tag digest status=0
  stage="the copy of $ref"
  name_tag=${ref%@*}
  digest=${ref##*@}
  timeout -k 10 "$COPY_SECONDS" docker run --rm --network host "$SKOPEO_IMAGE" copy --all --preserve-digests --retry-times 3 --dest-tls-verify=false \
    "docker://${name_tag%:*}@${digest}" "docker://${registry}/${name_tag}" 2>> "$errors" || status=$?
  check_bounded "$status" "$ref did not copy into the gateway's image mirror" "$COPY_SECONDS"
}

# open_tools_image creates the tools container from TOOLS_IMAGE as the local registry holds it, by its digest.
open_tools_image() {
  local name_tag=${TOOLS_IMAGE%@*} status=0
  stage="the read of $TOOLS_IMAGE from the gateway's image mirror"
  timeout -k 10 "$EXTRACT_SECONDS" docker create --network none --name "$tools_container" \
    "${registry}/${name_tag%:*}@${TOOLS_IMAGE##*@}" > /dev/null 2>> "$errors" || status=$?
  check_bounded "$status" "$TOOLS_IMAGE did not read from the gateway's image mirror" "$EXTRACT_SECONDS"
}

# extract_checked <path> <sha256> <dest> <mode> copies one file out of the tools container within EXTRACT_SECONDS, and
# writes it only once its bytes match.
extract_checked() {
  local tmp status=0
  stage="the copy of $1 out of the tools image"
  tmp=$(mktemp)
  timeout -k 10 "$EXTRACT_SECONDS" docker cp "$tools_container:$1" "$tmp" 2>> "$errors" || status=$?
  check_bounded "$status" "$1 did not copy out of the tools image" "$EXTRACT_SECONDS"
  if ! echo "$2  $tmp" | sha256sum -c --quiet - > /dev/null 2>&1; then
    rm -f "$tmp"
    fail "$1 in the tools image does not have sha256 $2"
  fi
  install -m "$4" "$tmp" "$3"
  rm -f "$tmp"
}

# install_from_tools_image <manifests.txt> <manifest dir> copies talosctl, kubectl and every served manifest out of the
# tools image, which the mirror holds by now.
install_from_tools_image() {
  local bin_dir=${PREPARE_BIN_DIR:-/usr/local/bin} file sha256 path status=0
  open_tools_image
  extract_checked /usr/local/bin/talosctl "$TALOSCTL_SHA256" "$bin_dir/talosctl" 0755
  extract_checked /usr/local/bin/kubectl "$KUBECTL_SHA256" "$bin_dir/kubectl" 0755
  mkdir -p "$2"
  while read -r file sha256 path; do
    if [ -n "$file" ]; then
      extract_checked "$path" "$sha256" "$2/$file" 0644
    fi
  done < "$1"
  stage="the removal of the tools container"
  timeout -k 10 "$EXTRACT_SECONDS" docker rm "$tools_container" > /dev/null 2>> "$errors" || status=$?
  check_bounded "$status" "the tools container did not go" "$EXTRACT_SECONDS"
}

main() {
  [ "$#" -eq 5 ] || fail "usage: prepare.sh <tools.env> <images.txt> <manifests.txt> <manifest dir> <completion file>"
  completion=$5
  # shellcheck source=/dev/null
  . "$1"
  if [ -z "${EXTRACT_SECONDS:-}" ] || [ -z "${COPY_SECONDS:-}" ]; then
    fail "tools.env names no bound for a copy"
  fi
  [ -n "${TOOLS_IMAGE:-}" ] || fail "tools.env names no tools image"

  local ref
  while read -r ref; do
    if [ -n "$ref" ]; then
      mirror_image "$ref"
    fi
  done < "$2"
  install_from_tools_image "$3" "$4"
  stage="the completion file"
  install -D -m 0644 /dev/null "$completion"
  echo "prepare: the mirror, the tools and the manifests are in place"
}

main "$@"
