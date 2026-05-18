# ENGINE CONTRACT — orchestrator ↔ engine

This document is the authoritative contract between `fcr-orchestrator` and each engine binary (`fcr-lighthouse`, future `fcr-teku`, etc.). Engines that violate this contract introduce harness bugs that contaminate cross-engine comparison.

## Engine binary CLI

```
fcr-<engine> [flags]

Flags:
  --beacon-node-url URL          (required) URL of orchestrator's HTTP server.
                                  Engine fetches blocks, states, genesis, and the
                                  attestation source plan from here.

  --start-slot N                  (required) First sim_slot to record. Inclusive.
  --end-slot N                    (required) Sim slot exclusive upper bound. Engine
                                  runs the slot loop from `warmup-start-slot+1` to
                                  `end-slot - 1`. Records output only for slots in
                                  `[start-slot, end-slot)`.

  --warmup-start-slot N           (required) Slot at which the engine bootstraps
                                  from the checkpoint state. Must be <= start-slot.
                                  Engine fetches state at this slot from the
                                  orchestrator and processes blocks from there.

  --network NAME                  (required) Network identifier. V1 supports
                                  "mainnet". Engine validates and configures spec.

  --byzantine-threshold N         (default 25) FCR byzantine threshold in percent.

  --attestation-source-mode MODE  (required) Either "next-non-missed" or
                                  "strict-source-block-k-minus-1". Used only for
                                  metadata in the output JSONL — the orchestrator's
                                  plan is the source of truth.

  --lookahead-cap N               (required) Cap value used by orchestrator. Used
                                  only for metadata.

  --output PATH                   (required) Path to write intermediate JSONL.

  --manifest-json                 (special) Print a JSON manifest to stdout and
                                  exit 0 immediately. No simulation runs. Output:
                                    {
                                      "engine_name": "lighthouse",
                                      "engine_version": "5.4.0",
                                      "engine_commit": "abc123...",
                                      "build_flags": ["fake_crypto"],
                                      "fcr_spec_commit": "..."   (optional, "" if unknown)
                                    }
                                  Orchestrator calls this before spawning the real
                                  simulation to capture engine metadata.
```

## Slot range semantics (CRITICAL — off-by-ones break parity)

Engine's slot loop must match current Lighthouse's `engine.rs:200`:

```rust
let mut slot = warmup_start_slot + 1;
while slot < end_slot {
    let is_recording = slot >= start_slot;
    // process block, inject attestations, recompute head
    if is_recording { write_jsonl_record(slot); }
    slot += 1;
}
```

**Slot loop**: `warmup_start_slot + 1` (inclusive) to `end_slot` (exclusive).
**Recording**: only slots where `slot >= start_slot` produce JSONL records.

Warmup slots are processed for fork-choice state but never recorded.

## Block fetching

Engine fetches blocks lazily from the orchestrator:

- `GET {beacon-node-url}/eth/v2/beacon/blocks/{slot}` with `Accept: application/octet-stream`
- 200 OK with SSZ-encoded `SignedBeaconBlock` and `Eth-Consensus-Version` header → process normally
- 404 Not Found → treat slot as missed (no block to process; `has_block=false`)
- 5xx → fatal error, exit non-zero with error to stderr

Engine MAY also fetch blocks by root: `GET .../eth/v2/beacon/blocks/{0x...}`. Required for checkpoint block lookup.

## State fetching

Engine fetches the checkpoint state at startup:

- `GET .../eth/v2/debug/beacon/states/{warmup_start_slot}` with `Accept: application/octet-stream`
- If 404 (checkpoint slot is missed), engine MAY retry with `state_id=genesis` for special cases, OR the orchestrator should be configured to never request a missed slot. Engine prefers explicit error over silent fallback.

Engine also fetches the checkpoint block (by root derived from state):
- `GET .../eth/v2/beacon/blocks/{0x...root}` with `Accept: application/octet-stream`

And the genesis state once if needed:
- `GET .../eth/v2/debug/beacon/states/genesis` with `Accept: application/octet-stream`

## Attestation source plan

Engine fetches the plan ONCE at startup, before the slot loop:

- `GET .../fcr-sim/v1/plan?from={warmup_start_slot+1}&to={end_slot}`
- Response: JSON `{ "entries": [{ "sim_slot": N, "source_block_slot": K | null }, ...] }`
- Engine builds an in-memory map `sim_slot → source_block_slot` for fast lookup.

**Plan completeness is contractual.** The response MUST contain exactly one entry per `sim_slot` in `[warmup_start_slot+1, end_slot)`, in ascending order, with no duplicates. Missing, duplicate, or out-of-order entries are a fatal bootstrap error (exit 2) — the engine validates this before starting the slot loop, matching Lighthouse `engine.rs:105`.

**`source_block_slot` may equal or exceed `end_slot`.** The `next-non-missed` planner scans forward up to `lookahead_cap` slots and is permitted to point at a block past the recording range. The orchestrator's `/eth/v2/beacon/blocks/{K}` endpoint MUST serve any `K` that appears as a non-null `source_block_slot` in the plan. If such a fetch returns 404, that is a fatal mid-run error (exit 3) — the orchestrator gave an inconsistent plan.

