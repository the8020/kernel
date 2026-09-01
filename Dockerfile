# syntax=docker/dockerfile:1
# The running container needs `--security-opt seccomp=unconfined` because its
# rootless service and development sandboxes launch nested gVisor processes.

FROM debian:trixie-slim AS builder

ARG DEBIAN_FRONTEND=noninteractive
ENV CGO_ENABLED=0 \
    KERNEL_NETWORK_MAIN_PORT=80 \
    KERNEL_NETWORK_SSH_PORT=22 \
    KERNEL_SANDBOX_RUNTIME_MODE=rootless \
    THE8020_OUTER_CONTAINER_BUILD=true \
    THE8020_SKIP_RUNTIME_HOST=true

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        bash \
        bzip2 \
        ca-certificates \
        curl \
        git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /usr/local/src/the8020
COPY . .

# run.sh performs the normal default installation, synchronizes the indexed
# packages, builds and bundles the service runtime, and builds the development
# sandbox image. Scripted `exit` closes only the build-time admin console; its
# wrapper then shuts the build-time kernel down cleanly.
RUN install -d -m 0755 /8020 \
    && cd /8020 \
    && printf 'exit\n' | /usr/local/src/the8020/run.sh \
    && rm -rf \
        /8020/node/kernel/runtime/downloads \
        /8020/node/kernel/runtime/gvisor \
        /8020/node/kernel/runtime/tmp \
        /8020/node/kernel/runtime/verification-deno-cache

FROM debian:trixie-slim

ARG DEBIAN_FRONTEND=noninteractive
ENV KERNEL_NETWORK_MAIN_PORT=80 \
    KERNEL_NETWORK_SSH_PORT=22 \
    KERNEL_SANDBOX_RUNTIME_MODE=rootless

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        bash \
        ca-certificates \
        git \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/src/the8020/.development/bin/kernel /usr/local/bin/kernel
COPY --from=builder /usr/local/src/the8020/.development/bin/admin /usr/local/bin/admin
COPY --from=builder /usr/local/src/the8020/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY --from=builder /usr/local/src/the8020/defaults/config/runtime/smoke-portable.sh /usr/local/lib/the8020/smoke-portable.sh
COPY --from=builder /8020 /8020

WORKDIR /8020
VOLUME ["/8020"]

EXPOSE 80/tcp 22/tcp
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/admin", "--root", "/8020", "system", "status"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve"]
