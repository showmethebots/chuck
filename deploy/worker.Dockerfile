# Worker image: one Claude Code headless task per ephemeral Fly Machine.
# CI only builds this image; nothing runs it until chuck-worker lands
# (docs/superpowers/specs/2026-11-03-delivery-loop-design.md, "Workers").

FROM golang:1.27-trixie AS go

FROM node:24-trixie-slim
ARG CLAUDE_CODE_VERSION=2.1.296
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates openssh-client \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && npm cache clean --force
# Claude Code will not skip permission prompts as root.
RUN useradd --create-home --shell /bin/bash worker
USER worker
WORKDIR /home/worker
ENTRYPOINT ["claude"]
