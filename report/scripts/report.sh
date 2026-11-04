#!/usr/bin/env bash
# Posts the node report: nodes with their zone and region labels, etcd members with their health and the leader, each
# node's STATE and EPHEMERAL encryption, Argo CD's health with each Application's applied objects, what each Argo CD
# route answers a caller without a session, Argo CD's Services, Ingresses and pod images, CloudNativePG's objects with
# each cluster's primary, every pod's node and phase, each OpenBao member's seal state and the active one, each claim
# and the default storage classes, every Service's type, each default-deny NetworkPolicy, the objects labelled
# mitstack.dev/subscription, the API server's key set, the mesh probe's results and a checks member naming each check
# of the platform's verdict, signed with a token the API server mints for the body through block-kit's
# token-request.sh in REPORT_PROOF_SCRIPT.
# When a read, the body or the token request fails or times out, it posts an unsigned report whose error names it
# instead, then exits non-zero.
# Usage: REPORT_URL=<https url> SUBSCRIPTION_ID=<id> HOST_IP=<this pod's node> REPORT_PROOF_SCRIPT=<script> MESH_PROBE_URL=<target's url> MESH_PROBE_ANSWER=<its answer> MESH_PROBE_IDENTITY=<its SPIFFE id> MESH_PROBE_CONFIGMAP=<namespace/name> OPENBAO_MEMBERS="<name>=<https url> ..." OPENBAO_CA_FILE=<pem> ARGOCD_URL=<https url> [POD_NAMESPACE=mitstack-node] [REPORT_TOKEN_FILE=/var/run/mitstack/token] [REPORT_READ_SECONDS=21] report.sh
# It exists so the platform sees a cluster it never dials into, as ADR 0072 requires, with os:reader and no Secret right.
set -euo pipefail

# Nine limits of 21 seconds, the token request's 5, each with a 5-second kill grace, and the post's 20 make 264, which
# leaves 16 seconds of the Job's activeDeadlineSeconds of 280 for the tools to start. The mesh, pod, Argo CD,
# default-deny, OpenBao and etcd status reads run beside them, each within one limit.
limit=${REPORT_READ_SECONDS:-21}
# The outside probe writes every five minutes, so a result three runs old means it stopped.
mesh_probe_max_age=900
# Kinds as kubectl --show-kind names them, as blocks name their objects; never secret, as the report holds no Secret right.
subscription_kinds=(namespace serviceaccount service resourcequota limitrange deployment.apps
  ingress.networking.k8s.io networkpolicy.networking.k8s.io role.rbac.authorization.k8s.io
  rolebinding.rbac.authorization.k8s.io application.argoproj.io appproject.argoproj.io
  externalsecret.external-secrets.io)
# Argo CD answers userinfo and settings without a session by design, and every other route goes through its authenticator.
argocd_routes=(/api/v1/session/userinfo /api/v1/account /api/v1/applications /api/v1/applicationsets /api/v1/certificates
  /api/v1/clusters /api/v1/gpgkeys /api/v1/notifications/services /api/v1/notifications/templates
  /api/v1/notifications/triggers /api/v1/projects /api/v1/repocreds /api/v1/repositories /api/v1/settings
  /api/v1/settings/plugins /api/v1/write-repocreds /api/v1/write-repositories)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  echo "report: $*" >&2
  exit 1
}

limited() {
  timeout -k 5 "$@"
}

# read_into NAME OUT SECONDS COMMAND... runs one read limited to SECONDS and reports it as failed when it does not exit 0.
read_into() {
  local name=$1 out=$2 status=0
  shift 2
  limited "$@" > "$out" 2> "$work/read.err" || status=$?
  [ "$status" -eq 0 ] || report_failure "$name" "$status"
}

