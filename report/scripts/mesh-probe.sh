#!/usr/bin/env bash
# Calls the mesh probe's target from outside the mesh: a plain call to its port, which the mesh must refuse, and a TLS
# handshake to its port 15008, where ztunnel presents the target's workload certificate. It writes both outcomes, with
# the certificate's identities, serial and validity, into the ConfigMap the node report reads.
# Usage: MESH_PROBE_HOST=<target's name> MESH_PROBE_PORT=<port> MESH_PROBE_CONFIGMAP=<name> POD_NAMESPACE=<namespace> mesh-probe.sh
# It exists because a pod inside the mesh cannot make a call without a workload identity, and the report runs inside it.
set -euo pipefail

: "${MESH_PROBE_HOST:?names the target}" "${MESH_PROBE_PORT:?names the port of the target}"
: "${MESH_PROBE_CONFIGMAP:?names the ConfigMap the report reads}" "${POD_NAMESPACE:?names the namespace of this pod}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# curl writes no output file for a call that brought nothing back, so the answer starts empty.
: > "$work/plain.out"
plain=0
timeout -k 5 10 curl -sS --max-time 5 -o "$work/plain.out" "http://$MESH_PROBE_HOST:$MESH_PROBE_PORT/" \
  2> "$work/plain.err" || plain=$?
handshake=0
timeout -k 5 10 openssl s_client -connect "$MESH_PROBE_HOST:15008" -alpn h2 < /dev/null \
  > "$work/handshake.out" 2> "$work/handshake.err" || handshake=$?
# s_client prints the certificate the server sent even when the server then refuses this side for presenting none.
openssl x509 -noout -serial -startdate -enddate -dateopt iso_8601 -ext subjectAltName \
  < "$work/handshake.out" > "$work/certificate.txt" 2>> "$work/handshake.err" || : > "$work/certificate.txt"

jq -n \
  --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson plain "$plain" \
  --rawfile answer "$work/plain.out" \
  --rawfile plain_err "$work/plain.err" \
  --argjson handshake "$handshake" \
  --rawfile handshake_err "$work/handshake.err" \
  --rawfile certificate "$work/certificate.txt" '
  def field($name): [$certificate | split("\n")[] | select(startswith($name + "=")) | ltrimstr($name + "=")] | first;
  def instant: if . then sub(" "; "T") else . end;
  {
    probedAt: $at,
    plainCall: {exitStatus: $plain, answer: $answer[0:256], stderr: $plain_err[-512:]},
    handshake: {exitStatus: $handshake, stderr: $handshake_err[-512:], certificate: (
      if field("serial") then {
        serial: field("serial"),
        notBefore: (field("notBefore") | instant),
        notAfter: (field("notAfter") | instant),
        identities: [$certificate | scan("URI:([^,\\s]+)") | .[0]]
      } else null end)}
  } | {data: {result: tojson}}' > "$work/patch.json"

timeout -k 5 10 kubectl patch configmap "$MESH_PROBE_CONFIGMAP" -n "$POD_NAMESPACE" --type merge --patch-file "$work/patch.json"
