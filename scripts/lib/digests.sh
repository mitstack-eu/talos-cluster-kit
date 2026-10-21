#!/usr/bin/env bash
# Defines refuse_undigested, which fails when a rendered manifest names any
# image by something other than a sha256 digest.
# Usage: . scripts/lib/digests.sh; refuse_undigested <rendered.yaml> <caller>
# It exists so every component a node applies pulls bytes the mirror holds by
# digest, the same rule mitstack-eu/cluster-bases holds its renders to.

refuse_undigested() {
  local undigested
  # A bare image: key, such as a CRD schema's property, names no image.
  undigested=$(grep -E '^[[:space:]]*(- )?image:[[:space:]]*[^[:space:]]' "$1" | grep -Ev '@sha256:[0-9a-f]{64}"?[[:space:]]*$' || true)
  [ -z "$undigested" ] && return 0
  printf '%s: these images are not named by digest:\n%s\n' "$2" "$undigested" >&2
  exit 1
}
