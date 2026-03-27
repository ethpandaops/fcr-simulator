# FCR Simulator

Replays historical Ethereum mainnet blocks through [Lighthouse's](https://github.com/sigp/lighthouse) production [Fast Confirmation Rule](https://github.com/ethereum/consensus-specs/pull/4747) implementation to determine what percentage of blocks would have fast confirmed.

## How It Works

For each slot N in the configured range:

1. Process block N through Lighthouse's production block processing pipeline (state transition, fork choice, attestation processing via `on_block`)
2. Find the next block (N+1, or N+2/N+3 if there are missed slots)
3. Inject that block's attestations into fork choice, simulating them arriving during slot N
4. Recompute the fork choice head, triggering FCR evaluation
5. Record the result

Blocks come from [ERA files](https://github.com/eth-clients/e2store-format-specs). The starting beacon state is fetched from a beacon node API and cached locally. BLS verification is disabled (`fake_crypto`) for speed since all blocks are from the canonical chain.

The attestation model is optimistic: we assume attestations from the next block all arrived at the attestation deadline. [This investigation](https://investigations.ethpandaops.io/2026-03/fcr-attestation-proxy/) validates that on-chain attestations are a reasonable proxy for what a node would see at the attestation deadline.

## Lighthouse Modifications

Uses a [fork of Lighthouse](https://github.com/samcm/lighthouse/tree/fcr-simulator) based on [dapplion's FCR branch](https://github.com/sigp/lighthouse/pull/8951). [Full diff of changes](https://github.com/dapplion/lighthouse/compare/fcr...samcm:lighthouse:fcr-simulator) -- adds the simulator crate and bypasses blob availability checks for historical blocks.

## Build

```bash
git clone --recursive https://github.com/ethpandaops/fcr-simulator.git
cd fcr-simulator/lighthouse
CARGO_NET_GIT_FETCH_WITH_CLI=true cargo build -p fcr-simulator --features fake_crypto --release
```

## Run

```bash
./target/release/fcr-simulator \
  --beacon-node-url http://your-beacon-node:5052 \
  --start-epoch 435000 \
  --end-epoch 435100 \
  --output results.csv \
  --cache-dir ~/.cache/fcr-simulator \
  --parallel 2
```

Each worker uses ~2-3GB RAM. `--parallel` should be at most `num_cpus / 4` due to memory bandwidth.

## Docker

```bash
docker run ghcr.io/ethpandaops/fcr-simulator \
  --beacon-node-url http://your-beacon-node:5052 \
  --start-epoch 435000 \
  --end-epoch 435100 \
  --output /data/results.csv \
  --cache-dir /data/cache \
  --parallel 2
```

## Output

CSV with one row per slot. Key columns: `slot`, `confirmed` (delay <= 1 slot), `confirmation_delay_slots`, `has_block`, `is_missed_slot`, `num_attestations_injected`.

## References

- [Fast Confirmation Rule spec](https://github.com/ethereum/consensus-specs/pull/4747)
- [Lighthouse FCR implementation](https://github.com/sigp/lighthouse/pull/8951)
- [Research paper (arXiv:2405.00549)](https://arxiv.org/abs/2405.00549)
