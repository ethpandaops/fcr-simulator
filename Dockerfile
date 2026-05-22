# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS orchestrator-builder

ENV CGO_ENABLED=0
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/fcr-orchestrator ./cmd/fcr-orchestrator


FROM rust:1.88-bookworm AS lighthouse-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      clang \
      cmake \
      git \
      libclang-dev \
      pkg-config \
      protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN cd /tmp \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "git@github.com:" \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "ssh://git@github.com/"
RUN if [ ! -f engines/lighthouse/lighthouse/Cargo.toml ]; then \
      git submodule update --init --recursive engines/lighthouse/lighthouse; \
    fi
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    --mount=type=cache,target=/src/engines/lighthouse/lighthouse/target \
    bash engines/lighthouse/build.sh


FROM eclipse-temurin:21-jdk-noble AS teku-builder

ARG FCR_TEKU_COMMIT=c5825d53325cd67ab91b35cc544a7b660be317ff

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      git \
    && rm -rf /var/lib/apt/lists/*

ENV GRADLE_USER_HOME=/root/.gradle
WORKDIR /src
COPY . .
RUN cd /tmp \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "git@github.com:" \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "ssh://git@github.com/"
RUN if [ -f engines/teku/teku/settings.gradle ] \
      && ! git -C engines/teku/teku rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
      rm -rf engines/teku/teku; \
      mkdir -p engines/teku/teku; \
      git -C engines/teku/teku init; \
      git -C engines/teku/teku remote add origin https://github.com/Nashatyrev/teku.git; \
      git -C engines/teku/teku fetch --depth 1 origin "${FCR_TEKU_COMMIT}"; \
      git -C engines/teku/teku checkout --detach FETCH_HEAD; \
    elif [ ! -f engines/teku/teku/settings.gradle ]; then \
      git submodule update --init --recursive engines/teku/teku; \
    fi
RUN --mount=type=cache,target=/root/.gradle \
    bash engines/teku/build.sh


FROM node:24-bookworm-slim AS lodestar-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      git \
      make \
      python3 \
    && rm -rf /var/lib/apt/lists/*

ENV PNPM_HOME=/pnpm
ENV PATH=/pnpm:${PATH}
WORKDIR /src
COPY . .
ARG FCR_LODESTAR_COMMIT
ARG FCR_LODESTAR_DESCRIBE
ENV FCR_LODESTAR_COMMIT=${FCR_LODESTAR_COMMIT}
ENV FCR_LODESTAR_DESCRIBE=${FCR_LODESTAR_DESCRIBE}
RUN corepack enable && corepack prepare pnpm@10 --activate
RUN cd /tmp \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "git@github.com:" \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "ssh://git@github.com/"
RUN if [ ! -f engines/lodestar/lodestar/package.json ]; then \
      git submodule update --init --recursive engines/lodestar/lodestar; \
    fi
RUN --mount=type=cache,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store \
    && bash engines/lodestar/build.sh


FROM debian:bookworm AS nimbus-builder

ARG FCR_NIMBUS_COMMIT=6fb05f36804d53c2e8e014cfeeea8ad7996a5efe

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash \
      build-essential \
      ca-certificates \
      cmake \
      curl \
      file \
      git \
      git-lfs \
      pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN cd /tmp \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "git@github.com:" \
    && git config --file /root/.gitconfig url."https://github.com/".insteadOf "ssh://git@github.com/"
RUN if [ -f engines/nimbus/nimbus-eth2/Makefile ] \
      && ! git -C engines/nimbus/nimbus-eth2 rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
      rm -rf engines/nimbus/nimbus-eth2; \
      mkdir -p engines/nimbus/nimbus-eth2; \
      git -C engines/nimbus/nimbus-eth2 init; \
      git -C engines/nimbus/nimbus-eth2 remote add origin https://github.com/status-im/nimbus-eth2.git; \
      git -C engines/nimbus/nimbus-eth2 fetch --depth 1 origin "${FCR_NIMBUS_COMMIT}"; \
      git -C engines/nimbus/nimbus-eth2 checkout --detach FETCH_HEAD; \
    elif [ ! -f engines/nimbus/nimbus-eth2/Makefile ]; then \
      git submodule update --init --recursive engines/nimbus/nimbus-eth2; \
    fi
RUN bash engines/nimbus/build.sh


FROM rust:1.88-bookworm AS grandine-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      clang \
      cmake \
      git \
      libclang-dev \
      pkg-config \
      protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    bash engines/grandine/build.sh


FROM debian:bookworm-slim AS orchestrator

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=orchestrator-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator
ENTRYPOINT ["fcr-orchestrator"]


FROM debian:bookworm-slim AS lighthouse

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      libgcc-s1 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=orchestrator-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator
COPY --from=lighthouse-builder /src/results/fcr-lighthouse /usr/local/bin/fcr-lighthouse
ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-lighthouse
ENTRYPOINT ["fcr-orchestrator"]


FROM eclipse-temurin:21-jre-noble AS teku

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=orchestrator-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator
COPY --from=teku-builder /src/engines/teku/.build/dist/fcr-teku-all.jar /usr/local/lib/fcr-teku/fcr-teku-all.jar
RUN printf '%s\n' \
      '#!/usr/bin/env sh' \
      'exec java -jar /usr/local/lib/fcr-teku/fcr-teku-all.jar "$@"' \
      > /usr/local/bin/fcr-teku \
    && chmod +x /usr/local/bin/fcr-teku
ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-teku
ENTRYPOINT ["fcr-orchestrator"]


FROM node:24-bookworm-slim AS lodestar

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=orchestrator-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator
COPY --from=lodestar-builder /src/engines/lodestar /opt/fcr/lodestar
RUN printf '%s\n' \
      '#!/usr/bin/env sh' \
      'exec node --enable-source-maps --max-old-space-size="${FCR_LODESTAR_MAX_OLD_SPACE_SIZE:-12288}" /opt/fcr/lodestar/dist/main.mjs "$@"' \
      > /usr/local/bin/fcr-lodestar \
    && chmod +x /usr/local/bin/fcr-lodestar
ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-lodestar
ENTRYPOINT ["fcr-orchestrator"]


FROM debian:bookworm-slim AS nimbus

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=orchestrator-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator
COPY --from=nimbus-builder /src/results/fcr-nimbus /usr/local/bin/fcr-nimbus
ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-nimbus
ENTRYPOINT ["fcr-orchestrator"]


FROM debian:bookworm-slim AS grandine

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      libgcc-s1 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=orchestrator-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator
COPY --from=grandine-builder /src/results/fcr-grandine /usr/local/bin/fcr-grandine
ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-grandine
ENTRYPOINT ["fcr-orchestrator"]
