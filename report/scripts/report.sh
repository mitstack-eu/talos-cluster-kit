#!/usr/bin/env bash
# Posts the node report: nodes with their zone and region labels, taints and unschedulable flag, etcd members with their
# health and the leader, each node's addresses with their types and its STATE and EPHEMERAL encryption, Argo CD's health
# of each Application and each object it applied, what each Argo CD route answers a caller without a session, Argo CD's
# Services, Ingresses and pod images, every Ingress with its class and the addresses its status holds, CloudNativePG's
# objects with each cluster's primary, archiving and last backup, the plugins it finds and each ObjectStore's recovery
# windows, every pod's node, phase and container states, each image reference the pods and the workloads' pod templates
# name, each OpenBao member's seal state and the active one, each claim with its size and the default storage classes,
# every Service's type, each default-deny NetworkPolicy, the objects labelled mitstack.dev/subscription, the API
# server's key set, the mesh probe's results, the outside probe's reading of each gateway's mirror, whether a TLS
# connection to Let's Encrypt's ACME API opens through the network's default route, each repository credential Argo CD's
# SealedSecrets declare with each AppProject's source repositories, the API server's answer to a dry run of each
# admission fixture, the security scan's last runs, newest Job with its events and kept summaries, and a checks member
# naming each check of the platform's verdict, signed with a token the API server mints for the body through block-kit's
# token-request.sh in REPORT_PROOF_SCRIPT.
# When a read, the body or the token request fails or times out, it posts an unsigned report whose error names it
# instead, then exits non-zero.
# Usage: REPORT_URL=<https url> SUBSCRIPTION_ID=<id> HOST_IP=<this pod's node> REPORT_PROOF_SCRIPT=<script> MESH_PROBE_URL=<target's url> MESH_PROBE_ANSWER=<its answer> MESH_PROBE_IDENTITY=<its SPIFFE id> MESH_PROBE_CONFIGMAP=<namespace/name> OPENBAO_MEMBERS="<name>=<https url> ..." OPENBAO_CA_FILE=<pem> ARGOCD_URL=<https url> GATEWAYS="<address> ..." EGRESS_URL=<https url> [POD_NAMESPACE=mitstack-node] [REPORT_TOKEN_FILE=/var/run/mitstack/token] [REPORT_READ_SECONDS=21] report.sh
# It exists so the platform sees a cluster it never dials into, as ADR 0072 requires, with os:reader and no Secret right.
set -euo pipefail

# Nine limits of 21 seconds, the token request's 5, each with a 5-second kill grace, and the post's 20 make 264, which
# leaves 16 seconds of the Job's activeDeadlineSeconds of 280 for the tools to start. The mesh, pod, workload, Argo CD,
# default-deny, credential, ObjectStore, scan, OpenBao, etcd status, gateway and egress reads run beside them, each within one limit,
# and the admission fixtures' read and their dry runs within two.
limit=${REPORT_READ_SECONDS:-21}
# The outside probe writes every five minutes, so a result three runs old means it stopped.
mesh_probe_max_age=900
# The report dry-runs the admission fixtures in the probe namespace, and reads what the security scan last kept.
probe_namespace=mitstack-admission-probe
scans_configmap=kube-system/mitstack-security-scans
scan_cronjob=mitstack-security-scan
# The report keeps the newest twenty events of the newest scan Job and its pods, since a failing Job repeats an event.
scan_events_kept=20
# CloudNativePG finds a plugin only by a Service in the namespace its operator runs in.
cnpg_namespace=cnpg-system
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
  local mesh_reads cluster_reads argocd_reads etcd_reads egress_reads
  read_mesh & mesh_reads=$!
  read_egress & egress_reads=$!
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
  wait "$mesh_reads" "$cluster_reads" "$argocd_reads" "$etcd_reads" "$egress_reads"
  openbao_members > "$work/openbao.json"
}

# read_egress opens a TLS connection to EGRESS_URL. -q reads no ~/.curlrc, and without --fail any answer over a verified
# connection counts.
read_egress() {
  read_beside egress curl -q -sS --proto '=https' --max-time 5 -o /dev/null "$EGRESS_URL"
}

