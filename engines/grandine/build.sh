#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
GRANDINE_DIR="$SCRIPT_DIR/grandine"
PATCH_FILE="$SCRIPT_DIR/grandine-engine.patch"
PINNED_SHA="9905f46fed0a5393ad13ee4294316e0a4975e795"

if [ ! -d "$GRANDINE_DIR" ]; then
  echo "cloning grandine at PR #656 HEAD into $GRANDINE_DIR" >&2
  git clone https://github.com/grandinetech/grandine.git "$GRANDINE_DIR"
  git -C "$GRANDINE_DIR" fetch --depth 1 origin "pull/656/head:pr-656"
  git -C "$GRANDINE_DIR" checkout "$PINNED_SHA"
fi

cd "$GRANDINE_DIR"

if [ "$(git rev-parse HEAD)" = "$PINNED_SHA" ]; then
  echo "applying engine patch on top of $PINNED_SHA" >&2
  git apply --check "$PATCH_FILE"
  git apply "$PATCH_FILE"
  git -c user.email=fcr-sim@ethpandaops.io -c user.name="fcr-simulator" \
    commit -am "fcr-simulator engine patch (do not push upstream)" >/dev/null
fi

git submodule update --init --recursive

GRANDINE_GIT_SHA="$PINNED_SHA" \
CARGO_NET_GIT_FETCH_WITH_CLI=true \
cargo build -p fcr-grandine --release

mkdir -p "$REPO_ROOT/results"
cp target/release/fcr-grandine "$REPO_ROOT/results/fcr-grandine"
echo "Built $REPO_ROOT/results/fcr-grandine"