collect() {
  local mesh_reads cluster_reads argocd_reads etcd_reads
  read_mesh & mesh_reads=$!
  read_cluster & cluster_reads=$!
  read_argocd & argocd_reads=$!
  read_into "kubectl get nodes" "$work/nodes.json" "$limit" kubectl get nodes -o json --show-managed-fields
  read_into "kubectl get applications" "$work/applications.json" "$limit" kubectl get applications.argoproj.io -n argocd -o json
  read_into "kubectl get --raw /openid/v1/jwks" "$work/jwks.json" "$limit" kubectl get --raw /openid/v1/jwks
  read_into "talosctl etcd members" "$work/members.txt" "$limit" talosctl -n "$HOST_IP" etcd members
  read_etcd_status & etcd_reads=$!
  read_system_volumes
  read_beside cloudnative-pg kubectl get clusters.postgresql.cnpg.io,databases.postgresql.cnpg.io,databaseroles.postgresql.cnpg.io -A -o json
  read_beside storage-and-services kubectl get persistentvolumeclaims,storageclasses.storage.k8s.io,services -A -o json
  read_subscription_resources
  wait "$mesh_reads" "$cluster_reads" "$argocd_reads" "$etcd_reads"
  openbao_members > "$work/openbao.json"
}

# read_cluster reads every pod and each default-deny NetworkPolicy, and asks each OpenBao member for its seal state and
# whether it is the active one, which OpenBao answers without a token.
read_cluster() {
  local member
  read_beside pods kubectl get pods -A -o json &
  read_beside default-deny kubectl get networkpolicies.networking.k8s.io -A --field-selector metadata.name=mitstack-default-deny -o json &
  mkdir "$work/openbao"
  for member in $OPENBAO_MEMBERS; do
    read_beside "openbao/${member%%=*}.seal-status" curl -sS --fail --proto '=https' --cacert "$OPENBAO_CA_FILE" \
      --max-time 5 "${member#*=}/v1/sys/seal-status" &
    read_beside "openbao/${member%%=*}.leader" curl -sS --fail --proto '=https' --cacert "$OPENBAO_CA_FILE" \
      --max-time 5 "${member#*=}/v1/sys/leader" &
  done
  wait
}

# read_etcd_status asks each etcd member's node for that member's status through the Talos API. talosctl prints a row
# for each node that answers and exits non-zero naming each node that does not.
read_etcd_status() {
  local nodes
  nodes=$(awk 'NR > 1 && NF { split($4, urls, ","); sub("^https://", "", urls[1]); sub(":[0-9]+$", "", urls[1]); print urls[1] }' \
    "$work/members.txt" | paste -sd, -) || nodes=
  read_beside etcd-status talosctl -n "$nodes" etcd status
}

# read_argocd lists Argo CD's Ingresses and asks each of its routes once, all at the same time.
read_argocd() {
  local i
  read_beside argocd-ingresses kubectl get ingresses.networking.k8s.io -n argocd -o json &
  mkdir "$work/routes"
  for i in "${!argocd_routes[@]}"; do
    argocd_route "$i" &
  done
  wait
  for i in "${!argocd_routes[@]}"; do
    cat "$work/routes/$i.json"
  done > "$work/routes.json"
}

# argocd_route INDEX asks one route as a caller without a session, with a GET that sends no credential, cookie or body and
# follows no redirect, and keeps its status, its size and, for userinfo, the answer's loggedIn. -q reads no ~/.curlrc
# that could add a credential. -k accepts the certificate Argo CD signs for itself, since the request carries nothing
# to steal and no text of the answer leaves the pod.
argocd_route() {
  local path=${argocd_routes[$1]} out=$work/routes/$1 status ended=0
  status=$(curl -q -s -k --proto '=https' --connect-timeout 1 --max-time 5 --max-filesize 65536 -o "$out.body" \
    -w '%{http_code}' "$ARGOCD_URL$path") || ended=$?
  [ -f "$out.body" ] || : > "$out.body"
  jq -c -n --arg path "$path" --argjson status "$((10#${status:-0}))" --argjson ended "$ended" \
    --argjson bytes "$(wc -c < "$out.body")" --rawfile body "$out.body" '
    if $ended != 0 then {path: $path, status: 0, bytes: 0, error: "curl exit \($ended)"}
    else {path: $path, status: $status, bytes: $bytes}
      + if $path != "/api/v1/session/userinfo" then {}
        elif $status != 200 then {loggedIn: null}
        else {loggedIn: (try ($body | fromjson | .loggedIn | if type == "boolean" then . else null end) catch null)} end
    end' > "$out.json"
}

