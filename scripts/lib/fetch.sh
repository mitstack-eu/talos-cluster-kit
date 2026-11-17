#!/usr/bin/env bash
# Defines fetch, which downloads a file and refuses it unless it has the
# pinned sha256, naming the URL when the download fails, and check_pin, the
# sha256 check that every pinned file the render reads passes.
# Usage: . scripts/lib/fetch.sh; fetch <url> <sha256> <dest>; check_pin <sha256> <file>
# It exists so a refused download says which of a render's many URLs it was,
# since curl's own error names none.

fetch() {
  local status=0
  curl -fsSL --proto '=https' -o "$3" "$1" || status=$?
  if [ "$status" -ne 0 ]; then
    printf 'fetch: could not download %s (curl exit %d)\n' "$1" "$status" >&2
    return "$status"
  fi
  check_pin "$2" "$3"
}

check_pin() {
  echo "$1  $2" | sha256sum -c --quiet -
}
