#!/usr/bin/env bash
# Calls the mesh probe's target from outside the mesh: a plain call to its port, which the mesh must refuse, and a TLS
# handshake to its port 15008, where ztunnel presents the target's workload certificate. It also asks each gateway's
# mirror for its API root under the gateway CA and names how the connection ended. It writes every outcome, with the
# certificate's identities, serial and validity, into the ConfigMap the node report reads.
# Usage: MESH_PROBE_HOST=<target's name> MESH_PROBE_PORT=<port> MESH_PROBE_CONFIGMAP=<name> POD_NAMESPACE=<namespace> [GATEWAYS="<address>=<https url> ..." GATEWAY_CA=<pem>] mesh-probe.sh
# It exists because a pod inside the mesh cannot make a call without a workload identity, and ztunnel answers a call
# from inside it to a closed port by accepting and then resetting it, and the report runs inside the mesh.
set -euo pipefail

: "${MESH_PROBE_HOST:?names the target}" "${MESH_PROBE_PORT:?names the port of the target}"
: "${MESH_PROBE_CONFIGMAP:?names the ConfigMap the report reads}" "${POD_NAMESPACE:?names the namespace of this pod}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# connection_of <curl exit status> <HTTP status> <stderr file> names how a mirror's connection ended. curl reads a peer
# that reset or closed the connection during the TLS handshake as 35, as it reads a failed handshake, so its words tell
# the two apart.
connection_of() {
  case $1 in
    0) if [ "$2" = 200 ]; then echo served; else echo answered; fi ;;
    7) echo closed ;;
    28) echo timed-out ;;
    52 | 55 | 56) echo reset ;;
    35) if grep -qiE 'reset by peer|broken pipe|unexpected eof' "$3"; then echo reset; else echo tls-failure; fi ;;
    51 | 53 | 54 | 58 | 59 | 60 | 64 | 66 | 77 | 80 | 82 | 83 | 90 | 91 | 98) echo tls-failure ;;
    *) echo failed ;;
  esac
}

# probe_gateway <address> <url> writes one mirror's reading.
probe_gateway() {
  local address=$1 url=$2 status=0 code
  code=$(timeout -k 5 10 curl -q -sS --proto '=https' --cacert "$work/gateway-ca.pem" --max-time 5 -o /dev/null \
    -w '%{http_code}' "$url/v2/" 2> "$work/gateway-$address.err") || status=$?
  jq -n --arg address "$address" --arg connection "$(connection_of "$status" "$code" "$work/gateway-$address.err")" \
    --argjson status "$status" --arg code "$code" --rawfile stderr "$work/gateway-$address.err" \
    '{address: $address, connection: $connection, exitStatus: $status, httpCode: $code, stderr: $stderr[-512:]}' \
    > "$work/gateway-$address.json"
}

probe_gateways() {
  local gateway
  printf '%s\n' "${GATEWAY_CA:-}" > "$work/gateway-ca.pem"
  for gateway in ${GATEWAYS:-}; do
    probe_gateway "${gateway%%=*}" "${gateway#*=}" &
  done
  wait
  for gateway in ${GATEWAYS:-}; do
    cat "$work/gateway-${gateway%%=*}.json"
  done | jq -s . > "$work/gateways.json"
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
probe_gateways

jq -n \
  --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
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
