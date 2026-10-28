#!/usr/bin/env bash
# Prepares a gateway once its packages are installed: installs the talosctl
# and kubectl that tools.env pins after checking their sha256, copies every
# image of the mirror list into the local registry by digest, and fetches
# every served manifest, refusing one whose sha256 differs from the list.
# Only then does it write the completion file, which Caddy waits for.
# Usage: prepare.sh <tools.env> <images.txt> <manifests.txt> <manifest dir> <completion file>
#   images.txt holds one name:tag@sha256:digest per line, and manifests.txt
#   one "<file name> <sha256> <url>" per line. PREPARE_BIN_DIR names where the
#   binaries go, /usr/local/bin by default.
# It exists so the nodes pull and fetch only from the gateway, while the
# gateway alone reaches the public registries, and only for pinned bytes.
set -euo pipefail

readonly registry=127.0.0.1:5000

fail() {
  echo "prepare: $*" >&2
  exit 1
}

# fetch_checked <url> <sha256> <dest> <mode> writes a download only once its bytes match.
fetch_checked() {
  local tmp
  tmp=$(mktemp)
  curl -fsSL --proto '=https' -o "$tmp" "$1"
  if ! echo "$2  $tmp" | sha256sum -c --quiet - > /dev/null 2>&1; then
    rm -f "$tmp"
    fail "$1 does not have sha256 $2"
  fi
  install -m "$4" "$tmp" "$3"
  rm -f "$tmp"
}

install_tools() {
  local bin_dir=${PREPARE_BIN_DIR:-/usr/local/bin}
  # shellcheck source=/dev/null
  . "$1"
  fetch_checked "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" "$TALOSCTL_SHA256" "$bin_dir/talosctl" 0755
  fetch_checked "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/amd64/kubectl" "$KUBECTL_SHA256" "$bin_dir/kubectl" 0755
}

# mirror_image <name:tag@digest> copies one image, its digest preserved, under its name and tag.
mirror_image() {
  local ref=$1 name_tag digest
  name_tag=${ref%@*}
  digest=${ref##*@}
  skopeo copy --all --preserve-digests --retry-times 3 --dest-tls-verify=false \
    "docker://${name_tag%:*}@${digest}" "docker://${registry}/${name_tag}"
}

main() {
  [ "$#" -eq 5 ] || fail "usage: prepare.sh <tools.env> <images.txt> <manifests.txt> <manifest dir> <completion file>"
  install_tools "$1"

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
  install -D -m 0644 /dev/null "$5"
  echo "prepare: the mirror and the manifests are in place"
}

main "$@"
