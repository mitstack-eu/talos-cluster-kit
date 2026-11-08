#!/usr/bin/env bash
# Prepares a gateway once its packages are installed: installs the talosctl and kubectl that tools.env pins after
# checking their sha256, copies every image of the mirror list into the local registry by digest with the skopeo image
# tools.env pins, and fetches every served manifest, refusing one whose sha256 differs from the list. Only then does it
# write the completion file, which Caddy waits for. Each step runs within its tools.env bound. The gateway has no login,
# so a failure writes its stage and error to GATEWAY_CONSOLE and GATEWAY_ISSUE, then powers the gateway off.
# Usage: prepare.sh <tools.env> <images.txt> <manifests.txt> <manifest dir> <completion file>
#   images.txt holds one name:tag@sha256:digest per line, and manifests.txt
#   one "<file name> <sha256> <url>" per line. PREPARE_BIN_DIR names where the
#   binaries go, /usr/local/bin by default.
# It exists so the nodes pull and fetch only from the gateway, while the
# gateway alone reaches the public registries, and only for pinned bytes.
set -euo pipefail

readonly registry=127.0.0.1:5000
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

# fetch_checked <url> <sha256> <dest> <mode> writes a download only once its bytes match.
fetch_checked() {
  local tmp
  stage="the download of $1"
  tmp=$(mktemp)
  curl -fsSL --proto '=https' --connect-timeout "$DOWNLOAD_CONNECT_SECONDS" --max-time "$DOWNLOAD_SECONDS" \
    -o "$tmp" "$1" 2>> "$errors"
  if ! echo "$2  $tmp" | sha256sum -c --quiet - > /dev/null 2>&1; then
    rm -f "$tmp"
    fail "$1 does not have sha256 $2"
  fi
  install -m "$4" "$tmp" "$3"
  rm -f "$tmp"
}

install_tools() {
  local bin_dir=${PREPARE_BIN_DIR:-/usr/local/bin}
  fetch_checked "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" "$TALOSCTL_SHA256" "$bin_dir/talosctl" 0755
  fetch_checked "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/amd64/kubectl" "$KUBECTL_SHA256" "$bin_dir/kubectl" 0755
}

# mirror_image <name:tag@digest> copies one image, its digest preserved, under its name and tag, within COPY_SECONDS.
mirror_image() {
  local ref=$1 name_tag digest status=0
  stage="the copy of $ref"
  name_tag=${ref%@*}
  digest=${ref##*@}
  timeout -k 10 "$COPY_SECONDS" docker run --rm --network host "$SKOPEO_IMAGE" copy --all --preserve-digests --retry-times 3 --dest-tls-verify=false \
    "docker://${name_tag%:*}@${digest}" "docker://${registry}/${name_tag}" 2>> "$errors" || status=$?
  case $status in
    0) ;;
    124) fail "$ref did not copy into the gateway's image mirror within ${COPY_SECONDS}s" ;;
    *) fail "$ref did not copy into the gateway's image mirror" ;;
  esac
}

main() {
  [ "$#" -eq 5 ] || fail "usage: prepare.sh <tools.env> <images.txt> <manifests.txt> <manifest dir> <completion file>"
  completion=$5
  # shellcheck source=/dev/null
  . "$1"
  : "${DOWNLOAD_CONNECT_SECONDS:?}" "${DOWNLOAD_SECONDS:?}" "${COPY_SECONDS:?}"
  install_tools

  local ref file sha256 url
  while read -r ref; do
    if [ -n "$ref" ]; then
      mirror_image "$ref"
    fi
  done < "$2"
  mkdir -p "$4"
  while read -r file sha256 url; do
    if [ -n "$file" ]; then
      fetch_checked "$url" "$sha256" "$4/$file" 0644
    fi
  done < "$3"
  stage="the completion file"
  install -D -m 0644 /dev/null "$completion"
  echo "prepare: the mirror and the manifests are in place"
}

main "$@"
