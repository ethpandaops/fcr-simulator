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

## How It Works

For each slot N:

1. Process the block at slot N through Lighthouse's full block processing pipeline (state transition + fork choice)
2. Scan forward to find the next block containing attestations for slot N
3. Inject those attestations into fork choice (simulating them arriving during slot N)
4. Trigger FCR evaluation
5. Record whether the head is confirmed and the confirmation delay

This is an **optimistic** simulation: it assumes all attestations that were eventually included arrived during their target slot. Real-world confirmation rates may be slightly lower due to late attestations.

## References

- [Fast Confirmation Rule spec](https://github.com/ethereum/consensus-specs/pull/4747)
- [Lighthouse FCR implementation](https://github.com/sigp/lighthouse/pull/8951)
- [Research paper (arXiv:2405.00549)](https://arxiv.org/abs/2405.00549)
- [fastconfirm.it](https://fastconfirm.it)