# openbao_members prints one entry per OpenBao member: its seal state and whether it is active, or the read that failed.
openbao_members() {
  local member name dir=$work/openbao
  for member in $OPENBAO_MEMBERS; do
    name=${member%%=*}
    jq -c -n --arg name "$name" \
      --rawfile seal "$dir/$name.seal-status.out" --rawfile seal_err "$dir/$name.seal-status.err" \
      --argjson seal_status "$(cat "$dir/$name.seal-status.status")" \
      --rawfile leader "$dir/$name.leader.out" --rawfile leader_err "$dir/$name.leader.err" \
      --argjson leader_status "$(cat "$dir/$name.leader.status")" '
      def failed($read; $status; $stderr): {read: $read, exitStatus: $status, stderr: $stderr[-512:]};
      def unread: {name: $name, initialized: null, sealed: null, active: null};
      ((try ($seal | fromjson) catch null) | if type == "object" then . else {} end) as $s
      | ((try ($leader | fromjson) catch null) | if type == "object" then . else {} end) as $l
      | if $seal_status != 0 then unread + {error: failed("sys/seal-status"; $seal_status; $seal_err)}
        elif ($s.sealed | type) != "boolean" then unread + {error: failed("sys/seal-status"; 0; "the answer names no seal state")}
        else {name: $name, initialized: $s.initialized, sealed: $s.sealed}
          + if $leader_status != 0 then {active: null, error: failed("sys/leader"; $leader_status; $leader_err)}
            elif ($l.is_self | type) != "boolean" then {active: null, error: failed("sys/leader"; 0; "the answer names no is_self")}
            else {active: $l.is_self} end
        end'
  done
}

# read_mesh calls the mesh probe's target from inside the mesh, where the report runs, and reads what the outside
# probe last wrote of its own calls.
read_mesh() {
  read_beside mesh-inside curl -sS --proto '=http' --max-time 5 "$MESH_PROBE_URL"
  read_beside mesh-outside kubectl get configmap "${MESH_PROBE_CONFIGMAP#*/}" -n "${MESH_PROBE_CONFIGMAP%%/*}" -o json
}

# read_beside NAME COMMAND... keeps the read's output, stderr and exit status, so a failed read is reported beside the
# others in a full report.
read_beside() {
  local name=$1 status=0
  shift
  limited "$limit" "$@" > "$work/$name.out" 2> "$work/$name.err" || status=$?
  printf '%s' "$status" > "$work/$name.status"
}

# read_system_volumes reads every node's volume statuses through the Talos API and slurps talosctl's stream of objects
# into one array.
read_system_volumes() {
  local nodes
  nodes=$(jq -r '[.items[].status.addresses[]? | select(.type == "InternalIP") | .address] | join(",")' "$work/nodes.json") || nodes=
  read_beside system-volumes talosctl -n "$nodes" get volumestatus -o json
  jq -s . "$work/system-volumes.out" > "$work/system-volumes.json" 2> /dev/null || true
}

