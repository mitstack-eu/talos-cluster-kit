# Each binary is checked against its sha256 pin: talosctl and kubectl against tools.env, the file the gateway installs
# them from, and jq against jq.env, the file the report's tests fetch the same jq by. coreutils replaces busybox's
# timeout, which exits 143 on a timeout where the node report reads GNU timeout's 124. ssh refuses to run for a uid
# with no passwd entry, so the image names the uid every Job runs as.
FROM docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8

COPY tools.env jq.env /tmp/

RUN apk add --no-cache bash ca-certificates coreutils curl git gpg openssh-client openssl \
 && . /tmp/tools.env \
 && . /tmp/jq.env \
 && curl -fsSL --proto '=https' -o /usr/local/bin/talosctl "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" \
 && echo "${TALOSCTL_SHA256}  /usr/local/bin/talosctl" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/amd64/kubectl" \
 && echo "${KUBECTL_SHA256}  /usr/local/bin/kubectl" | sha256sum -c - \
 && curl -fsSL --proto '=https' -o /usr/local/bin/jq "https://github.com/jqlang/jq/releases/download/${JQ_VERSION}/jq-linux-amd64" \
 && echo "${JQ_SHA256}  /usr/local/bin/jq" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/talosctl /usr/local/bin/kubectl /usr/local/bin/jq \
 && rm /tmp/tools.env /tmp/jq.env \
 && addgroup -g 65532 mitstack \
 && adduser -D -H -h /tmp -s /sbin/nologin -G mitstack -u 65532 mitstack

USER 65532:65532
