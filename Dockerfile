FROM rust:1.88-bookworm AS rust-builder

RUN apt-get update && apt-get install -y \
    cmake \
    clang \
    libclang-dev \
    protobuf-compiler \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY . .

RUN cd engines/lighthouse/lighthouse && git submodule update --init --recursive

WORKDIR /build/engines/lighthouse/lighthouse

RUN CARGO_NET_GIT_FETCH_WITH_CLI=true \
    cargo build -p fcr-simulator --features fake_crypto --release


FROM eclipse-temurin:21-jdk-noble AS teku-builder

RUN apt-get update && apt-get install -y git bash && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY . .

RUN git -c protocol.file.allow=always submodule update --init --recursive engines/teku/teku
RUN bash engines/teku/build.sh


FROM golang:1.24-bookworm AS go-builder

WORKDIR /build
COPY . .

RUN go build -o /out/fcr-orchestrator ./cmd/fcr-orchestrator


FROM eclipse-temurin:21-jre-noble

RUN apt-get update && \
    apt-get install -y ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=rust-builder /build/engines/lighthouse/lighthouse/target/release/fcr-lighthouse /usr/local/bin/fcr-lighthouse
COPY --from=teku-builder /build/engines/teku/.build/dist/fcr-teku-all.jar /usr/local/lib/fcr-teku-all.jar
COPY --from=go-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator

RUN printf '#!/usr/bin/env sh\nexec java -jar /usr/local/lib/fcr-teku-all.jar "$@"\n' > /usr/local/bin/fcr-teku && \
    chmod +x /usr/local/bin/fcr-teku

ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-lighthouse
ENTRYPOINT ["fcr-orchestrator"]
