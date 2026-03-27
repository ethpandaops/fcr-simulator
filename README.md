# FCR Simulator

Replays historical Ethereum mainnet blocks through [Lighthouse's](https://github.com/sigp/lighthouse) production [Fast Confirmation Rule](https://github.com/ethereum/consensus-specs/pull/4747) implementation to determine what percentage of blocks would have fast confirmed.

Uses Lighthouse's actual consensus code (state transitions, fork choice, FCR evaluation) with `fake_crypto` to skip BLS verification for speed. Blocks are sourced from [ERA files](https://github.com/eth-clients/e2store-format-specs), checkpoint states from a beacon node API.

## Lighthouse Modifications

This project uses a [fork of Lighthouse](https://github.com/samcm/lighthouse/tree/fcr-simulator) based on [dapplion's FCR branch](https://github.com/sigp/lighthouse/pull/8951). The modifications are minimal and auditable:

- [`fcr-simulator/`](https://github.com/samcm/lighthouse/tree/fcr-simulator/fcr-simulator) - the simulator crate (new)
- [`data_availability_checker.rs`](https://github.com/samcm/lighthouse/compare/fcr...fcr-simulator#diff-data_availability_checker) - `new_without_da_check()` to bypass blob availability for historical blocks
- [`block_verification_types.rs`](https://github.com/samcm/lighthouse/compare/fcr...fcr-simulator#diff-block_verification_types) - `from_available_block()` constructor for RangeSyncBlock
- [`Cargo.toml`](https://github.com/samcm/lighthouse/compare/fcr...fcr-simulator#diff-Cargo.toml) - adds `fcr-simulator` to workspace members

[Full diff of all Lighthouse changes](https://github.com/samcm/lighthouse/compare/fcr...fcr-simulator)

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

ERA files are downloaded automatically from `https://mainnet.era.nimbus.team` and cached locally. The beacon node is only used to fetch the starting checkpoint state (once per worker, cached).

Each worker uses ~2-3GB RAM. `--parallel` should be at most `num_cpus / 4` due to memory bandwidth constraints.

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

CSV with one row per slot:

| Column | Description |
|--------|-------------|
| `slot` | Beacon chain slot number |
| `epoch` | Epoch number |
| `has_block` | Whether a block was proposed at this slot |
| `block_root` | Head block root |
| `confirmed` | Whether FCR confirmed within 1 slot (delay <= 1) |
| `confirmed_root` | The FCR confirmed block root |
| `confirmed_slot` | Slot of the confirmed block |
| `confirmation_delay_slots` | Slots between current slot and confirmed slot |
| `head_root` | Fork choice head root |
| `finalized_epoch` | Current finalized epoch |
| `justified_epoch` | Current justified epoch |
| `num_attestations_injected` | Peek-ahead attestations injected |
| `is_epoch_boundary` | Whether this slot starts an epoch |
| `is_missed_slot` | Whether the proposer missed this slot |

## Methodology

### Simulation Loop

For each slot N:

1. Process the block at slot N through Lighthouse's full block processing pipeline (state transition, fork choice `on_block`, attestation processing)
2. Recompute the fork choice head
3. Scan forward to find the next block that contains attestations for slot N (up to 32 slots ahead)
4. Inject only those attestations targeting slot N into the fork choice store
5. Advance the slot clock and recompute head, which triggers the FCR evaluation
6. Record the FCR result: confirmed root, confirmation delay, and per-slot metadata

### Attestation Model

The simulator uses an **optimistic attestation arrival model**: for each slot N, we extract the aggregate attestations that were eventually included on-chain and simulate them as if they all arrived at the attestation deadline (4 seconds into slot N+1).

Concretely, attestations for slot N are sourced from the first subsequent block that includes them. In the normal case this is block N+1. If slot N+1 is missed, we scan forward through N+2, N+3, etc. (up to the 32-slot inclusion window) until we find a block containing attestations with `attestation.data.slot == N`.

This means:

- **We only use attestations that were actually included on-chain.** We do not fabricate attestations or assume participation rates. The attestation data reflects what validators actually produced and propagated.
- **We assume timely arrival.** In reality, some attestations arrive late and miss the FCR evaluation window. Our model assumes they all arrive on time, which makes our results an upper bound on confirmation rates.
- **We use the first block containing each slot's attestations.** Attestations for a given slot can be split across multiple blocks (e.g., partial aggregates in N+1 and N+2). We only inject from the first block found. This means we slightly under-count attestation weight in some cases, which makes our results a conservative estimate.
- **Missed slots are handled correctly.** When slot N has no block, the committee at slot N still attests (to the parent block). These attestations land in the next available block and are injected when we simulate slot N.

### What FCR Sees

The FCR evaluation uses Lighthouse's production `FastConfirmationRule` code from [PR #8951](https://github.com/sigp/lighthouse/pull/8951). It receives the same inputs it would in a live node:

- The proto-array fork choice store with all processed blocks and their weights
- The materialized vote trackers (latest messages from all validators)
- The beacon state for committee assignments and balance information
- Justified and finalized checkpoints

The only difference from a live node is the attestation timing: a live node sees attestations arrive in real-time via gossip, while we batch-inject them from the historical record.

### Known Limitations

1. **Optimistic bias from timely arrival assumption.** Real nodes may not see all attestations before the FCR evaluation point. Our confirmation rates are an upper bound.
2. **Conservative bias from single-block attestation sourcing.** Late attestations included in blocks N+2, N+3, etc. are not captured. This partially offsets limitation #1.
3. **No equivocation simulation.** The simulator does not model equivocating validators. Equivocations on mainnet are extremely rare and would not materially affect results.
4. **Byzantine threshold is configurable.** Default is 25% (the spec maximum). Lower thresholds confirm faster but provide weaker safety guarantees.

### Correctness Guarantees

- **State transitions** use Lighthouse's production `per_slot_processing` and `per_block_processing` code.
- **Fork choice** uses Lighthouse's production `proto_array` implementation.
- **FCR evaluation** uses Lighthouse's production `FastConfirmationRule` from the `fcr` branch.
- **BLS verification** is disabled via `fake_crypto` for performance. This does not affect FCR results since the FCR algorithm operates on attestation weights and fork choice scores, not signature validity. All blocks being replayed are from the canonical chain and have already been verified by the network.
- **Blob availability** checks are bypassed for historical blocks. This does not affect fork choice or FCR, which only depend on the beacon block contents (attestations, state roots, parent roots).

## References

- [Fast Confirmation Rule spec](https://github.com/ethereum/consensus-specs/pull/4747)
- [Lighthouse FCR implementation](https://github.com/sigp/lighthouse/pull/8951)
- [Research paper (arXiv:2405.00549)](https://arxiv.org/abs/2405.00549)
- [fastconfirm.it](https://fastconfirm.it)
