#!/usr/bin/env bash
# Runs kube-bench on the cis-1.10 benchmark, and then kubescape on the NSA framework, and patches a summary of each, with
# the run's outcome, into the ConfigMap the node report reads. A tool that fails is kept as its summary's error, and the
# scan then exits non-zero, so the Job runs it again.
# Usage: SCAN_RESULTS=<dir> KUBE_BENCH_CONFIG=<cfg dir> KUBE_BENCH_VERSION=<v> security-scan.sh bench
#   SCAN_RESULTS=<dir> SCAN_CONFIGMAP=<namespace/name> KUBESCAPE_ARTIFACTS=<dir holding nsa and default_config_inputs>
#   KUBESCAPE_CONTROLS_VERSION=<v> security-scan.sh
# The bench step reads the host as root with no token and leaves its summary in SCAN_RESULTS; the second step runs as
# another user with the token, so no tool holds both. It exists because a Talos node runs no host service, so the scan
# runs as a Job and leaves its summaries where the report, which holds no right to start it, can read them.
set -euo pipefail

# kube-bench's version mapping picks no benchmark for Talos, so the scan names the one Sidero documents for it.
readonly benchmark=cis-1.10 targets=master,etcd,controlplane,node,policies
# The report carries each summary up to 16 KiB, so the failed list keeps 12 KB and each control's workloads 4 KB.
readonly failed_limit=12000 resources_limit=4000 error_limit=1000

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

# run_tool NAME COMMAND... runs one scanner, keeping its stdout in $work/NAME.out, and on failure writes the error the
# summary carries into $work/NAME.error.
run_tool() {
  local name=$1 status=0
  shift
  "$@" > "$work/$name.out" 2> "$work/$name.err" || status=$?
  if [ "$status" -ne 0 ]; then
    { printf '%s exited %d: ' "$name" "$status"; tail -c "$error_limit" "$work/$name.err"; } > "$work/$name.error"
    return 1
  fi
}

# unreadable NAME WHAT keeps why a scanner's output gave no summary, unless its run already failed.
unreadable() {
  local name=$1 what=$2
  [ -s "$work/$name.error" ] || { printf '%s %s: ' "$name" "$what"; tail -c "$error_limit" "$work/$name.err"; } > "$work/$name.error"
  echo '{}' > "$work/$name.measured"
}

# summary NAME prints the error a scanner's summary carries, or nothing.
summary_error() {
  cat "$work/$1.error" 2> /dev/null || true
}

# Every summary bounds its lists with this jq, keeping each entry whole while the list's JSON fits n bytes.
# shellcheck disable=SC2016 # jq expands its own variables
readonly bounded='def bounded($n): reduce .[] as $x ([]; if ((. + [$x]) | tojson | utf8bytelength) <= $n then . + [$x] else . end);'

kube_bench() {
  local started status=0
  started=$(now)
  run_tool kube-bench kube-bench run --config-dir "$KUBE_BENCH_CONFIG" --benchmark "$benchmark" --targets "$targets" --json || status=$?
  if [ "$status" -eq 0 ] && jq -c --argjson limit "$failed_limit" "$bounded"' {
      totals: (.Totals | {pass: .total_pass, fail: .total_fail, warn: .total_warn, info: .total_info}),
      failed: ([.Controls[].tests[].results[] | select(.status == "FAIL")
        | {id: .test_number, scored: (.scored == true), text: (.test_desc | .[0:160])}] | bounded($limit))
    }' "$work/kube-bench.out" > "$work/kube-bench.measured" 2> "$work/kube-bench.err"; then
    :
  else
    unreadable kube-bench "printed no results jq could read"
    status=1
  fi
  jq -c -n --arg version "$KUBE_BENCH_VERSION" --arg started "$started" --arg finished "$(now)" --arg profile "$benchmark" \
    --slurpfile measured "$work/kube-bench.measured" --arg error "$(summary_error kube-bench; echo x)" '
    ($error | rtrimstr("x")) as $error |
    {tool: "kube-bench", version: $version, startedAt: $started, profile: $profile} + $measured[0]
      + (if $error == "" then {} else {error: $error} end) + {finishedAt: $finished}' > "$work/kube-bench.json"
  return "$status"
}

