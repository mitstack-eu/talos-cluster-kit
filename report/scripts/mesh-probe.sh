#!/usr/bin/env bash
# Calls the mesh probe's target from outside the mesh: a plain call to its port, which the mesh must refuse, and a TLS
# handshake to its port 15008, where ztunnel presents the target's workload certificate. It writes both outcomes, with
# the certificate's identities, serial and validity, and how each gateway mirror's connection ended, into the ConfigMap
# the node report reads.
# Usage: MESH_PROBE_HOST=<target's name> MESH_PROBE_PORT=<port> MESH_PROBE_CONFIGMAP=<name> POD_NAMESPACE=<namespace> [GATEWAYS="<address>[=<https url>] ..." GATEWAY_CA=<pem>] mesh-probe.sh
# It exists because a pod inside the mesh cannot call without a workload identity or see a closed port, and the report
# runs inside it.
set -euo pipefail
shopt -s nullglob

: "${MESH_PROBE_HOST:?names the target}" "${MESH_PROBE_PORT:?names the port of the target}"
: "${MESH_PROBE_CONFIGMAP:?names the ConfigMap the report reads}" "${POD_NAMESPACE:?names the namespace of this pod}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# connection_of <curl exit> <HTTP status> <stderr> names how a mirror's connection ended; curl's words split exit 35,
# where curl on OpenSSL 3.0 names a peer's close before it answered SSL_ERROR_SYSCALL and later releases unexpected eof.
connection_of() {
  case $1 in
    0) if [ "$2" = 200 ]; then echo served; else echo answered; fi ;;
    7) echo closed ;;
    28) echo timed-out ;;
    5[256]) echo reset ;;
    35) if grep -qiE 'reset by peer|broken pipe|unexpected eof|SSL_ERROR_SYSCALL' "$3"; then echo reset; else echo tls-failure; fi ;;
    5[13489] | 6[046] | 77 | 8[023] | 9[018]) echo tls-failure ;;
    *) echo failed ;;
  esac
}

probe_gateway() {
  local err=$work/gw-$1.err status=0 code url=https://${2#https://}
  code=$(timeout -k 5 10 curl -q -sS --proto '=https' --cacert "$work/ca.pem" --max-time 5 -o /dev/null \
    -w '%{http_code}' "$url/v2/" 2> "$err") || status=$?
  jq -n --arg address "$1" --arg connection "$(connection_of "$status" "$code" "$err")" --argjson status "$status" \
    --arg code "$code" --rawfile stderr "$err" \
    '{address: $address, connection: $connection, exitStatus: $status, httpCode: $code, stderr: $stderr[-512:]}' \
    > "$work/gw-$1.json"
}

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
# The time is taken before the mirrors are asked, so no reading is stamped later than the call it records.
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s\n' "${GATEWAY_CA:-}" > "$work/ca.pem"
for gateway in ${GATEWAYS:-}; do
  probe_gateway "${gateway%%=*}" "${gateway#*=}" &
done
wait
jq -s . /dev/null "$work"/gw-*.json > "$work/gateways.json"

jq -n \
  --arg at "$at" \
  --argjson plain "$plain" \
  --rawfile answer "$work/plain.out" \
  --rawfile plain_err "$work/plain.err" \
  --argjson handshake "$handshake" \
  --rawfile handshake_err "$work/handshake.err" \
  --rawfile certificate "$work/certificate.txt" \
  --slurpfile gateways "$work/gateways.json" '
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
      } else null end)},
    gateways: $gateways[0]
  } | {data: {result: tojson}}' > "$work/patch.json"

timeout -k 5 10 kubectl patch configmap "$MESH_PROBE_CONFIGMAP" -n "$POD_NAMESPACE" --type merge --patch-file "$work/patch.json"
