# The tools image the etcd member removal and the node report run in: bash,
# curl, jq, talosctl and kubectl, each binary checked against the sha256 that
# tools.env pins, the same file the gateway installs its binaries from.
# The kit's release job pushes and signs it, and tools.Image names it.
FROM docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8

COPY tools.env /tmp/tools.env

RUN apk add --no-cache bash ca-certificates curl jq \
 && . /tmp/tools.env \
 && curl -fsSL --proto '=https' -o /usr/local/bin/talosctl "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" \
 && echo "${TALOSCTL_SHA256}  /usr/local/bin/talosctl" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/amd64/kubectl" \
 && echo "${KUBECTL_SHA256}  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/talosctl /usr/local/bin/kubectl \
 && rm /tmp/tools.env

USER 65532:65532
