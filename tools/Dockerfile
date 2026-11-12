# Each binary is checked against its sha256 pin: talosctl and kubectl against tools.env, which the gateway checks them
# against once it copies them out, and jq against jq.env, the file the report's tests fetch the same jq by. coreutils
# replaces busybox's timeout, which exits 143 on a timeout where the node report reads GNU timeout's 124. ssh refuses to
# run for a uid with no passwd entry, so the image names the uid every non-root container runs as. The image carries the
# node report's, its outside mesh probe's, the security scan's, the topology labeller's and the cluster tokens' scripts,
# since a node's machine configuration has no room for them inside the builder's user data. The security scan's
# kube-bench with its own cis-1.10 config, kubescape and the NSA framework with its default inputs are checked against
# scan.env. procps gives kube-bench the ps it finds each component's process with, which busybox's ps refuses. The image
# carries every manifest a gateway serves at its repository path under the scripts' directory, since a gateway copies
# talosctl, kubectl and those manifests out of it. It builds from the repository's root: docker build -f tools/Dockerfile .
FROM docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8

COPY tools/tools.env tools/jq.env tools/scan.env /tmp/

RUN apk add --no-cache bash ca-certificates coreutils curl git gpg openssh-client openssl procps \
 && . /tmp/tools.env \
 && . /tmp/jq.env \
 && curl -fsSL --proto '=https' -o /usr/local/bin/talosctl "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" \
 && echo "${TALOSCTL_SHA256}  /usr/local/bin/talosctl" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/amd64/kubectl" \
 && echo "${KUBECTL_SHA256}  /usr/local/bin/kubectl" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/bin/jq "https://github.com/jqlang/jq/releases/download/${JQ_VERSION}/jq-linux-amd64" \
 && echo "${JQ_SHA256}  /usr/local/bin/jq" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/talosctl /usr/local/bin/kubectl /usr/local/bin/jq \
 && . /tmp/scan.env \
 && curl -fsSL --proto '=https' -o /tmp/kube-bench.tar.gz "https://github.com/aquasecurity/kube-bench/releases/download/${KUBE_BENCH_VERSION}/kube-bench_${KUBE_BENCH_VERSION#v}_linux_amd64.tar.gz" \
 && echo "${KUBE_BENCH_SHA256}  /tmp/kube-bench.tar.gz" | sha256sum -c - \
 && mkdir -p /usr/local/lib/mitstack/kube-bench /usr/local/lib/mitstack/kubescape \
 && tar -xzf /tmp/kube-bench.tar.gz -C /usr/local/lib/mitstack/kube-bench kube-bench cfg/config.yaml \
    cfg/cis-1.10/controlplane.yaml cfg/cis-1.10/etcd.yaml cfg/cis-1.10/master.yaml \
    cfg/cis-1.10/node.yaml cfg/cis-1.10/policies.yaml \
 && mv /usr/local/lib/mitstack/kube-bench/kube-bench /usr/local/bin/kube-bench \
 && curl -fsSL --proto '=https' -o /usr/local/bin/kubescape "https://github.com/kubescape/kubescape/releases/download/${KUBESCAPE_VERSION}/kubescape_${KUBESCAPE_VERSION#v}_linux_amd64" \
 && echo "${KUBESCAPE_SHA256}  /usr/local/bin/kubescape" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/lib/mitstack/kubescape/nsa "https://github.com/kubescape/regolibrary/releases/download/${KUBESCAPE_CONTROLS_VERSION}/nsa" \
 && echo "${NSA_FRAMEWORK_SHA256}  /usr/local/lib/mitstack/kubescape/nsa" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/lib/mitstack/kubescape/default_config_inputs "https://github.com/kubescape/regolibrary/releases/download/${KUBESCAPE_CONTROLS_VERSION}/default_config_inputs" \
 && echo "${NSA_CONFIG_INPUTS_SHA256}  /usr/local/lib/mitstack/kubescape/default_config_inputs" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/kube-bench /usr/local/bin/kubescape \
 && rm /tmp/tools.env /tmp/jq.env /tmp/scan.env /tmp/kube-bench.tar.gz \
 && addgroup -g 65532 mitstack \
 && adduser -D -H -h /tmp -s /sbin/nologin -G mitstack -u 65532 mitstack

COPY report/scripts/report.sh report/scripts/mesh-probe.sh report/scripts/security-scan.sh topology/scripts/label-nodes.sh controlplane/scripts/cluster-tokens.sh /usr/local/lib/mitstack/
COPY tools/kube-bench-talos.yaml /usr/local/lib/mitstack/kube-bench/cfg/cis-1.10/config.yaml
COPY components/ /usr/local/lib/mitstack/components/

USER 65532:65532