# kubescape scans with an empty exception list, since only the platform may exempt a finding, and with no host sensor,
# which it would otherwise deploy into the cluster.
kubescape_scan() {
  local started status=0
  started=$(now)
  jq '.settings.postureControlInputs' "$KUBESCAPE_ARTIFACTS/default_config_inputs" > "$work/inputs.json"
  echo '[]' > "$work/exceptions.json"
  mkdir "$work/result"
  run_tool kubescape kubescape scan framework nsa --use-from "$KUBESCAPE_ARTIFACTS/nsa" --controls-config "$work/inputs.json" \
    --exceptions "$work/exceptions.json" --host-scan=false --format json --output "$work/result/kubescape.json" \
    --cache-dir "$work/kubescape-cache" || status=$?
  if [ "$status" -eq 0 ] && jq -c --argjson limit "$failed_limit" --argjson resources "$resources_limit" "$bounded"'
      def rank($severity): ["Critical", "High", "Medium", "Low"] | index($severity) // 4;
      def workload: (.metadata // .) as $m | {namespace: ($m.namespace // null), kind: (.kind // null), name: ($m.name // null)};
      (reduce (.resources // [])[] as $r ({}; .[$r.resourceID] = ($r.object // {}))) as $objects
      | (reduce (.results // [])[] as $r ({}; reduce ($r.controls[] | select(.status.status == "failed") | .controlID) as $c (.;
          .[$c] += [$objects[$r.resourceID] // {} | workload]))) as $failing
      | {version: .metadata.scanMetadata.kubescapeVersion,
         failedControlsBySeverity: .summaryDetails.controlsSeverityCounters,
         failed: ([.summaryDetails.controls | to_entries[] | select(.value.status == "failed")
             | {key, value, rank: rank(.value.severity)}]
           | sort_by(.rank, .key)
           | map({id: .key, name: .value.name, severity: .value.severity,
               failedResources: .value.ResourceCounters.failedResources,
               resources: (if .rank < 2 then ($failing[.key] // [] | sort_by(tojson) | bounded($resources)) else [] end)})
           | bounded($limit))}' "$work/result/kubescape.json" > "$work/kubescape.measured" 2> "$work/kubescape.err"; then
    :
  else
    unreadable kubescape "wrote no result jq could read"
    status=1
  fi
  jq -c -n --arg controls "$KUBESCAPE_CONTROLS_VERSION" --arg started "$started" --arg finished "$(now)" \
    --slurpfile measured "$work/kubescape.measured" --arg error "$(summary_error kubescape; echo x)" '
    ($error | rtrimstr("x")) as $error |
    {tool: "kubescape", version: ($measured[0].version // null), startedAt: $started, framework: "NSA", controlsVersion: $controls}
      + ($measured[0] | del(.version)) + (if $error == "" then {} else {error: $error} end) + {finishedAt: $finished}' > "$work/kubescape.json"
  return "$status"
}

# bench_step always exits 0, keeping a failure in its summary, so the step after it still runs kubescape and patches both.
bench_step() {
  : "${KUBE_BENCH_CONFIG:?names the config directory of kube-bench}" "${KUBE_BENCH_VERSION:?names the kube-bench the image carries}"
  local outcome=ran
  kube_bench || outcome=failed
  cp "$work/kube-bench.json" "$SCAN_RESULTS/kube-bench.json"
  echo "security-scan: kube-bench $outcome"
}

scan_step() {
  : "${SCAN_CONFIGMAP:?names the ConfigMap the report reads}" "${KUBESCAPE_ARTIFACTS:?names the directory of the NSA framework}"
  : "${KUBESCAPE_CONTROLS_VERSION:?names the release of the NSA framework}"
  local bench=ran scan=ran status=0
  if [ -s "$SCAN_RESULTS/kube-bench.json" ]; then
    cp "$SCAN_RESULTS/kube-bench.json" "$work/kube-bench.json"
  else
    jq -c -n --arg finished "$(now)" '{tool: "kube-bench", error: "the kube-bench step left no summary", finishedAt: $finished}' \
      > "$work/kube-bench.json"
  fi
  jq -e 'has("error") | not' "$work/kube-bench.json" > /dev/null || { bench=failed; status=1; }
  kubescape_scan || { scan=failed; status=1; }
  jq -n --rawfile bench "$work/kube-bench.json" --rawfile scan "$work/kubescape.json" --arg finished "$(now)" \
    --argjson status "$status" --arg output "kube-bench: $bench; kubescape: $scan" '
    {data: {"kube-bench": ($bench | rtrimstr("\n")), kubescape: ($scan | rtrimstr("\n")),
      "last-run": ({finishedAt: $finished, exitStatus: $status, output: $output} | tojson)}}' > "$work/patch.json"
  kubectl patch configmap "${SCAN_CONFIGMAP#*/}" -n "${SCAN_CONFIGMAP%%/*}" --type merge --patch-file "$work/patch.json" > /dev/null
  echo "security-scan: kube-bench $bench, kubescape $scan"
  return "$status"
}

main() {
  : "${SCAN_RESULTS:?names the directory the two steps share}"
  export HOME=$work
  case "${1:-}" in
    bench) bench_step ;;
    "") scan_step ;;
    *) echo "security-scan: unknown step $1" >&2; return 2 ;;
  esac
}

main "$@"
