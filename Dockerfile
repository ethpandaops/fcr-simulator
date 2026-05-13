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

RUN cd lighthouse && git submodule update --init --recursive

WORKDIR /build/lighthouse

RUN CARGO_NET_GIT_FETCH_WITH_CLI=true \
    cargo build -p fcr-simulator --features fake_crypto --release


FROM golang:1.24-bookworm AS go-builder

WORKDIR /build
COPY . .

RUN go build -o /out/fcr-orchestrator ./cmd/fcr-orchestrator


FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=rust-builder /build/lighthouse/target/release/fcr-lighthouse /usr/local/bin/fcr-lighthouse
COPY --from=go-builder /out/fcr-orchestrator /usr/local/bin/fcr-orchestrator

ENV FCR_ENGINE_BINARY=/usr/local/bin/fcr-lighthouse
ENTRYPOINT ["fcr-orchestrator"]
