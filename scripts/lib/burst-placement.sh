#!/usr/bin/env bash
# Defines keep_off_burst_nodes, which gives the pod template of every
# Deployment, StatefulSet, ReplicaSet, Job and CronJob in a rendered file a
# required node affinity refusing any node with the mitstack.dev/burst label.
# It adds the node affinity beside an affinity the pod already has, and fails,
# naming the workload, on a pod whose own node affinity it would have to merge.
# Usage: . scripts/lib/burst-placement.sh; keep_off_burst_nodes <rendered.yaml>
# It exists because a Cloud pool removes only its idle burst members, so the
# kit's components must run on the steady members alone for a removal to move
# none, and the charts they come from offer no common value to say so.

keep_off_burst_nodes() {
  awk '
    function indent(line) { match(line, /^ */); return RLENGTH }
    function blank(line) { return line ~ /^[ \t]*(#.*)?$/ }
    function placement(pad, lines) {
      lines = pad "  nodeAffinity:\n" pad "    requiredDuringSchedulingIgnoredDuringExecution:\n" \
        pad "      nodeSelectorTerms:\n" pad "        - matchExpressions:\n" \
        pad "            - key: mitstack.dev/burst\n" pad "              operator: DoesNotExist"
      return lines
    }
    # block_end is the first line after start that sits at or above depth.
    function block_end(start, depth,   k) {
      for (k = start + 1; k <= n; k++) if (!blank(doc[k]) && indent(doc[k]) <= depth) return k
      return n + 1
    }
    function flush(   i, kind, name, in_metadata, t, spec, end, pod_end, k, pad, affinity) {
      kind = ""; name = ""
      for (i = 1; i <= n; i++) {
        if (doc[i] ~ /^kind:/) { kind = doc[i]; sub(/^kind:[ \t]*/, "", kind); sub(/[ \t]+$/, "", kind) }
        if (doc[i] ~ /^[^ \t#]/) in_metadata = (doc[i] ~ /^metadata:[ \t]*$/)
        if (in_metadata && name == "" && doc[i] ~ /^  name:/) { name = doc[i]; sub(/^  name:[ \t]*/, "", name) }
      }
      spec = 0
      if (kind ~ /^(Deployment|StatefulSet|ReplicaSet|Job|CronJob)$/) {
        for (i = 1; i <= n && !spec; i++) {
          if (doc[i] !~ /^ *template:[ \t]*$/) continue
          t = indent(doc[i]); end = block_end(i, t)
          for (k = i + 1; k < end; k++) if (indent(doc[k]) == t + 2 && doc[k] ~ /^ *spec:[ \t]*$/) { spec = k; break }
        }
      }
      if (spec) {
        pad = sprintf("%" (t + 4) "s", ""); pod_end = block_end(spec, t + 2); affinity = 0
        for (k = spec + 1; k < pod_end; k++) {
          if (indent(doc[k]) != t + 4 || doc[k] !~ /^ *affinity:/) continue
          if (doc[k] !~ /^ *affinity:[ \t]*$/) fail(kind, name, "writes its affinity inline")
          affinity = k
          for (i = k + 1; i < block_end(k, t + 4); i++)
            if (indent(doc[i]) == t + 6 && doc[i] ~ /^ *nodeAffinity:/) fail(kind, name, "sets a node affinity of its own")
        }
      }
      for (i = 1; i <= n; i++) {
        print doc[i]
        if (spec && !affinity && i == spec) print pad "affinity:\n" placement(pad)
        if (spec && affinity && i == affinity) print placement(pad)
      }
      n = 0
    }
    function fail(kind, name, why) {
      printf "keep_off_burst_nodes: %s %s %s, which this pass cannot merge with the burst placement\n", kind, name, why > "/dev/stderr"
      failed = 1; exit 1
    }
    /^---([ \t]|$)/ { flush(); print; next }
    { doc[++n] = $0 }
    END { if (!failed) flush() }
  ' "$1" > "$1.placed" || { rm -f "$1.placed"; return 1; }
  mv "$1.placed" "$1"
}
