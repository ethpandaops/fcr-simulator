# FCR Simulator

Replays historical Ethereum mainnet blocks through CL clients' [Fast Confirmation Rule](https://github.com/ethereum/consensus-specs/pull/4747) implementations to measure what percentage of blocks would have fast-confirmed.

V1 ships with Lighthouse and Teku engines. The architecture is engine-agnostic; adding Lodestar / Nimbus / Prysm / Grandine is "implement the engine CLI contract".

## Architecture

A Go orchestrator owns everything engine-agnostic: ERA file download, checkpoint state fetch, attestation source plan, output schema. Engine binaries are subprocesses that talk to the orchestrator over a localhost beacon-API server.

```
┌──────────────────────────────────────────────────────────┐
│  fcr-orchestrator  (Go)                                  │
│  - CLI, chunking, subprocess spawn                       │
│  - Pre-download ERA files                                │
│  - Per-worker checkpoint state fetch                     │
│  - Localhost beacon-API HTTP server                      │
│  - JSONL → CSV merge + run manifest                      │
└──────────────────────────────────────────────────────────┘
                     ▲
                     │  HTTP (SSZ bodies, beacon-API)
                     │
       ┌─────────────┴─────────────┐
       │  fcr-lighthouse  (Rust)   │
       │  fcr-teku        (Java)   │
       └───────────────────────────┘
```

The orchestrator computes a **plan** mapping each sim slot to the block whose attestations should be injected. Engines fetch that plan once at startup, then for each sim slot they (a) fetch the slot's block via beacon-API, (b) fetch the source block, (c) extract attestations natively using their own fork-aware parser, and (d) inject into fork choice using `AttestationFromBlock::True` (or each client's equivalent).

For each sim slot the engine emits one JSONL record (slot, head root, confirmed root, fast-confirmed flag, attestations injected, etc.). The orchestrator validates every record, enriches with engine metadata, and writes the final user-facing JSONL + CSV.

Full architectural details in [`PLAN.md`](PLAN.md). Engine contract in [`docs/ENGINE_CONTRACT.md`](docs/ENGINE_CONTRACT.md). Output schema in [`docs/SCHEMA_V3.md`](docs/SCHEMA_V3.md). Attestation source planner spec in [`docs/TEST_SPEC_attplan.md`](docs/TEST_SPEC_attplan.md).

## How it works (per slot)

For each sim slot N:

1. Engine fetches block at slot N via `GET /eth/v2/beacon/blocks/{N}` (orchestrator serves from local ERA cache). 404 means missed slot.
2. If block exists, engine processes it through its production state-transition + fork-choice pipeline.
3. Engine looks up `plan[N].source_block_slot` — the orchestrator-chosen block whose attestations to inject.
4. Engine fetches the source block (if non-nil), extracts attestations, indexes them, and injects with `AttestationFromBlock::True`.
5. Engine calls `recompute_head_at_slot(N+1)` to trigger FCR evaluation.
6. Engine emits one JSONL record.

BLS verification is disabled (`fake_crypto`), as are execution-layer notification and blob/data-column availability checks — historical canonical-chain replay doesn't need them. The orchestrator refuses to run an engine that wasn't built with these bypasses (manifest enforcement).

## Attestation source modes

The orchestrator owns which block sources attestations for each sim slot. Two modes:

- **`next-non-missed`** (default): for sim slot N, scan forward N+1..N+cap, use the first non-missed block. `--lookahead-cap=4` reproduces current Lighthouse behavior exactly; `--lookahead-cap=32` includes late inclusions up to the spec maximum.
- **`strict-source-block-k-minus-1`**: for sim slot N, source is exactly N+1 if present, else nil (no attestations injected that slot).

The planner is a pure function; all boundary scenarios (missed slots, cap edges, overflow) are covered by table-driven tests in [`pkg/attplan/attplan_test.go`](pkg/attplan/attplan_test.go) and matched against the spec in [`docs/TEST_SPEC_attplan.md`](docs/TEST_SPEC_attplan.md).

## Build

```bash
git clone --recursive https://github.com/ethpandaops/fcr-simulator.git
cd fcr-simulator

# Lighthouse engine (~10-20 min cold, much faster on rebuild)
( cd lighthouse && CARGO_NET_GIT_FETCH_WITH_CLI=true cargo build -p fcr-simulator --features fake_crypto --release )
cp lighthouse/target/release/fcr-lighthouse ./results/fcr-lighthouse

# Teku engine (requires JDK 21; ~5 min cold, much faster on rebuild)
bash engines/teku/build.sh
# produces ./results/fcr-teku (a shim that exec's java -jar)

# Orchestrator (seconds)
go build -o ./results/fcr-orchestrator ./cmd/fcr-orchestrator
```