# read_subscription_resources reads each kind on its own, all of them within one time limit, and keeps a kind only when
# its read exits 0 with a list, so a read that fails or is cut short leaves its kind out of the report.
read_subscription_resources() {
  local kind
  mkdir "$work/kinds"
  # shellcheck disable=SC2016 # the inner script expands its own arguments
  limited "$limit" bash -c 'dir=$1; shift; for kind; do
      kubectl get "$kind" -A -l mitstack.dev/subscription -o json > "$dir/$kind.read" && mv "$dir/$kind.read" "$dir/$kind.json"
    done' _ "$work/kinds" "${subscription_kinds[@]}" || true
  : > "$work/subscription-resources.json"
  for kind in "${subscription_kinds[@]}"; do
    [ -f "$work/kinds/$kind.json" ] || continue
    if jq -c --arg kind "$kind" '{kind: $kind, objects: [.items[] | {
        namespace: (.metadata.namespace // null), kind: $kind, name: .metadata.name,
        subscription: .metadata.labels["mitstack.dev/subscription"]
      }]}' "$work/kinds/$kind.json" > "$work/kind.read" 2>/dev/null; then
      cat "$work/kind.read" >> "$work/subscription-resources.json"
    fi
  done
}

# report_failure NAME STATUS posts a report whose error names the read, how it ended and the last 1 KiB of its
# stderr, keeping no NUL and no line that holds the report token or looks like a bearer token or a JWT.
report_failure() {
  local name=$1 status=$2
  tr -d '\000' < "$work/read.err" \
    | { grep -avF -- "$token" || true; } \
    | { grep -avE '[Bb]earer|eyJ[A-Za-z0-9_-]+\.' || true; } \
    | tail -c 1024 > "$work/read.tail"
  cat "$work/read.tail" >&2
  rm -f "$work/proof"
  jq -n \
    --arg subscription "$SUBSCRIPTION_ID" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg read "$name" \
    --argjson status "$status" \
    --rawfile stderr "$work/read.tail" '{
      subscriptionId: $subscription,
      reportedAt: $at,
      error: ({read: $read, stderr: $stderr} + if $status == 124 then {timedOut: true} else {exitStatus: $status} end)
    }' > "$work/body.json"
  post
  fail "$name ended with status $status, and the report names it"
}

