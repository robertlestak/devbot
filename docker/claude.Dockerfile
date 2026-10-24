# The claude agent image, from a published devbot base image, for your own
# registry: Claude Code is (c) Anthropic PBC under Anthropic's Commercial
# Terms, so devbot publishes no image with it (docs/operating.md#agent-images).
# The same as docker/Dockerfile's claude target, without building devbot:
#
#   docker buildx build -f docker/claude.Dockerfile \
#     --build-arg BASE=docker.io/robertlestak/devbot:<version> \
#     --build-arg CLAUDE_VERSION=<version> \
#     -t <your-registry>/devbot-claude:<tag> --push .

ARG DEBIAN_IMAGE=debian:13-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
ARG BASE=docker.io/robertlestak/devbot:latest
ARG CLAUDE_VERSION=2.1.303

FROM ${DEBIAN_IMAGE} AS fetch-claude
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/*
COPY docker/fetch docker/fetch-claude /usr/local/bin/
# Set by buildx per platform; a default here would override it.
ARG TARGETARCH CLAUDE_VERSION
RUN fetch-claude "$CLAUDE_VERSION" "${TARGETARCH:-amd64}" /claude && chmod 755 /claude

FROM ${BASE}
COPY --from=fetch-claude /claude /usr/local/bin/claude
# A run never downloads an update it wouldn't use.
ENV DISABLE_AUTOUPDATER=1
RUN claude --version
ARG CLAUDE_VERSION
LABEL org.opencontainers.image.title="devbot-claude" \
      org.opencontainers.image.description="devbot with Claude Code ${CLAUDE_VERSION}, the agent sidecar for harness claude. Claude Code is (c) Anthropic PBC, used under Anthropic's Commercial Terms (https://www.anthropic.com/legal/commercial-terms); don't redistribute this image." \
      org.opencontainers.image.licenses="MIT AND LicenseRef-Anthropic-Commercial-Terms"