## Run

```bash
./results/fcr-orchestrator \
  --engine lighthouse \
  --engine-binary ./results/fcr-lighthouse \
  --network mainnet \
  --beacon-node-url http://your-beacon-node:5052 \
  --start-epoch 435000 \
  --end-epoch 435100 \
  --warmup-epochs 10 \
  --parallel 2 \
  --output results/results.csv \
  --output-format both \
  --attestation-source-mode next-non-missed \
  --lookahead-cap 4 \
  --cache-dir ~/.cache/fcr-simulator
```

Run Teku (same flags, different `--engine` and `--engine-binary`):

```bash
./results/fcr-orchestrator \
  --engine teku \
  --engine-binary ./results/fcr-teku \
  --network mainnet \
  --beacon-node-url http://your-beacon-node:5052 \
  --start-epoch 435000 \
  --end-epoch 435100 \
  --warmup-epochs 10 \
  --parallel 1 \
  --output results/results.csv \
  --output-format both \
  --attestation-source-mode next-non-missed \
  --lookahead-cap 4 \
  --cache-dir ~/.cache/fcr-simulator
```

For long-running chunked execution (resumable across crashes), use [`run.sh`](run.sh):

```bash
./run.sh --start-epoch 435000 --end-epoch 440000 --chunk-size 1000 \
         --beacon-node-url http://your-beacon-node:5052 --parallel 4
```

**Beacon node requirements**: must serve `/eth/v2/debug/beacon/states/{slot}` and `/eth/v1/beacon/headers/{slot}` for the warmup slots. Lighthouse requires `--reconstruct-historic-states`; Teku requires `--data-storage-mode=ARCHIVE`. Recent ranges (within ~256 epochs of head) usually work without archive mode because checkpoint states are retained.

Each worker uses ~2-3 GB RAM (Lighthouse `BeaconState` is big). `--parallel` should be at most `num_cpus / 4` due to memory bandwidth.

## Docker

```bash
docker run -v $(pwd)/results:/data ghcr.io/ethpandaops/fcr-simulator \
  --engine lighthouse \
  --network mainnet \
  --beacon-node-url http://your-beacon-node:5052 \
  --start-epoch 435000 --end-epoch 435100 \
  --warmup-epochs 10 --parallel 2 \
  --output /data/results.csv --output-format both \
  --cache-dir /data/cache
```

The image bundles both `fcr-orchestrator` and `fcr-lighthouse`; `FCR_ENGINE_BINARY=/usr/local/bin/fcr-lighthouse` is preset.

## Output

CSV starts with `# fcr-simulator-csv-schema-version:3` followed by the header row. JSONL is one record per line. Schema in [`docs/SCHEMA_V3.md`](docs/SCHEMA_V3.md). Key columns: `slot`, `epoch`, `has_block`, `block_root`, `confirmed_root`, `confirmed_slot`, `confirmation_delay_slots`, `fast_confirmed`, `strict_one_slot_confirmed`, `source_block_slot`, `num_attestations_injected`, `engine_name`, `engine_version`, `engine_commit`, `attestation_source_mode`, `lookahead_cap`.

The orchestrator also writes a sidecar `results.manifest.json` capturing the engine manifest, run config, ERA file hashes, and output hashes for reproducibility.

## Verifying parity with the old in-process simulator

The V1 architecture was validated against the current in-process Lighthouse simulator using `scripts/v1-parity-diff.sh`. On Phase 0 mainnet (epoch 0, 31 slots) the orchestrator + slim engine produce byte-equal output to the OLD simulator across all 13 consensus columns. Larger-range validation requires a state-archive-mode beacon node.

```bash
# Run new orchestrator + engine
./results/fcr-orchestrator [args] --output results/new.csv

# Run old in-process simulator on the same range (separate git worktree)
./lighthouse/target/release/fcr-simulator [args] --output results/old.csv

# Diff projected to consensus columns
./scripts/v1-parity-diff.sh results/old.csv results/new.csv
```

## References

- [Fast Confirmation Rule spec (consensus-specs#4747)](https://github.com/ethereum/consensus-specs/pull/4747)
- [Lighthouse FCR implementation (sigp/lighthouse#8951)](https://github.com/sigp/lighthouse/pull/8951)
- [Research paper (arXiv:2405.00549)](https://arxiv.org/abs/2405.00549)
- Engine contract: [`docs/ENGINE_CONTRACT.md`](docs/ENGINE_CONTRACT.md)
- Output schema: [`docs/SCHEMA_V3.md`](docs/SCHEMA_V3.md)
- Architecture decision log: [`PLAN.md`](PLAN.md)
