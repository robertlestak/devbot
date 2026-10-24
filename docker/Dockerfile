# devbot's images, one build target each:
#
#   base    devbot, git, mise and the runtime packages, with no agent CLI:
#           serve, the job harness and the model proxy run it
#   codex   base + the Codex CLI, the agent sidecar of a role on codex
#   claude  base + Claude Code, the agent sidecar of a role on claude. Claude
#           Code is Anthropic's, under its Commercial Terms, so this image
#           is for your own registry, not a public one (docs/operating.md#agent-images)
#
# `docker build` with no --target builds base. docker/claude.Dockerfile
# builds the claude image from a published base instead.
#
# Deliberately thin: no language toolchains, no libraries. The agent
# installs what each repo needs at run time - languages through mise, system
# packages and services through apt - so nothing here goes stale and no repo is
# turned away for a missing toolchain.
#
# Every download is checked against a SHA-256 its publisher puts out:
# pinned below for mise and Codex, Anthropic's release manifest for Claude
# Code. hack/image-versions bumps the pins.

# By digest, so a rebuild is the same image.
ARG GO_IMAGE=golang:1.26.9-trixie@sha256:f06b7122f888680048afb247c4e5f49c0069eb6c4c1ccae0f3f4834527a8d22c
ARG DEBIAN_IMAGE=debian:13-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

# Pinned so a rebuild is the same image. Bump deliberately.
ARG CLAUDE_VERSION=2.1.303
ARG CODEX_VERSION=0.163.0
ARG CODEX_SHA256_AMD64=582646c6bf0561eb0de540f7fc85b9121b0091a826bc6e96830b7f1722122182
ARG CODEX_SHA256_ARM64=9d8ecf2d19d3e184877983902d6c85de48b3888dcb72c382a8a0c975845927fb
ARG MISE_VERSION=v2026.10.14
ARG MISE_SHA256_AMD64=50be7185bcf75929f01cc5630276bcb43ca367f7c5b176e2580903d28c611dc4
ARG MISE_SHA256_ARM64=a9a8eec284d1c8e1a76685dcfa2fd609e76773fe56546eb72e4568dd1f50c1c3

# The images' OCI labels.
ARG VERSION=dev
ARG REVISION=""
ARG SOURCE=https://github.com/robertlestak/devbot

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/devbot ./cmd/devbot

# Downloads happen here, so jq and the scripts stay out of the images. Each
# fetch-* stage takes TARGETARCH, which buildx sets per platform; a default
# would override it.
FROM ${DEBIAN_IMAGE} AS fetch
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/*
COPY docker/fetch docker/fetch-claude /usr/local/bin/

FROM fetch AS fetch-mise
ARG TARGETARCH MISE_VERSION MISE_SHA256_AMD64 MISE_SHA256_ARM64
RUN arch=$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo x64) \
    && sum=$([ "$TARGETARCH" = "arm64" ] && echo "$MISE_SHA256_ARM64" || echo "$MISE_SHA256_AMD64") \
    && fetch "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-linux-${arch}" "$sum" /mise \
    && chmod 755 /mise

# Codex's release package carries the code-mode host and ripgrep the CLI
# looks for beside its binary.
FROM fetch AS fetch-codex
ARG TARGETARCH CODEX_VERSION CODEX_SHA256_AMD64 CODEX_SHA256_ARM64
RUN arch=$([ "$TARGETARCH" = "arm64" ] && echo aarch64 || echo x86_64) \
    && sum=$([ "$TARGETARCH" = "arm64" ] && echo "$CODEX_SHA256_ARM64" || echo "$CODEX_SHA256_AMD64") \
    && fetch "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-package-${arch}-unknown-linux-musl.tar.gz" "$sum" /codex.tar.gz \
    && mkdir -p /opt/codex \
    && tar -xzf /codex.tar.gz -C /opt/codex \
    && chmod -R a+rX /opt/codex

FROM fetch AS fetch-claude
ARG TARGETARCH CLAUDE_VERSION
RUN fetch-claude "$CLAUDE_VERSION" "${TARGETARCH:-amd64}" /claude && chmod 755 /claude

FROM ${DEBIAN_IMAGE} AS runtime

# git for the harness; curl/ca-certificates/gnupg/xz for whatever the agent
# installs; procps so the model can see what it started. apt lists are
# removed - the agent runs `apt-get update` itself when it needs a package.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg xz-utils unzip procps \
    && rm -rf /var/lib/apt/lists/*

# The harness owns the shared workspace and runs unprivileged; the agent
# runs as root, and git there must still work in the repo the harness
# handed over. The harness's own git reads no system config.
RUN git config --system --add safe.directory '*'

COPY --from=fetch-mise /mise /usr/local/bin/mise
COPY --from=build /out/devbot /usr/local/bin/devbot

# mise's shims first, so tools the agent installs with `mise use -g` are on
# PATH for the checks too. IS_SANDBOX lets claude skip permission prompts as
# root, which is safe here only because the pod is the sandbox.
ENV HOME=/root \
    PATH=/root/.local/share/mise/shims:/root/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    IS_SANDBOX=1 \
    DEBIAN_FRONTEND=noninteractive \
    MISE_YES=1 \
    MISE_HIDE_UPDATE_WARNING=1

ARG VERSION REVISION SOURCE
LABEL org.opencontainers.image.title="devbot" \
      org.opencontainers.image.description="devbot: serve, the job harness and the model proxy, with no agent CLI" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="MIT"

ENTRYPOINT ["devbot"]

FROM runtime AS codex
COPY --from=fetch-codex /opt/codex /opt/codex
RUN ln -s /opt/codex/bin/codex /usr/local/bin/codex && codex --version
ARG CODEX_VERSION
LABEL org.opencontainers.image.title="devbot-codex" \
      org.opencontainers.image.description="devbot with the Codex CLI ${CODEX_VERSION} (Apache-2.0), the agent sidecar for harness codex" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0"

FROM runtime AS claude
COPY --from=fetch-claude /claude /usr/local/bin/claude
# A run never downloads an update it wouldn't use.
ENV DISABLE_AUTOUPDATER=1
RUN claude --version
ARG CLAUDE_VERSION
LABEL org.opencontainers.image.title="devbot-claude" \
      org.opencontainers.image.description="devbot with Claude Code ${CLAUDE_VERSION}, the agent sidecar for harness claude. Claude Code is (c) Anthropic PBC, used under Anthropic's Commercial Terms (https://www.anthropic.com/legal/commercial-terms); don't redistribute this image." \
      org.opencontainers.image.licenses="MIT AND LicenseRef-Anthropic-Commercial-Terms"

# Last, so a build with no --target is base.
FROM runtime AS base
