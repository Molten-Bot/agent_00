FROM golang:1.26.5-trixie AS build
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG GIT_CHANGES_BY_DAY_VERSION=v0.0.0-20260518234615-87ad8a8d0d77

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOBIN=/out \
    go install github.com/moltenbot000/git-changes-by-day@${GIT_CHANGES_BY_DAY_VERSION}

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/harness ./cmd/harness

FROM node:26.5.0-trixie-slim AS runtime
ARG CODEX_VERSION=0.147.0
ARG CLAUDE_CODE_VERSION=2.1.224
ARG RAILSMITH_VERSION=0.1.2
ARG PLAYWRIGHT_VERSION=1.62.1
ARG OPENAI_PYTHON_VERSION=2.53.0
ENV GIT_TERMINAL_PROMPT=0 \
    HARNESS_AGENT_HARNESS="" \
    HARNESS_AGENT_COMMAND="" \
    HARNESS_AGENTS_SEED_PATH=/opt/moltenhub/library/AGENTS.md \
    HARNESS_WORKSPACE_RAM_BASE=/workspace \
    HARNESS_WORKSPACE_DISK_BASE=/workspace \
    HOME=/workspace/config/home \
    NODE_PATH=/usr/local/lib/node_modules \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright \
    PLAYWRIGHT_SKIP_BROWSER_GC=1 \
    PATH="/usr/local/go/bin:${PATH}"

RUN export DEBIAN_FRONTEND=noninteractive \
    && apt-get update \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        file \
        git \
        gh \
        jq \
        openssh-client \
        python3-pip \
        python3-venv \
        python3 \
        ripgrep \
    && ln -sf /usr/bin/python3 /usr/local/bin/python \
    && ln -sf /usr/bin/pip3 /usr/local/bin/pip \
    && python3 -m pip install --no-cache-dir --break-system-packages --root-user-action=ignore --upgrade "openai==${OPENAI_PYTHON_VERSION}" \
    && npm install --global --no-audit --no-fund \
      "@openai/codex@${CODEX_VERSION}" \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
      "@moltenbot/railsmith@${RAILSMITH_VERSION}" \
      "playwright@${PLAYWRIGHT_VERSION}" \
      "@playwright/test@${PLAYWRIGHT_VERSION}" \
    && playwright install --with-deps --no-shell chromium \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/* /tmp/* \
    && mkdir -p /workspace/config/home /workspace/agent_00/tasks \
    && chown -R node:node /workspace /opt/ms-playwright
WORKDIR /workspace

COPY --from=build --chmod=755 /out/harness /usr/local/bin/harness
COPY --from=build --chmod=755 /out/git-changes-by-day /usr/local/bin/git-changes-by-day
COPY --from=build /usr/local/go /usr/local/go
COPY library /opt/moltenhub/library
COPY skills /opt/moltenhub/skills
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint
COPY --chmod=755 docker/with-config.sh /usr/local/bin/with-config
RUN rm -rf /usr/local/go/api /usr/local/go/doc /usr/local/go/misc /usr/local/go/test \
    && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
    && ln -s /opt/moltenhub/library /workspace/library \
    && ln -s /opt/moltenhub/skills /workspace/skills

VOLUME ["/workspace/config"]

USER node

ENTRYPOINT ["/usr/local/bin/entrypoint"]
CMD ["/usr/local/bin/with-config"]