# body prints the report on one line, so its length is what jq measures; a member line is NODE ID HOSTNAME PEER-URLS
# CLIENT-URLS LEARNER, and a status line's cells, split at talosctl's padding of three spaces, are NODE MEMBER DB-SIZE
# IN-USE LEADER RAFT-INDEX RAFT-TERM RAFT-APPLIED-INDEX LEARNER PROTOCOL STORAGE ERRORS. Every Application's applied objects are carried only while the body fits the platform's 256 KiB.
body() {
  limited "$limit" jq -c -n \
    --arg subscription "$SUBSCRIPTION_ID" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --slurpfile nodes "$work/nodes.json" \
    --slurpfile applications "$work/applications.json" \
    --slurpfile jwks "$work/jwks.json" \
    --rawfile members "$work/members.txt" \
    --rawfile etcd_status "$work/etcd-status.out" \
    --rawfile etcd_status_err "$work/etcd-status.err" \
    --argjson etcd_status_status "$(cat "$work/etcd-status.status")" \
    --rawfile pods "$work/pods.out" \
    --rawfile pods_err "$work/pods.err" \
    --argjson pods_status "$(cat "$work/pods.status")" \
    --slurpfile routes "$work/routes.json" \
    --rawfile ingresses "$work/argocd-ingresses.out" \
    --rawfile ingresses_err "$work/argocd-ingresses.err" \
    --argjson ingresses_status "$(cat "$work/argocd-ingresses.status")" \
    --rawfile default_deny "$work/default-deny.out" \
    --rawfile default_deny_err "$work/default-deny.err" \
    --argjson default_deny_status "$(cat "$work/default-deny.status")" \
    --slurpfile openbao "$work/openbao.json" \
    --rawfile cnpg "$work/cloudnative-pg.out" \
    --rawfile cnpg_err "$work/cloudnative-pg.err" \
    --argjson cnpg_status "$(cat "$work/cloudnative-pg.status")" \
    --rawfile storage "$work/storage-and-services.out" \
    --rawfile storage_err "$work/storage-and-services.err" \
    --argjson storage_status "$(cat "$work/storage-and-services.status")" \
    --rawfile volumes "$work/system-volumes.json" \
    --rawfile volumes_err "$work/system-volumes.err" \
    --argjson volumes_status "$(cat "$work/system-volumes.status")" \
    --arg made_at "$made_at" \
    --slurpfile kinds "$work/subscription-resources.json" \
    --rawfile mesh_inside "$work/mesh-inside.out" \
    --rawfile mesh_inside_err "$work/mesh-inside.err" \
    --argjson mesh_inside_status "$(cat "$work/mesh-inside.status")" \
    --rawfile mesh_outside "$work/mesh-outside.out" \
    --rawfile mesh_outside_err "$work/mesh-outside.err" \
    --argjson mesh_outside_status "$(cat "$work/mesh-outside.status")" \
    --arg mesh_answer "$MESH_PROBE_ANSWER" \
    --arg mesh_identity "$MESH_PROBE_IDENTITY" \
    --arg mesh_configmap "$MESH_PROBE_CONFIGMAP" \
    --argjson mesh_max_age "$mesh_probe_max_age" \
    --from-file /dev/stdin <<'JQ'
    def talos_version: (.status.nodeInfo.osImage // "") | (try capture("^Talos \\((?<v>[^)]+)\\)").v catch null);
    def ready: [.status.conditions[]? | select(.type == "Ready") | .status == "True"] | any;
    def cloudnative_pg: {
      namespace: .metadata.namespace, kind: .kind, name: .metadata.name,
      subscription: (.metadata.labels["mitstack.dev/subscription"] // null)
    } + if .kind == "Cluster" then
      {phase: .status.phase, instances: .status.instances, readyInstances: .status.readyInstances,
        currentPrimary: (.status.currentPrimary // null)}
    else
      {applied: .status.applied, message: (.status.message // null | if . then .[0:256] else . end)}
    end;
    # zone_label_managers are the field managers whose entry in managedFields holds the zone label.
    def zone_label_managers: [.metadata.managedFields[]?
      | select(.fieldsV1["f:metadata"]["f:labels"]["f:topology.kubernetes.io/zone"] != null)
      | {manager, operation, time}];
    def storage: {
      claims: [.items[] | select(.kind == "PersistentVolumeClaim") | {
        namespace: .metadata.namespace, name: .metadata.name,
        storageClass: (.spec.storageClassName // null), phase: (.status.phase // null)
      }],
      defaultStorageClasses: [.items[] | select(.kind == "StorageClass")
        | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true") | .metadata.name]
    };
    def pod: {namespace: .metadata.namespace, name: .metadata.name, node: (.spec.nodeName // null), phase: (.status.phase // null)};
    def etcd_status_rows: [$etcd_status | split("\n") | .[1:][] | sub("\\s+$"; "") | select(test("\\S"))
      | [splits("\\s{2,}")] | {member: .[1], leader: .[4], errors: (.[11] // "")}];
    # etcd is each member with whether its node answered with no error, the leader the answers name, and the status
    # read's failure, so a member whose node is down reads unhealthy beside the read that names it.
    def etcd: etcd_status_rows as $rows | {
      members: [$members | split("\n") | .[1:][] | select(test("\\S")) | [splits("\\s+")] | {
        id: .[1], hostname: .[2], peerURLs: (.[3] | split(",")), learner: (.[5] == "true")
      } | .id as $id | . + {healthy: ([$rows[] | select(.member == $id and .errors == "")] != [])}],
      leader: ([$rows[] | .leader | select(test("^0+$") | not)] | .[0]),
      statusError: (if $etcd_status_status == 0 then null else {exitStatus: $etcd_status_status, stderr: $etcd_status_err[-1024:]} end)
    };
    def services: [.items[] | select(.kind == "Service") | {namespace: .metadata.namespace, name: .metadata.name, type: (.spec.type // null)}];
    def default_deny_policy: {
      namespace: .metadata.namespace, name: .metadata.name,
      podSelector: (.spec.podSelector // {}), policyTypes: (.spec.policyTypes // []),
      ingressRules: (.spec.ingress // [] | length), egressRules: (.spec.egress // [] | length),
      managedBy: (.metadata.labels["app.kubernetes.io/managed-by"] // null)
    };
    def system_volume: {
      node: .node, name: .metadata.id, phase: .spec.phase,
      encryptionProvider: (.spec.encryptionProvider // "none"),
      encryptionKeys: (.spec.configuredEncryptionKeys // []),
      lockedToState: (.spec.encryptionLockedToState // false)
    };
    # beside(status; stderr; refusal; read) is a read_beside read's result, or how it failed.
    def beside($status; $stderr; $refusal; read):
      if $status != 0 then {exitStatus: $status, stderr: $stderr[-1024:]}
      else try read catch {exitStatus: 0, stderr: $refusal}
      end;
    # An applied object carries no field but these five, so the list holds no Secret's contents.
    def applied: {group: (.group // ""), kind: .kind, namespace: (.namespace // null), name: .name, sync: (.status // "Unknown")};
    # A kind is listed whole or not at all, and 1000 objects keep the body inside the platform's 256 KiB.
    def subscription_resources:
      reduce $kinds[] as $read ({objects: [], kinds: []};
        if (.objects | length) + ($read.objects | length) <= 1000 then
          .objects += $read.objects | .kinds += [$read.kind]
        else . end);
    def failed($reason): {passed: false, reason: $reason};
    def call_from_inside:
      if $mesh_inside_status != 0 then
        failed("the call from inside the mesh ended with curl exit status \($mesh_inside_status): \($mesh_inside_err[-512:])")
      elif $mesh_inside != $mesh_answer then
        failed("the target answered \($mesh_inside[0:256] | tojson) where it serves \($mesh_answer | tojson)")
      else {passed: true} end;
    # outside_probe is what the outside probe last wrote, or an error naming why the report has nothing it can judge.
    def outside_probe:
      if $mesh_outside_status != 0 then
        {error: "reading the ConfigMap \($mesh_configmap) ended with exit status \($mesh_outside_status): \($mesh_outside_err[-512:])"}
      else (try ($mesh_outside | fromjson | .data.result | fromjson) catch null) as $probe
        | (try (($at | fromdateiso8601) - ($probe.probedAt | fromdateiso8601)) catch null) as $age
        | if ($probe | type) != "object" then {error: "the ConfigMap \($mesh_configmap) holds no result the outside probe wrote"}
          elif $age == null then {error: "the outside probe's result in \($mesh_configmap) names no time it was made"}
          elif $age > $mesh_max_age then
            {error: "the outside probe last wrote at \($probe.probedAt), over \($mesh_max_age / 60) minutes before this report"}
          elif $age < -60 then {error: "the outside probe's result in \($mesh_configmap) names \($probe.probedAt), after this report"}
          else $probe end
      end;
    # judged turns a stored result whose shape breaks a judgement into a failed result, so the member never goes missing.
    def judged(f): try f catch failed("the outside probe's result in \($mesh_configmap) cannot be judged: \(tostring)");
    def workload_identity($probe):
      ($probe.handshake.certificate // null) as $cert
      | ($probe.probedAt // null) as $probed
      | ($cert.identities // []) as $names
      | if $probe.error then failed($probe.error)
        elif $cert == null then
          failed("no workload certificate came back from port 15008: openssl s_client ended with exit status \($probe.handshake.exitStatus): \($probe.handshake.stderr)")
        elif ($names | index($mesh_identity)) == null then
          failed("the certificate on port 15008 names \($names | if . == [] then "no identity" else join(", ") end), not \($mesh_identity)")
        elif (try (($probed | fromdateiso8601) as $t | $t < ($cert.notBefore | fromdateiso8601) or $t > ($cert.notAfter | fromdateiso8601))
            catch true) then
          failed("the certificate on port 15008 holds from \($cert.notBefore) to \($cert.notAfter), which leaves out \($probed)")
        else {passed: true, identity: $mesh_identity, serial: $cert.serial, notBefore: $cert.notBefore, notAfter: $cert.notAfter} end
      | . + {probedAt: $probed};
    # A plain call passes only when the target took the connection and closed it unanswered, curl's 52 or 56, while
    # ztunnel held the target's certificate on 15008 and the target answered the report, so a target that is down or
    # unreachable never reads as refused.
    def plain_call_from_outside($probe; $identity; $inside):
      ($probe.plainCall // {}) as $call
      | if $probe.error then failed($probe.error)
        elif $call.exitStatus == 0 then failed("the target answered a call that carried no workload identity: \($call.answer | tojson)")
        elif [52, 56] | index($call.exitStatus) | not then
          failed("the plain call never reached the target: curl ended with exit status \($call.exitStatus): \($call.stderr)")
        elif $identity.passed | not then
          failed("the target closed the plain call, and no workload certificate shows the mesh holds it: \($identity.reason)")
        elif $inside.passed | not then
          failed("the target closed the plain call, but it did not answer the report either, so it may be down: \($inside.reason)")
        else {passed: true, curlExitStatus: $call.exitStatus, stderr: $call.stderr} end
      | . + {probedAt: ($probe.probedAt // null)};
    def mesh:
      outside_probe as $probe | call_from_inside as $inside | judged(workload_identity($probe)) as $identity | {
        callFromInside: $inside,
        plainCallFromOutside: judged(plain_call_from_outside($probe; $identity; $inside)),
        workloadIdentity: $identity
      };
    subscription_resources as $listed | {
      subscriptionId: $subscription,
      reportedAt: $at,
      madeAt: $made_at,
      nodes: [$nodes[0].items[] | {
        name: .metadata.name,
        ready: ready,
        talosVersion: talos_version,
        kubeletVersion: .status.nodeInfo.kubeletVersion,
        internalIP: ([.status.addresses[]? | select(.type == "InternalIP") | .address] | first),
        allocatable: .status.allocatable,
        zone: (.metadata.labels["topology.kubernetes.io/zone"] // null),
        region: (.metadata.labels["topology.kubernetes.io/region"] // null),
        zoneLabelManagers: zone_label_managers
      }],
      etcd: etcd,
      systemVolumes: beside($volumes_status; $volumes_err;
        "talosctl exited 0, and its output is not the volume statuses this report reads";
        $volumes | fromjson | [.[] | select(.metadata.id == "STATE" or .metadata.id == "EPHEMERAL") | system_volume]
          | if . == [] then error("no STATE or EPHEMERAL") else . end),
      argocd: {
        applications: [$applications[0].items[] | {
          name: .metadata.name,
          health: (.status.health.status // "Unknown"),
          sync: (.status.sync.status // "Unknown"),
          resources: [.status.resources[]? | applied]
        }],
        routes: $routes,
        services: beside($storage_status; $storage_err;
          "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
          $storage | fromjson | [services[] | select(.namespace == "argocd") | {name, type}]),
        ingresses: beside($ingresses_status; $ingresses_err; "kubectl exited 0, and its output is not Argo CD's Ingresses";
          $ingresses | fromjson | [.items[] | {name: .metadata.name}])
      },
      kubernetes: {argocdPods: beside($pods_status; $pods_err; "kubectl exited 0, and its output is not the pods this report reads";
        $pods | fromjson | [.items[] | select(.metadata.namespace == "argocd")
          | {name: .metadata.name, containers: [.spec.containers[] | {image}]}])},
      cloudNativePG: beside($cnpg_status; $cnpg_err;
        "kubectl exited 0, and its output is not the CloudNativePG objects this report reads";
        $cnpg | fromjson | [.items[] | cloudnative_pg]),
      pods: beside($pods_status; $pods_err; "kubectl exited 0, and its output is not the pods this report reads";
        $pods | fromjson | [.items[] | pod]),
      openbao: {members: $openbao, leader: ([$openbao[] | select(.active == true) | .name] | .[0])},
      storage: beside($storage_status; $storage_err;
        "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
        $storage | fromjson | storage),
      services: beside($storage_status; $storage_err;
        "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
        $storage | fromjson | services),
      defaultDenyPolicies: beside($default_deny_status; $default_deny_err;
        "kubectl exited 0, and its output is not the default-deny NetworkPolicies this report reads";
        $default_deny | fromjson | [.items[] | default_deny_policy]),
      subscriptionResources: $listed.objects,
      subscriptionResourceKinds: $listed.kinds,
      # The report cannot read these three yet, so each says why and its check fails with that reason.
      repositorySecrets: {unreadable: "the report holds no Secret right, and no list of the declared repository credentials reaches it yet"},
      admission: {unreadable: "the cluster has no admission probe namespace for the report to dry-run fixtures into yet"},
      securityScans: {unreadable: "no kube-bench or kubescape scan runs on this cluster yet, so the report has no summary to carry"},
      jwks: $jwks[0],
      mesh: mesh,
      checks: {contract: "node-report/v1", names: ["routes", "exposure", "credentials", "version", "advisories", "admission",
        "default-deny", "cis-benchmark", "nsa-scan"]}
    } | if tojson | utf8bytelength < 256 * 1024 then . else del(.argocd.applications[].resources) end
JQ
}

sign() {
  read_into "kubectl create token" "$work/proof" 5 bash -c "$REPORT_PROOF_SCRIPT" token-request.sh \
    "$work/body.json" "$REPORT_URL" "${POD_NAMESPACE:-mitstack-node}" mitstack-node-report
  [ -s "$work/proof" ] || { echo "the proof script exited 0 and printed no proof" >> "$work/read.err"; report_failure "kubectl create token" 0; }
}

post() {
  local status
  printf 'Authorization: Bearer %s\n' "$token" > "$work/headers"
  [ ! -s "$work/proof" ] || printf 'Mitstack-Report-Proof: %s\n' "$(cat "$work/proof")" >> "$work/headers"
  status=$(curl -sS --proto '=https' --tlsv1.2 --connect-timeout 5 --max-time 20 \
    -X POST -H @"$work/headers" -H 'Content-Type: application/json' \
    --data-binary @"$work/body.json" -o /dev/null -w '%{http_code}' \
    "$REPORT_URL") || status=000
  case $status in
    2[0-9][0-9]) echo "report: the platform took the report" ;;
    *) fail "the platform did not take the report: HTTP $status" ;;
  esac
}

main() {
  : "${REPORT_URL:?names the report endpoint of the platform}" "${SUBSCRIPTION_ID:?names the subscription}" "${HOST_IP:?names the node this pod runs on}"
  : "${MESH_PROBE_URL:?names the target of the mesh probe}" "${MESH_PROBE_ANSWER:?names what the target answers}"
  : "${MESH_PROBE_IDENTITY:?names the SPIFFE identity of the target}" "${MESH_PROBE_CONFIGMAP:?names the ConfigMap of the outside probe}"
  : "${OPENBAO_MEMBERS:?names each OpenBao member and its URL}" "${OPENBAO_CA_FILE:?names the authority that signs the OpenBao certificate}"
  : "${ARGOCD_URL:?names the URL of the Argo CD server}"
  local token_file=${REPORT_TOKEN_FILE:-/var/run/mitstack/token} status=0
  [ -s "$token_file" ] || fail "no report token at $token_file"
  token=$(cat "$token_file")
  made_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  collect
  body > "$work/body.json" 2> "$work/read.err" || status=$?
  [ "$status" -eq 0 ] || report_failure body "$status"
  sign
  post
}

main "$@"
