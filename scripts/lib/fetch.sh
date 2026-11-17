#!/usr/bin/env bash
# Defines fetch, which downloads a file and refuses it unless it has the
# pinned sha256, naming the URL when the download fails.
# Usage: . scripts/lib/fetch.sh; fetch <url> <sha256> <dest>
# It exists so a refused download says which of a render's many URLs it was,
# since curl's own error names none.

fetch() {
  local status=0
  curl -fsSL --proto '=https' -o "$3" "$1" || status=$?
  if [ "$status" -ne 0 ]; then
    printf 'fetch: could not download %s (curl exit %d)\n' "$1" "$status" >&2
    return "$status"
  fi
  echo "$2  $3" | sha256sum -c --quiet -
}
