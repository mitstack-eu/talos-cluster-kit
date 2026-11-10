#!/usr/bin/env bash
# Defines drop_empty_metadata_maps, which removes an empty labels or
# annotations map from the top-level metadata of every document of a rendered
# file, whether written as "labels: {}" or as "labels:" above an indented "{}".
# Usage: . scripts/lib/empty-maps.sh; drop_empty_metadata_maps <rendered.yaml>
# It exists because the API server stores no empty map, so a document that
# declares one never matches its live object and Argo CD reads it OutOfSync.

drop_empty_metadata_maps() {
  awk '
    function flush() { if (held != "") print held; held = "" }
    /^---([ \t]|$)/ { flush(); in_metadata = 0; print; next }
    held != "" {
      if ($0 ~ /^[ \t]+\{\}[ \t]*$/) { held = ""; next }
      flush()
    }
    /^[^ \t#]/ { in_metadata = ($0 ~ /^metadata:[ \t]*$/) }
    in_metadata && /^  (labels|annotations):[ \t]*\{\}[ \t]*$/ { next }
    in_metadata && /^  (labels|annotations):[ \t]*$/ { held = $0; next }
    { print }
    END { flush() }
  ' "$1" > "$1.dropped"
  mv "$1.dropped" "$1"
}