For each sim_slot during the loop, engine looks up the source and fetches the source block (if non-nil) to extract attestations.

## Attestation injection (CRITICAL)

The engine MUST inject attestations using the equivalent of Lighthouse's `AttestationFromBlock::True`:

- This is a specific fork-choice code path that:
  - Skips current-time target-epoch validation
  - Requires target/head roots to already be present in the proto-array
  - Otherwise behaves as a normal attestation injection

For Lighthouse, this is `fc.on_attestation(inject_slot, indexed, AttestationFromBlock::True)` (see `fork_choice.rs:995-1006`).

For other engines (future), they must use their equivalent injection mode. If no such mode exists, the engine cannot be added to this simulator without spec-level investigation.

### Slot-clock and indexing-state (CRITICAL — pin Lighthouse semantics)

The exact ordering inside one sim_slot iteration is part of the contract. Engines MUST follow it or cross-client comparison is invalid (matches Lighthouse `engine.rs:143`/`engine.rs:261`/`engine.rs:282`/`engine.rs:170`):

1. Advance the chain's internal slot clock to `sim_slot`.
2. If a block exists at `sim_slot`, import it (state transition).
3. Snapshot the head state. Attestations from the plan's `source_block_slot` MUST be indexed (committee → validator index resolution) against THIS state — `chain.head_snapshot().beacon_state` after the sim_slot block is imported but BEFORE `recompute_head` runs. Engines that index from the source block's post-state will diverge.
4. For each indexed attestation, call the from-block injection with `current_slot = inject_slot`.
5. Call the engine's equivalent of `chain.recompute_head_at_slot(inject_slot)`.

`inject_slot = sim_slot + 1`. This is unchanged across epoch boundaries: when `sim_slot % 32 == 31`, `inject_slot` is the first slot of the next epoch — the engine MUST inject/recompute at that slot WITHOUT importing the block at `sim_slot + 1` (the source block is fetched only for attestation extraction; if it happens to be `sim_slot + 1` the block at `sim_slot + 1` is still imported normally on the NEXT iteration when `slot = sim_slot + 1`).

## Replay shortcuts (must be enabled — historical canonical-chain replay)

Engines MUST disable the following safety checks. Without them, the simulator can't process historical blocks:

| Check | Lighthouse mechanism | Rationale |
|---|---|---|
| BLS signature verification | `fake_crypto` feature flag | All blocks are from canonical chain, signatures already validated by mainnet |
| Execution-layer notification | `NotifyExecutionLayer::No` | No EL to talk to in replay mode |
| Blob/data-column availability | `AvailableBlock::new_without_da_check` | Historical replay without blob sidecars |

Future engines must document equivalent bypasses in their `--manifest-json` `build_flags`.

## Output format (intermediate JSONL)

One line per sim_slot in `[start_slot, end_slot)`. Each line is a JSON object with the fields marked "Required" in `SCHEMA_V3.md`. Engine MUST NOT emit orchestrator-added fields.

Engine MUST emit records in ascending `slot` order. Engine MUST flush at least every 100 records to enable crash-resilient mid-run salvage by the orchestrator.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success, all sim_slots in `[start_slot, end_slot)` recorded |
| 1 | Configuration error (bad flags, network mismatch, etc.) |
| 2 | Bootstrap failure (checkpoint state fetch failed, block fetch failed, plan fetch failed) |
| 3 | Mid-run failure (block import failed, fork choice error). Partial JSONL is on disk. |
| 130 | SIGINT — engine should flush and exit cleanly |

Engine MUST write a useful error message to stderr before non-zero exit.

## Error policy

| Situation | Engine behavior |
|---|---|
| Block fetch returns 404 | Treat as missed slot, continue |
| Block fetch returns 5xx | Fatal, exit 2 (bootstrap) or exit 3 (mid-run) |
| Plan fetch returns 5xx | Fatal, exit 2 |
| Plan entry's source_block_slot fetch returns 404 | Fatal, exit 3 (orchestrator gave inconsistent plan) |
| Block import (state transition) fails | Fatal, exit 3 |
| Attestation injection fails | Log warning, continue (matches current Lighthouse behavior at `engine.rs:386`) |
| Orchestrator HTTP server unreachable | Fatal, exit 2 |

## Idempotency

Engine MUST be safe to invoke multiple times with the same `--output` path. The output file is overwritten on each invocation.

## Concurrency

Engine is single-process, single-worker. The orchestrator runs multiple engine subprocesses in parallel, each with disjoint sim_slot ranges. Engine does NOT need internal parallelism.

## Future engines

When adding a new engine (Teku, Lodestar, etc.), the engine binary must:
1. Implement the CLI exactly as specified above (flags, exit codes, error policy).
2. Implement `--manifest-json` returning the documented JSON shape.
3. Write JSONL records matching the "Required" fields in `SCHEMA_V3.md`.
4. Use its native equivalent of `AttestationFromBlock::True`.
5. Document its replay-shortcut bypasses in `build_flags`.

If any of these is impossible for a given engine, that engine cannot be cleanly added without harness-bias risk.
