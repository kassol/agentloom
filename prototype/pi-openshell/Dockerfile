FROM node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global --ignore-scripts "@earendil-works/pi-coding-agent@0.84.1" \
    && groupadd --system sandbox \
    && useradd --system --gid sandbox --home-dir /sandbox --shell /bin/bash sandbox \
    && mkdir -p /sandbox /opt/codex-loom \
    && chown sandbox:sandbox /sandbox

COPY internal/pi/loom_extension.ts /opt/codex-loom/loom_extension.ts
COPY prototype/pi-openshell/probe_extension.ts /opt/codex-loom/probe_extension.ts

WORKDIR /sandbox
USER sandbox:sandbox
ENTRYPOINT ["pi"]
