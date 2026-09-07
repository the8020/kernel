# syntax=docker/dockerfile:1
# Build the checked-out kernel release with its compatible packages.

FROM debian:trixie-slim AS builder

ARG DEBIAN_FRONTEND=noninteractive
ENV CGO_ENABLED=0 \
    THE8020_NETWORK_MAIN_PORT=80 \
    THE8020_NETWORK_SSH_PORT=22 \
    THE8020_SANDBOX_RUNTIME_MODE=rootless \
    THE8020_OUTER_CONTAINER_BUILD=true \
    THE8020_SKIP_RUNTIME_HOST=true

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        bash bzip2 ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
COPY . /usr/local/src/the8020/
WORKDIR /usr/local/src/the8020

RUN kernel_tag=$(git describe --tags --exact-match HEAD) \
    && kernel_version=${kernel_tag#v} \
    && if [[ ! "$kernel_version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then \
      echo "check out a major.minor.patch kernel release before building" >&2; \
      exit 1; \
    fi \
    && release_line=${kernel_version%.*} \
    && install -d -m 0755 /8020 /usr/local/share/the8020 \
    && printf 'release_line=%s\nkernel_tag=%s\nkernel_commit=%s\n' \
      "$release_line" "$kernel_tag" "$(git rev-parse --verify HEAD)" \
      > /usr/local/share/the8020/release \
    && cd /8020 \
    && export THE8020_RELEASE_VERSION="$release_line" \
    && printf 'exit\n' | /usr/local/src/the8020/run.sh \
    && rm -rf \
        /8020/node/kernel/runtime/downloads \
        /8020/node/kernel/runtime/gvisor \
        /8020/node/kernel/runtime/tmp \
        /8020/node/kernel/runtime/verification-deno-cache

FROM debian:trixie-slim

ARG DEBIAN_FRONTEND=noninteractive
ENV THE8020_NETWORK_MAIN_PORT=80 \
    THE8020_NETWORK_SSH_PORT=22 \
    THE8020_SANDBOX_RUNTIME_MODE=rootless

LABEL org.opencontainers.image.source="https://github.com/the8020/kernel"

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        bash ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/src/the8020/.development/bin/ /usr/local/bin/
COPY --from=builder /usr/local/src/the8020/docker/rootfs/ /
COPY --from=builder /usr/local/share/the8020/ /usr/local/share/the8020/
COPY --from=builder /8020/ /8020/

WORKDIR /8020
VOLUME ["/8020"]
EXPOSE 80/tcp 22/tcp
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/admin", "--root", "/8020", "kernel.status"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve"]
