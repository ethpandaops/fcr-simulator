# FCR Simulator

Replays historical Ethereum mainnet blocks through each CL client's [Fast Confirmation Rule](https://github.com/ethereum/consensus-specs/pull/4747) implementation and reports per-slot FCR output for cross-client comparison.

## Engines

| Engine | Upstream pin | Build |
|---|---|---|
| lighthouse | submodule `lighthouse/` | `( cd lighthouse && CARGO_NET_GIT_FETCH_WITH_CLI=true cargo build -p fcr-simulator --features fake_crypto --release )` then `cp lighthouse/target/release/fcr-lighthouse results/fcr-lighthouse` |
| teku       | submodule `engines/teku/teku` (Nashatyrev/teku `confirmation-2`) | `bash engines/teku/build.sh` |
| nimbus     | submodule `engines/nimbus/nimbus-eth2` (status-im/nimbus-eth2 `unstable`) | `bash engines/nimbus/build.sh` |
| lodestar   | submodule `engines/lodestar/lodestar` (ChainSafe/lodestar PR #8837) | `bash engines/lodestar/build.sh` |
| grandine   | upstream `bomanaps/grandine` PR #656 via `engines/grandine/grandine-engine.patch` | `bash engines/grandine/build.sh` |

Prysm is deferred: PR #15164 implements an older spec (`adiasg/eth2.0-specs:3e3ef28`), not [consensus-specs#4747](https://github.com/ethereum/consensus-specs/pull/4747). Shipping a binary against the older algorithm would contaminate cross-engine comparison. Revisit once the upstream PR rebases.

## Architecture

A Go orchestrator owns everything engine-agnostic — ERA file download, checkpoint state fetch, attestation source plan, output schema, JSONL → CSV merge. Engines are subprocesses that speak HTTP to a localhost beacon-API the orchestrator serves.

```
┌─────────────────────────────────────────────────────────┐
│  fcr-orchestrator (Go)                                  │
└────────────────────────────────┬────────────────────────┘
                                 │ HTTP (SSZ blocks/states + /fcr-sim/v1/plan)
       ┌──────────┬──────────┬───┴───────┬──────────┐
   fcr-lighthouse fcr-teku  fcr-nimbus  fcr-lodestar fcr-grandine
       (Rust)    (Java)     (Nim)       (TypeScript) (Rust)
```

Per sim slot N the engine: fetches block N, imports it, looks up `plan[N].source_block_slot`, extracts that block's attestations, injects them using its native equivalent of Lighthouse's `AttestationFromBlock::True`, runs `recompute_head_at_slot(N+1)`, emits one JSONL record. The orchestrator validates each record, enriches with engine metadata, writes the final CSV / JSONL / manifest.

BLS, execution-layer, and blob/DA checks are bypassed in each engine — historical canonical-chain replay doesn't need them. The orchestrator hard-rejects any engine binary that doesn't declare `fake_crypto` in its `--manifest-json` `build_flags`.

Engine contract: [`ENGINE_CONTRACT.md`](ENGINE_CONTRACT.md). Output schema: [`SCHEMA.md`](SCHEMA.md). Attestation planner spec: [`pkg/attplan/SPEC.md`](pkg/attplan/SPEC.md).

## Attestation source modes

The orchestrator owns which block sources attestations for each sim slot:

- **`next-non-missed`** (default): for sim slot N, the first non-missed block in `N+1..N+lookahead-cap`. `--lookahead-cap=4` reproduces today's Lighthouse behavior; `--lookahead-cap=32` covers the spec's full inclusion range.
- **`strict-source-block-k-minus-1`**: source is exactly `N+1` if it exists, else nothing.

## Build

```bash
git clone --recursive https://github.com/ethpandaops/fcr-simulator.git
cd fcr-simulator

# Orchestrator (seconds)
go build -o ./results/fcr-orchestrator ./cmd/fcr-orchestrator

# Each engine (see Engines table above for the build command)
```

## Run

```bash
./results/fcr-orchestrator \
  --engine lighthouse \
  --engine-binary ./results/fcr-lighthouse \
  --network mainnet \
  --beacon-node-url http://your-beacon-node:5052 \
  --start-epoch 435000 --end-epoch 435100 \
  --warmup-epochs 10 --parallel 2 \
  --output results/results.csv --output-format both \
  --attestation-source-mode next-non-missed --lookahead-cap 4 \
  --cache-dir ~/.cache/fcr-simulator
```

Swap `--engine` and `--engine-binary` for any other built engine.

For long resumable runs, [`run.sh`](run.sh) chunks the range and merges on completion:

```bash
./run.sh --start-epoch 435000 --end-epoch 440000 --chunk-size 1000 \
         --beacon-node-url http://your-beacon-node:5052 --parallel 4
```

**Beacon node**: must serve `/eth/v2/debug/beacon/states/{slot}` and `/eth/v1/beacon/headers/{slot}` for the warmup slots. Lighthouse needs `--reconstruct-historic-states`; Teku needs `--data-storage-mode=ARCHIVE`. Recent ranges (within ~256 epochs of head) often work without archive mode.

Per-worker RAM: ~2–3 GB for the Rust engines; Lodestar settles around 4 GB after the bounded-cache fix. `--parallel` should be at most `num_cpus / 4`.

## Cross-client comparison

`scripts/cross-client/` contains:

- `pick-epochs.py` — deterministic 10-epoch mainnet sample (seed `20260514`, range `[435000, 445000)`).
- `manifest-check.sh` — fast contract validation across every `results/fcr-<engine>` binary.
- `run.sh <engine> <binary>` — run that engine over the sample, write per-epoch CSVs under `results/cross-client/<engine>/`.
- `_run-1hr.sh`, `_run-12hr.sh` — parallel 5-engine runs over a fixed window, write `results/<window>/<engine>/run.csv`.
- `diff.py` — per-slot cross-engine diff, emits `per-slot.csv` + `divergences.csv` + a per-column disagreement summary.

## Output

CSV starts with `# fcr-simulator-csv-schema-version:3` followed by the header row. JSONL is one record per line. Full schema in [`SCHEMA.md`](SCHEMA.md). Key columns: `slot`, `epoch`, `has_block`, `block_root`, `head_root`, `confirmed_root`, `confirmed_slot`, `confirmation_delay_slots`, `fast_confirmed`, `strict_one_slot_confirmed`, `source_block_slot`, `num_attestations_injected`, `engine_name`, `engine_version`, `engine_commit`.

A sidecar `<output>.manifest.json` captures engine manifest, run config, ERA file hashes, and output hashes for reproducibility.

## References

- [Fast Confirmation Rule spec (consensus-specs#4747)](https://github.com/ethereum/consensus-specs/pull/4747)
- [Research paper (arXiv:2405.00549)](https://arxiv.org/abs/2405.00549)
- Per-client FCR PRs: [Lighthouse #8951](https://github.com/sigp/lighthouse/pull/8951), [Teku confirmation-2](https://github.com/Nashatyrev/teku/tree/confirmation-2), [Nimbus (merged to unstable)](https://github.com/status-im/nimbus-eth2), [Lodestar #8837](https://github.com/ChainSafe/lodestar/pull/8837), [Grandine #656](https://github.com/grandinetech/grandine/pull/656), [Prysm #15164 (deferred)](https://github.com/OffchainLabs/prysm/pull/15164)