# read_cluster reads every pod, every workload with a pod template, each default-deny NetworkPolicy and each ObjectStore
# of the Barman Cloud plugin, whose kind a cluster without the plugin does not serve, and asks each
# OpenBao member for its seal state and whether it is the active one, which OpenBao answers without a token.
read_cluster() {
  local member
  read_beside pods kubectl get pods -A -o json &
  read_beside workloads kubectl get deployments.apps,statefulsets.apps,daemonsets.apps,jobs.batch,cronjobs.batch -A -o json &
  read_beside default-deny kubectl get networkpolicies.networking.k8s.io -A --field-selector metadata.name=mitstack-default-deny -o json &
  read_beside sealed-secrets kubectl get sealedsecrets.bitnami.com -n argocd -o json &
  read_beside app-projects kubectl get appprojects.argoproj.io -n argocd -o json &
  read_beside object-stores kubectl get objectstores.barmancloud.cnpg.io -A -o json &
  read_beside security-scans kubectl get configmap "${scans_configmap#*/}" -n "${scans_configmap%%/*}" -o json &
  read_beside security-scan-cronjob kubectl get cronjob.batch "$scan_cronjob" -n "${scans_configmap%%/*}" -o json &
  read_beside security-scan-events kubectl get events -n "${scans_configmap%%/*}" -o json &
  read_admission &
  mkdir "$work/openbao"
  for member in $OPENBAO_MEMBERS; do
    read_beside "openbao/${member%%=*}.seal-status" curl -sS --fail --proto '=https' --cacert "$OPENBAO_CA_FILE" \
      --max-time 5 "${member#*=}/v1/sys/seal-status" &
    read_beside "openbao/${member%%=*}.leader" curl -sS --fail --proto '=https' --cacert "$OPENBAO_CA_FILE" \
      --max-time 5 "${member#*=}/v1/sys/leader" &
  done
  wait
}

# read_admission reads the probe namespace's fixtures and has the API server dry-run each one named for a rule, all at
# the same time, so each answer is the admission chain's verdict on that fixture and nothing is created.
read_admission() {
  local rule dir=$work/fixtures
  mkdir "$dir"
  : > "$work/admission.json"
  read_beside admission-fixtures kubectl get configmap admission-fixtures -n "$probe_namespace" -o json
  [ "$(cat "$work/admission-fixtures.status")" = 0 ] || return 0
  jq -r '.data // {} | keys[] | select(test("^[a-z0-9-]+$"))' "$work/admission-fixtures.out" > "$dir/rules" 2> /dev/null || return 0
  while read -r rule; do
    jq -j --arg rule "$rule" '.data[$rule]' "$work/admission-fixtures.out" > "$dir/$rule.yaml"
    dry_run "$dir/$rule" &
  done < "$dir/rules"
  wait
  while read -r rule; do
    jq -c -n --arg rule "$rule" --rawfile fixture "$dir/$rule.yaml" --rawfile answer "$dir/$rule.answer" \
      --argjson status "$(cat "$dir/$rule.status")" \
      '{rule: $rule, fixture: $fixture[0:4096], exitStatus: $status, message: $answer[0:2048]}' >> "$work/admission.json"
  done < "$dir/rules"
}

# dry_run PATH sends PATH.yaml to the API server as a server-side dry run, keeping its answer and exit status beside it.
dry_run() {
  local status=0
  limited "$limit" kubectl create --dry-run=server -n "$probe_namespace" -f "$1.yaml" > "$1.answer" 2>&1 || status=$?
  printf '%s' "$status" > "$1.status"
}

# read_etcd_status asks each etcd member's node for that member's status through the Talos API. talosctl prints a row
# for each node that answers and exits non-zero naming each node that does not.
read_etcd_status() {
  local nodes
  nodes=$(awk 'NR > 1 && NF { split($4, urls, ","); sub("^https://", "", urls[1]); sub(":[0-9]+$", "", urls[1]); print urls[1] }' \
    "$work/members.txt" | paste -sd, -) || nodes=
  read_beside etcd-status talosctl -n "$nodes" etcd status
}

