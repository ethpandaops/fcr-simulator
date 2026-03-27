FROM rust:1.88-bookworm AS builder

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

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/lighthouse/target/release/fcr-simulator /usr/local/bin/

ENTRYPOINT ["fcr-simulator"]