# read_argocd makes the one Ingress read that argocd.ingresses and ingresses share, and asks each of Argo CD's routes
# once, all at the same time.
read_argocd() {
  local i
  read_beside ingresses kubectl get ingresses.networking.k8s.io -A -o json &
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
# follows no redirect, and keeps its status, its size and one boolean of the two answers Argo CD gives without a session:
# userinfo's loggedIn, and settings' userLoginsDisabled, which reads true once no account can sign in. -q reads no ~/.curlrc
# that could add a credential. -k accepts the certificate Argo CD signs for itself, since the request carries nothing
# to steal and no text of the answer leaves the pod.
argocd_route() {
  local path=${argocd_routes[$1]} out=$work/routes/$1 status ended=0
  status=$(curl -q -s -k --proto '=https' --connect-timeout 1 --max-time 5 --max-filesize 65536 -o "$out.body" \
    -w '%{http_code}' "$ARGOCD_URL$path") || ended=$?
  [ -f "$out.body" ] || : > "$out.body"
  jq -c -n --arg path "$path" --argjson status "$((10#${status:-0}))" --argjson ended "$ended" \
    --argjson bytes "$(wc -c < "$out.body")" --rawfile body "$out.body" '
    def kept_boolean($name):
      if $status != 200 then null
      else try ($body | fromjson | .[$name] | if type == "boolean" then . else null end) catch null end;
    if $ended != 0 then {path: $path, status: 0, bytes: 0, error: "curl exit \($ended)"}
    else {path: $path, status: $status, bytes: $bytes}
      + if $path == "/api/v1/session/userinfo" then {loggedIn: kept_boolean("loggedIn")}
        elif $path == "/api/v1/settings" then {userLoginsDisabled: kept_boolean("userLoginsDisabled")}
        else {} end
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
    --rawfile workloads "$work/workloads.out" \
    --rawfile workloads_err "$work/workloads.err" \
    --argjson workloads_status "$(cat "$work/workloads.status")" \
    --slurpfile routes "$work/routes.json" \
    --rawfile ingresses "$work/ingresses.out" \
    --rawfile ingresses_err "$work/ingresses.err" \
    --argjson ingresses_status "$(cat "$work/ingresses.status")" \
    --rawfile default_deny "$work/default-deny.out" \
    --rawfile default_deny_err "$work/default-deny.err" \
    --argjson default_deny_status "$(cat "$work/default-deny.status")" \
    --slurpfile openbao "$work/openbao.json" \
    --rawfile cnpg "$work/cloudnative-pg.out" \
    --rawfile cnpg_err "$work/cloudnative-pg.err" \
    --argjson cnpg_status "$(cat "$work/cloudnative-pg.status")" \
    --arg cnpg_namespace "$cnpg_namespace" \
    --rawfile object_stores "$work/object-stores.out" \
    --rawfile object_stores_err "$work/object-stores.err" \
    --argjson object_stores_status "$(cat "$work/object-stores.status")" \
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
    --arg gateways "$GATEWAYS" \
    --arg egress_url "$EGRESS_URL" \
    --rawfile egress_err "$work/egress.err" \
    --argjson egress_status "$(cat "$work/egress.status")" \
    --rawfile sealed "$work/sealed-secrets.out" \
    --rawfile sealed_err "$work/sealed-secrets.err" \
    --argjson sealed_status "$(cat "$work/sealed-secrets.status")" \
    --rawfile projects "$work/app-projects.out" \
    --rawfile projects_err "$work/app-projects.err" \
    --argjson projects_status "$(cat "$work/app-projects.status")" \
    --slurpfile admission "$work/admission.json" \
    --rawfile fixtures_err "$work/admission-fixtures.err" \
    --argjson fixtures_status "$(cat "$work/admission-fixtures.status")" \
    --rawfile scans "$work/security-scans.out" \
    --rawfile scans_err "$work/security-scans.err" \
    --argjson scans_status "$(cat "$work/security-scans.status")" \
    --arg scans_configmap "$scans_configmap" \
    --rawfile scan_job "$work/security-scan-cronjob.out" \
    --rawfile scan_job_err "$work/security-scan-cronjob.err" \
    --argjson scan_job_status "$(cat "$work/security-scan-cronjob.status")" \
    --arg scan_cronjob "$scan_cronjob" \
    --argjson scan_events_kept "$scan_events_kept" \
    --rawfile scan_events "$work/security-scan-events.out" \
    --rawfile scan_events_err "$work/security-scan-events.err" \
    --argjson scan_events_status "$(cat "$work/security-scan-events.status")" \
    --from-file /dev/stdin <<'JQ'
    def talos_version: (.status.nodeInfo.osImage // "") | (try capture("^Talos \\((?<v>[^)]+)\\)").v catch null);
    def ready: [.status.conditions[]? | select(.type == "Ready") | .status == "True"] | any;
    # Each message the report carries from a controller is cut to 256 characters, so its size has a bound.
    def at_most_256: if type == "string" then .[0:256] else . end;
    # condition is the Cluster's condition of one type as CloudNativePG wrote it, or null while the Cluster holds none.
    def condition($type): [.status.conditions[]? | select(.type == $type)
      | {status, reason: (.reason // null), message: (.message // null | at_most_256)}] | .[0];
    def cloudnative_pg: {
      namespace: .metadata.namespace, kind: .kind, name: .metadata.name,
      subscription: (.metadata.labels["mitstack.dev/subscription"] // null)
    } + if .kind == "Cluster" then
      {phase: .status.phase, instances: .status.instances, readyInstances: .status.readyInstances,
        currentPrimary: (.status.currentPrimary // null),
        continuousArchiving: condition("ContinuousArchiving"), lastBackup: condition("LastBackupSucceeded")}
    else
      {applied: .status.applied, message: (.status.message // null | at_most_256)}
    end;
    def cloudnative_pg_plugins: [.items[] | select(.kind == "Service" and .metadata.namespace == $cnpg_namespace)
      | .metadata.labels["cnpg.io/pluginName"] // empty] | unique;
    # CloudNativePG sets no backup time on a Cluster that backs up through a plugin, so the times come from the
    # ObjectStore, one window per server that writes to it.
    def object_store: {
      namespace: .metadata.namespace, kind: .kind, name: .metadata.name,
      subscription: (.metadata.labels["mitstack.dev/subscription"] // null),
      recoveryWindows: (.status.serverRecoveryWindow // {} | map_values({
        firstRecoverabilityPoint: (.firstRecoverabilityPoint // null),
        lastSuccessfulBackupTime: (.lastSuccessfulBackupTime // null),
        lastFailedBackupTime: (.lastFailedBackupTime // null)
      }))
    };
    # zone_label_managers are the field managers whose entry in managedFields holds the zone label.
    def zone_label_managers: [.metadata.managedFields[]?
      | select(.fieldsV1["f:metadata"]["f:labels"]["f:topology.kubernetes.io/zone"] != null)
      | {manager, operation, time}];
    def storage: {
      claims: [.items[] | select(.kind == "PersistentVolumeClaim") | {
        namespace: .metadata.namespace, name: .metadata.name,
        storageClass: (.spec.storageClassName // null), phase: (.status.phase // null),
        requestedSize: (.spec.resources.requests.storage // null), capacity: (.status.capacity.storage // null)
      }],
      defaultStorageClasses: [.items[] | select(.kind == "StorageClass")
        | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true") | .metadata.name]
    };
    # A container carries its state with the reason Kubernetes gives, and the message only while it waits, since a
    # terminated container's message is whatever the app wrote.
    def container: (.state // {}) as $s | {name: .name}
      + if $s.waiting then
          {state: "waiting", reason: ($s.waiting.reason // null), message: ($s.waiting.message // null | at_most_256)}
        elif $s.terminated then {state: "terminated", reason: ($s.terminated.reason // null)}
        elif $s.running then {state: "running"}
        else {state: null} end;
    def pod: {
      namespace: .metadata.namespace, name: .metadata.name, node: (.spec.nodeName // null), phase: (.status.phase // null),
      containers: [(.status.initContainerStatuses // []) + (.status.containerStatuses // []) | .[] | container]
    };
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
    # Of an Ingress's status only each address leaves the cluster, and its class is cut, so its size has a bound.
    def ingress: {
      namespace: .metadata.namespace, name: .metadata.name,
      class: (.spec.ingressClassName // .metadata.annotations["kubernetes.io/ingress.class"] // null | at_most_256),
      status: {loadBalancer: {ingress: [.status.loadBalancer.ingress[]? | {ip, hostname} | with_entries(select(.value != null))]}}
    };
    def ingresses(read): beside($ingresses_status; $ingresses_err; "kubectl exited 0, and its output is not the Ingresses this report reads";
      $ingresses | fromjson | read);
    def named_digest: (capture("@(?<d>sha256:[0-9a-f]{64})$").d // null);
    def containers: (.containers // []) + (.initContainers // []);
    # pod_images names each container's image with its own digest, or else the one the pod's status reports it runs by.
    def pod_images: ([(.status.containerStatuses // []) + (.status.initContainerStatuses // []) | .[]
        | {key: .name, value: ((.imageID // "") | named_digest)}] | from_entries) as $ran
      | .spec | containers | .[] | {image, digest: ((.image | named_digest) // $ran[.name])};
    def template_images: (if .kind == "CronJob" then .spec.jobTemplate.spec.template else .spec.template end).spec
      | containers | .[] | {image, digest: (.image | named_digest)};
    # images lists each reference once, and a template's reference with no digest only when no pod resolves it.
    def images($refs): ([$refs[] | select(.digest != null) | .image] | unique) as $resolved
      | [$refs[] | select(.digest != null or (.image as $i | any($resolved[]; . == $i) | not))] | unique;
    def image_list:
      beside($pods_status; $pods_err; "kubectl exited 0, and its output is not the pods this report reads";
        $pods | fromjson | [.items[] | pod_images]) as $ran
      | beside($workloads_status; $workloads_err; "kubectl exited 0, and its output is not the workloads this report reads";
        $workloads | fromjson | [.items[] | template_images]) as $templated
      | if ($ran | type) != "array" then $ran + {read: "pods"}
        elif ($templated | type) != "array" then $templated + {read: "workloads"}
        else images($ran + $templated) end;
    # Only identity, sync and health leave the cluster, so no Secret's contents do. Health is left out when Argo CD
    # keeps it off the Application, so a reader never takes an unread health for none.
    def applied($health_inline):
      {group: (.group // ""), kind: .kind, namespace: (.namespace // null), name: .name, sync: (.status // "Unknown")}
      + if $health_inline | not then {}
        elif .health.status == null or .health.status == "Healthy" then {health: (.health.status // null)}
        else {health: .health.status, message: (.health.message // null | at_most_256)} end;
    # The phase of Argo CD's last sync of an Application, with its message unless it succeeded, or null before its first.
    def last_sync: if . == null then null
      else {phase, message: (if .phase == "Succeeded" then null else .message // null | at_most_256 end)} end;
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
    # A sealed credential's template names the Secret it unseals into, its Argo CD type and its plain project and URL.
    def repository_credential: (.spec.template // {}) as $t | ($t.metadata.labels["argocd.argoproj.io/secret-type"] // null) as $type
      | select($type != null)
      | {name: ($t.metadata.name // .metadata.name), secretType: $type, project: ($t.data.project // ""), url: ($t.data.url // "")};
    # newest_scan_job is the scan CronJob's newest Job with its counts and conditions, or null while it owns none.
    def newest_scan_job:
      beside($workloads_status; $workloads_err; "kubectl exited 0, and its output is not the workloads this report reads";
        [$workloads | fromjson | .items[] | select(.kind == "Job" and any(.metadata.ownerReferences[]?; .kind == "CronJob" and .name == $scan_cronjob))]
        | sort_by(.metadata.creationTimestamp) | last
        | if . == null then null else {name: .metadata.name, createdAt: .metadata.creationTimestamp, startedAt: (.status.startTime // null),
            active: (.status.active // 0), succeeded: (.status.succeeded // 0), failed: (.status.failed // 0),
            conditions: [.status.conditions[]? | {type, status, reason: (.reason // null), message: (.message // null | at_most_256)}]} end);
    # scan_job_events are the newest events of that Job and of its pods, whose names start with the Job's, since a Job that
    # creates no pod says why only in an event.
    def scan_job_events($job):
      beside($scan_events_status; $scan_events_err; "kubectl exited 0, and its output is not the events this report reads";
        [$scan_events | fromjson | .items[] | select($job.name != null and (.involvedObject.name as $n | $n == $job.name or ($n | startswith("\($job.name)-"))))
          | {kind: .involvedObject.kind, name: .involvedObject.name, type, reason, message: (.message // null | at_most_256), count: (.count // null),
            lastSeenAt: (.series.lastObservedTime // .lastTimestamp // .eventTime // .metadata.creationTimestamp)}]
        | sort_by(.lastSeenAt | sub("\\.[0-9]+Z$"; "Z")) | reverse | .[0:$scan_events_kept]);
    # runner is when the scan CronJob last started a scan and when one last succeeded, or why the report could not read it.
    def runner:
      if $scan_job_status != 0 then {exitStatus: $scan_job_status, output: $scan_job_err[-512:]}
      else (try ($scan_job | fromjson | .status // {}) catch null) as $s
        | if ($s | type) != "object" then {exitStatus: 0, output: "kubectl exited 0, and its output is not the scan CronJob"}
          else newest_scan_job as $job | {exitStatus: 0, output: ("the CronJob \($scans_configmap | split("/")[0])/\($scan_cronjob) "
            + (if $s.lastScheduleTime then "last started a scan at \($s.lastScheduleTime)" else "has started no scan yet" end)
            + (if $s.lastSuccessfulTime then ", and one last succeeded at \($s.lastSuccessfulTime)" else ", and none has succeeded yet" end)),
            job: $job, events: scan_job_events($job)}
          end
      end;
    # kept is one summary the scan kept, while it is one JSON object within the 16 KiB the report carries of it.
    def kept($data; $key): $data[$key] as $v
      | if $v == null then null
        elif ($v | utf8bytelength) > 16384 then {unreadable: "the kept \($key) summary is \($v | utf8bytelength) bytes, over the 16384 the report carries"}
        else (try ($v | fromjson) catch null) as $o
          | if ($o | type) == "object" then $o else {unreadable: "the kept \($key) summary is not one JSON object"} end
        end;
    def security_scans:
      if $scans_status != 0 then
        {unreadable: "reading the ConfigMap \($scans_configmap) ended with exit status \($scans_status): \($scans_err[-512:])"}
      else (try ($scans | fromjson | .data // {}) catch null) as $data
        | if ($data | type) != "object" then {unreadable: "kubectl exited 0, and its output is not the ConfigMap \($scans_configmap)"}
          else {runner: runner, lastRun: kept($data; "last-run"), kubeBench: kept($data; "kube-bench"), kubescape: kept($data; "kubescape")}
          end
      end;
    def connection_words($read):
      if $read.connection == "answered" then "the mirror answered HTTP \($read.httpCode)"
      else ({closed: "nothing took the connection to the mirror's port", reset: "the connection was reset or closed before the mirror answered",
          "tls-failure": "the TLS handshake with the mirror failed", "timed-out": "the connection timed out"}[$read.connection]
          // "the outside probe read the connection as \($read.connection)")
        + ": curl ended with exit status \($read.exitStatus): \($read.stderr)"
      end;
    # gateway_readings is the reading of each gateway in the result of the outside probe, with the time it was taken.
    def gateway_readings($probe):
      [$gateways | splits(" +") | select(. != "")] as $addresses
      | try [$addresses[] as $address
          | if $probe.error then {address: $address, passed: false, reason: $probe.error}
            else ([$probe.gateways[]? | select(.address == $address)] | first) as $read
            | if $read == null then
                {address: $address, passed: false,
                  reason: "the outside probe's result in \($mesh_configmap) holds no reading of the gateway at \($address)"}
              elif $read.connection == "served" then {address: $address, passed: true, connection: "served", probedAt: $probe.probedAt}
              else {address: $address, passed: false, connection: $read.connection, probedAt: $probe.probedAt,
                reason: connection_words($read)} end
            end]
        catch (tostring as $e | [$addresses[] | {address: ., passed: false,
          reason: "the outside probe's result in \($mesh_configmap) cannot be judged: \($e)"}]);
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
        addresses: [.status.addresses[]? | {type, address}],
        allocatable: .status.allocatable,
        zone: (.metadata.labels["topology.kubernetes.io/zone"] // null),
        region: (.metadata.labels["topology.kubernetes.io/region"] // null),
        zoneLabelManagers: zone_label_managers,
        taints: [.spec.taints[]? | {key, value: (.value // null), effect}],
        unschedulable: (.spec.unschedulable // false)
      }],
      etcd: etcd,
      systemVolumes: beside($volumes_status; $volumes_err;
        "talosctl exited 0, and its output is not the volume statuses this report reads";
        $volumes | fromjson | [.[] | select(.metadata.id == "STATE" or .metadata.id == "EPHEMERAL") | system_volume]
          | if . == [] then error("no STATE or EPHEMERAL") else . end),
      argocd: {
        applications: [$applications[0].items[] | ((.status.resourceHealthSource // "") == "") as $health_inline | {
          name: .metadata.name,
          health: (.status.health.status // "Unknown"),
          sync: (.status.sync.status // "Unknown"),
          revision: .status.sync.revision,
          operation: (.status.operationState // null | last_sync),
          resources: [.status.resources[]? | applied($health_inline)]
        }],
        routes: $routes,
        services: beside($storage_status; $storage_err;
          "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
          $storage | fromjson | [services[] | select(.namespace == "argocd") | {name, type}]),
        ingresses: ingresses([.items[] | select(.metadata.namespace == "argocd") | {name: .metadata.name}])
      },
      kubernetes: {argocdPods: beside($pods_status; $pods_err; "kubectl exited 0, and its output is not the pods this report reads";
        $pods | fromjson | [.items[] | select(.metadata.namespace == "argocd")
          | {name: .metadata.name, containers: [.spec.containers[] | {image}]}])},
      cloudNativePG: beside($cnpg_status; $cnpg_err;
        "kubectl exited 0, and its output is not the CloudNativePG objects this report reads";
        $cnpg | fromjson | [.items[] | cloudnative_pg]),
      cloudNativePGPlugins: beside($storage_status; $storage_err;
        "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
        $storage | fromjson | cloudnative_pg_plugins),
      cloudNativePGObjectStores: beside($object_stores_status; $object_stores_err;
        "kubectl exited 0, and its output is not the ObjectStores this report reads";
        $object_stores | fromjson | [.items[] | object_store]),
      pods: beside($pods_status; $pods_err; "kubectl exited 0, and its output is not the pods this report reads";
        $pods | fromjson | [.items[] | pod]),
      images: image_list,
      openbao: {members: $openbao, leader: ([$openbao[] | select(.active == true) | .name] | .[0])},
      storage: beside($storage_status; $storage_err;
        "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
        $storage | fromjson | storage),
      services: beside($storage_status; $storage_err;
        "kubectl exited 0, and its output is not the claims, storage classes and Services this report reads";
        $storage | fromjson | services),
      ingresses: ingresses([.items[] | ingress]),
      defaultDenyPolicies: beside($default_deny_status; $default_deny_err;
        "kubectl exited 0, and its output is not the default-deny NetworkPolicies this report reads";
        $default_deny | fromjson | [.items[] | default_deny_policy]),
      subscriptionResources: $listed.objects,
      subscriptionResourceKinds: $listed.kinds,
      repositorySecrets: beside($sealed_status; $sealed_err; "kubectl exited 0, and its output is not Argo CD's SealedSecrets";
        $sealed | fromjson | [.items[] | repository_credential]),
      appProjects: beside($projects_status; $projects_err; "kubectl exited 0, and its output is not Argo CD's AppProjects";
        $projects | fromjson | [.items[] | {name: .metadata.name, sourceRepos: (.spec.sourceRepos // [])}]),
      admission: beside($fixtures_status; $fixtures_err; "kubectl exited 0, and its output is not the admission fixtures"; $admission),
      securityScans: security_scans,
      jwks: $jwks[0],
      mesh: mesh,
      gateways: gateway_readings(outside_probe),
      egress: (if $egress_status == 0 then {passed: true}
        else failed("the TLS connection to \($egress_url) ended with curl exit status \($egress_status): \($egress_err[-512:])") end),
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
  : "${ARGOCD_URL:?names the URL of the Argo CD server}" "${EGRESS_URL:?names where the egress reading opens a TLS connection}"
  : "${GATEWAYS?names the address of each gateway}"
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
