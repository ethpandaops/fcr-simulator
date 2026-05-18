## Multi-engine FCR simulator — design plan

Status: V1 scope reduced to single-engine refactor (Teku deferred). Design locked. Not yet started cutting code. Resumable from this file.

### V1 goal (this round)

Refactor the current Lighthouse-only FCR simulator into an **orchestrator + engine** architecture. The architecture is engine-agnostic by design, but in V1 only one engine (Lighthouse) is implemented. The point of this round is to:
- prove the orchestrator-engine contract works end-to-end against a known-good baseline
- unlock the future addition of Teku (and other CL clients) as "implement the engine CLI contract" rather than "redesign the system"
- fix existing bugs (`block_root` column writing `head_root`; JSON-array output when run.sh expects JSONL/CSV-with-schema-marker; uncapped 4-slot attestation lookahead)
- get the attestation source heuristic under unit test so the boundaries and missed-slot behavior are no longer a design worry

**V1 success criterion: orchestrator + slimmed Lighthouse engine produce slot-by-slot identical results to the current in-process Lighthouse simulator on the same range.** Test on ranges that include missed slots, epoch boundaries, Deneb blobs, and chunk boundaries. Any deviation is a harness bug, fix before declaring done.

### Future scope (not V1)

Adding additional engines (Teku first, then Lodestar / Nimbus / Prysm / Grandine). End-state UX:

```
fcr-simulator --engine=lighthouse|teku --network=mainnet --start-epoch=N --end-epoch=N \
              --warmup-epochs=10 --parallel=4 --beacon-node-url=... --output=results.csv
```

Multi-engine runs against the same input produce results that can be diffed on `slot` for cross-client divergence analysis. The V1 architecture supports this without further refactoring — adding an engine is "build a CLI binary that speaks the engine contract and writes JSONL with `engine_name=<name>`".

### Current state (single engine, V0)

- `fcr-simulator` is today a Rust crate living inside a fork of Lighthouse at `lighthouse/fcr-simulator/`.
- `lighthouse/fcr-simulator/src/sim/engine.rs` is the core: builds a headless `BeaconChain`, processes each block from ERA files, injects the next block's attestations into fork choice, reads FCR state from `chain.canonical_head.fast_confirmation`, writes one CSV row per slot.
- ERA download, xatu reader, beacon checkpoint state fetch, parallel worker management, CSV merge, all live inside the Lighthouse Rust binary today.
- Xatu attestation timing data exists but is being deferred for V1 (the README notes xatu vs. next-block attestations agreed within 0.005%).

### Architecture (locked)

**Orchestrator-as-beacon-node.** A new top-level Go binary owns everything engine-agnostic. Engines become subprocesses that talk to it over a localhost beacon-API HTTP server. The orchestrator is the source of truth for **blocks** (served via standard beacon-API) and the **attestation-source plan** (a per-sim-slot mapping telling each engine which block to source attestations from). Engines do the actual attestation extraction from blocks using their native fork-aware block parsers.

```
┌────────────────────────────────────────────────────────────────┐
│  cmd/fcr-orchestrator  (Go)                                    │
│  - CLI, chunking, worker spawn                                 │
│  - Download ERA files into shared cache                        │
│  - Fork-aware block decoder (extract attestations per fork)    │
│  - Attestation-source planner (decide which block per sim_slot)│
│  - Per-worker checkpoint state fetch from real BN (once each)  │
│  - Localhost HTTP server (beacon-API + /fcr-sim/v1)            │
│  - JSONL → CSV merge + run-manifest sidecar                    │
└────────────────────────────────────────────────────────────────┘
                       ▲
                       │  HTTP (SSZ bodies, Eth-Consensus-Version)
                       │
       ┌───────────────┴──────────────┐
       │  fcr-lighthouse (Rust)       │
       │  fcr-teku       (Java)       │
       └──────────────────────────────┘
```

**Engine API contract (orchestrator HTTP server):**
```
GET /eth/v2/beacon/blocks/{block_id}              → SSZ block       (404 on missed/unknown)
GET /eth/v2/debug/beacon/states/{state_id}        → SSZ state       (per-worker checkpoint)
GET /eth/v2/beacon/genesis                        → JSON genesis info
GET /fcr-sim/v1/plan?from={slot}&to={slot}        → JSON: per-sim-slot attestation source plan
    Response: {"entries": [{"sim_slot": N, "source_block_slot": K|null}, ...]}
    Engine reads this once at startup, holds in memory.
    For each sim_slot N, the engine fetches block K via /eth/v2/beacon/blocks/{K}
    (already cached if K was processed earlier or will be soon) and extracts
    attestations using its native block-parsing code.
```

**`block_id` and `state_id` semantics** (per beacon-API spec, https://ethereum.github.io/beacon-APIs):
- Decimal integer → look up by slot. 404 if slot is missed or out of range.
- `0x`-prefixed hex → look up by root. 404 if not in cache.
- Special: `genesis`, `head`, `finalized`, `justified` may be supported as needed (V1 needs at minimum `genesis` for state_id since current Lighthouse uses `/eth/v2/debug/beacon/states/genesis`).
- Current Lighthouse bootstrap fetches state-by-slot, derives `latest_block_root`, then fetches the block **by root**. Both lookups must work or bootstrap fails.

Why no `/fcr-sim/v1/attestations/{slot}` endpoint: engines already have battle-tested fork-aware block-parsing code (it's how they normally handle state transition). Inventing a separate `List[Attestation]` SSZ codec — which would need fork-aware updates every Electra/Fulu — adds harness surface area without value. The orchestrator owns *selection* (which block sources attestations for which sim_slot); engines own *extraction* from already-fetched blocks.

**Engine slot loop** (same shape for Lighthouse and Teku):
```
plan = GET /fcr-sim/v1/plan?from=warmup_start&to=end    # once, at startup
for slot in (warmup_start, end):                        # exclusive start (warmup_start+1)
    block = GET /eth/v2/beacon/blocks/{slot}            # may be 404
    if block: chain.process_block(block)
    source_block_slot = plan[slot].source_block_slot
    if source_block_slot is not None:
        source_block = GET /eth/v2/beacon/blocks/{source_block_slot}
        for att in source_block.body.attestations:
            indexed = current_state.index(att)          # engine-native (needs state)
            fc.on_attestation(slot+1, indexed, FromBlock=True)
    chain.recompute_head_at_slot(slot+1)
    emit JSONL { slot, has_block, head_root, confirmed_root, confirmed_slot, ... }
```

`FromBlock=True` is **not optional** — it's a specific code path in fork choice that:
- skips current-time target-epoch validation
- requires target/head roots to already be in proto-array
- See Lighthouse `fork_choice.rs:995, 1006`

Teku must use its equivalent path. If it uses a different attestation injection mode, observed divergences will be harness artifacts, not real client differences. **The whole point of the multi-engine simulator is to surface real client differences; harness-introduced noise defeats it.** This is the load-bearing invariant of the architecture.

**CLI contract — both engines accept identical flags:**
```
fcr-<engine> \
  --beacon-node-url http://127.0.0.1:PORT \
  --start-slot N --end-slot N --warmup-start-slot N \
  --network mainnet \
  --byzantine-threshold 25 \
  --output worker-0.jsonl
```

**Repo layout (target):**
```
fcr-simulator/
├── cmd/fcr-orchestrator/main.go
├── pkg/
│   ├── era/                 # lazy ERA reader: file → slot→byte-offset index, LRU on decoded blocks
│   ├── blockdecode/         # fork-aware block decoder (attestantio/go-eth2-client or ethpandaops/ethcore)
│   ├── attsource/           # attestation source planner (see "Open question" below)
│   ├── beaconapi/           # HTTP server: blocks (by slot or root), states (by slot, root, or special id), genesis info, /fcr-sim/v1/plan
│   ├── beaconfetch/         # HTTP client to real BN for per-worker checkpoint state
│   ├── chunk/               # epoch range splitting, worker spawn, JSONL collect
│   ├── merge/               # JSONL → CSV merge + first-divergence diff tool
│   ├── manifest/            # run manifest sidecar writer
│   └── schema/              # SlotResult schema v3 definition
├── engines/
│   ├── lighthouse/          # submodule: samcm/lighthouse:fcr-simulator pinned SHA
│   └── teku/                # submodule: ethpandaops/teku fork pinned SHA + patch series
├── docker/
│   ├── orchestrator.Dockerfile
│   ├── engine-lighthouse.Dockerfile
│   └── engine-teku.Dockerfile
└── run.sh                   # chunked runner, gains --engine flag
```

### Cross-engine contract decisions

- **Output pipeline**: engine → intermediate JSONL → orchestrator → final JSONL + CSV.
  - Each engine binary writes intermediate JSONL records to its `--output` path (one record per sim_slot, fields specified in `SCHEMA_V3.md`).
  - The orchestrator reads each worker's intermediate JSONL, validates against the schema, enriches each record with engine metadata (`engine_name`, `engine_version`, `engine_commit`) captured at engine startup, and writes the final user-facing output: a single normalized JSONL file and a single CSV file (with `# fcr-simulator-csv-schema-version:3` marker for `run.sh` parity).
  - Engines never write final CSV. This keeps the schema in one language (Go) and prevents per-engine drift.
- **Schema v3** is defined explicitly in `SCHEMA_V3.md` (Phase 1 entry criterion), not by example. Must include every field with type, units, nullable, and meaning.
- **Orchestrator owns attestation source selection**, not extraction. The orchestrator decides which block sources attestations for each sim_slot and serves this as a plan. Engines extract attestations from the source block using their native fork-aware parsers (already part of normal state-transition code).
- **Per-worker checkpoint state**, not one global. Each worker bootstraps from `start_epoch - warmup_epochs`. Orchestrator fetches + caches N SSZ states.
- **Version metadata**: each engine implements `--manifest-json` returning `{engine_name, engine_version, engine_commit, build_flags, fcr_spec_commit}`. Orchestrator captures it before launch, embeds in JSONL records and run manifest. Run manifest also includes ERA file hashes, network config, input config.
- **Determinism is not assumed**. Expect cross-engine divergence; we build divergence-finding tooling from day one (first-divergence finder, per-slot trace mode `--trace-slot=N` re-runs `[N-16, N+16]` with verbose output).

### Phase 1 — Orchestrator scaffolding (Go) + slim Lighthouse engine

In dependency order:

1. **`pkg/era`** — lazy ERA reader. ERA file format = e2store records (length-prefixed). Each entry is snappy-compressed SSZ block. Build `slot → byte-offset` index on file open, decode on-demand, LRU cache for decoded blocks.
2. **`pkg/blockdecode`** — fork-aware decoder. Pick a Go lib (`attestantio/go-eth2-client` or `ethpandaops/ethcore`) that handles Phase0/Altair/Bellatrix/Capella/Deneb/Electra/Fulu. Provides typed access to `block.body.attestations` (orchestrator only needs to read attestation counts and block existence for plan generation — engines do the actual attestation extraction).
3. **`pkg/attplan`** — attestation source planner. Pure function `(slot → block_exists, mode, cap) → (sim_slot → source_block_slot)`. Implements both `next-non-missed` (parameterized by cap) and `strict-source-block-k-minus-1` modes (see "Attestation source heuristic" below).

   **3a. Unit test scenarios (write the tests before the implementation):**
   ```
   ModeA / next-non-missed (cap parameterized):
     TestA_NoMisses                → next slot always (any cap >= 1)
     TestA_OneMiss                 → skip the missed slot, point at next existing
     TestA_ConsecutiveMisses_WithinCap → same source block for all gap sim_slots
     TestA_ConsecutiveMisses_ExceedCap → sim_slots beyond cap return nil

     TestA_CapBoundary_Cap4_Block5  → with cap=4 and only N+5 existing, source = nil
                                       (N+5 is 5 slots forward, outside cap=4)
     TestA_CapBoundary_Cap4_Block4  → with cap=4 and only N+4 existing, source = N+4
                                       (exact boundary, included)
     TestA_CapBoundary_Cap32_Block32 → with cap=32 and only N+32 existing, source = N+32
     TestA_CapBoundary_Cap32_Block33 → with cap=32 and only N+33 existing, source = nil

     TestA_EndOfRange_ScanPastEnd  → final sim_slot in output range scans into blocks
                                       beyond end_slot; works correctly
     TestA_AllMissedInRange        → all sim_slots return nil
     TestA_Cap4_ReproducesCurrent  → exact byte-equal match against fixtures sampled
                                       from current simulator's behavior (parity test)

   ModeB / strict-source-block-k-minus-1:
     TestB_NoMisses                → source = N+1 always
     TestB_OneMiss                 → sim_slot for the missed-next is nil
     TestB_ConsecutiveMisses       → all gap sim_slots return nil
     TestB_EndOfRange              → final sim_slot's source is N+1 (within or beyond end)

   Cross-mode invariants:
     InvA_ModeASourceNilIffNoBlockInWindow
     InvB_ModeBSourceExactlyNPlus1IfPresent
     InvAB_ModeBSourceIsSubsetOfModeA   // every non-nil in B is also non-nil in A (and equal)

   Row range:
     TestRange_MatchesEngineLoop   → planner output covers slots
                                       [warmup_start+1, end_slot) matching engine.rs:200
   ```
   Each test is a tabular (input, expected) pair. The implementation falls out from making them pass. Property test `PropPlannerIsPure` dropped — it tests a Go-language guarantee, not a domain invariant.
4. **`pkg/beaconapi`** — HTTP server. Endpoints documented above. `Accept: application/octet-stream` returns SSZ. `404` for missed slots. Sets `Eth-Consensus-Version` header.
5. **`pkg/beaconfetch`** — fetches per-worker checkpoint state, block, genesis from real BN once at startup. Handles missed-checkpoint-slot by scanning backward to the closest prior non-missed block (or rejecting the run with a clear error).
6. **`cmd/fcr-orchestrator`** — CLI, chunking, subprocess spawn, JSONL collect, CSV merge, manifest writer.
7. **Slim Lighthouse engine** (must happen in Phase 1, not deferred):
   - Drop `era/`, `xatu/`, multi-worker spawn, beacon state pre-fetch from `lighthouse/fcr-simulator/src/`.
   - Replace `EraBlockIterator` with HTTP block fetcher pointing at orchestrator.
   - Replace `inject_next_block_attestations` with: load plan at startup, for each sim_slot fetch source block via existing HTTP path, extract attestations natively, inject with `AttestationFromBlock::True`.
   - Replace JSON-array output (`writer.rs:33-44, 80-82` — wraps records in `[ ... ]`) with JSONL (newline-delimited). CSV output stays as-is but moves into the orchestrator (engines only emit JSONL; orchestrator converts to CSV).
   - Add `--manifest-json` flag.

**Note on previously-claimed bugs that aren't actually bugs in current code:**
- `block_root` column writes the actual canonical block root (`engine.rs:207, 622`), not `head_root`. The earlier plan claimed otherwise — wrong, no fix needed.
- CSV schema marker `# fcr-simulator-csv-schema-version:2` is emitted (`writer.rs:24`). The earlier plan claimed it was missing — wrong. The marker convention will continue in v3 with orchestrator-owned CSV.

**Phase 1 success criterion: orchestrator + slimmed Lighthouse engine produce slot-by-slot identical results to the current in-process Lighthouse simulator on the same range.** Test on a range that includes missed slots, epoch boundaries, Deneb blobs, and chunk boundaries. Any deviation is a harness bug — fix before proceeding.

### Phase 2 — Packaging

- Two Dockerfiles for V1: `orchestrator.Dockerfile`, `engine-lighthouse.Dockerfile`. Orchestrator image bundles the Lighthouse engine binary on `$PATH`.
- `run.sh` gains `--engine` flag (passes through to orchestrator). For V1 only `lighthouse` is valid — but the flag is there so future engines slot in cleanly.
- k8s benchmark.yaml updated.

### Deferred to future scope (post-V1)

**Teku engine** (next priority after V1):
- Fork `Nashatyrev/teku:confirmation-2` into `ethpandaops/teku`. Pin a SHA, not a floating branch.
- Apply patches as a `git format-patch` series:
  - Lift `ConfirmationRuleReplay.replay()` from JUnit test into `tech.pegasys.teku.fcrsimulator.Main` with proper `main()`.
  - Restructure replay loop to bootstrap once from checkpoint state + process blocks internally (mirror Lighthouse's shape).
  - Switch to orchestrator's `/eth/v2/beacon/blocks/{slot}` + `/fcr-sim/v1/plan` for blocks and attestation sources.
  - Patch `ConfirmationRuleUtil` + `ForkChoice` to expose a structured FCR callback alongside `DEBUG_PRINTER`.
  - JSONL writer + `--manifest-json` flag.
- **Teku spike still needed before this lands** — checklist below (preserved from prior plan):
  - [ ] Anton's branch builds locally with our network config.
  - [ ] BLS signature bypass possible (Lighthouse equivalent: `fake_crypto`).
  - [ ] Execution-layer notification bypass possible (Lighthouse equivalent: `NotifyExecutionLayer::No`).
  - [ ] Blob/data-column availability check bypass possible (Lighthouse equivalent: `AvailableBlock::new_without_da_check`).
  - [ ] Weak-subjectivity bootstrap from arbitrary slot.
  - [ ] Structured FCR readout (no log-string parsing).
  - [ ] "Attestation from block" injection path identified (equivalent to `AttestationFromBlock::True`).

**Other engines**: Lodestar, Nimbus, Prysm, Grandine — see "State of FCR across CL clients" reference table.

**Xatu attestation timing**: re-add as a third `--attestation-source-mode=xatu` once V1 lands.

### Attestation source heuristic

Three configurations, all implemented. The orchestrator emits the per-sim-slot mapping via `/fcr-sim/v1/plan`.

**Mode A: `next-non-missed` (parameterized by `--lookahead-cap`, default `cap=4` in V1)**

For each `sim_slot N`, the source block is the **first non-missed block at slot K > N**, scanning forward up to `N + lookahead_cap`.

```python
def source_block_slot(N, cap):
    for K in range(N + 1, N + 1 + cap):
        if block_exists(K):
            return K
    return None
```

**V1 default is cap=4** because that exactly reproduces today's behavior (Lighthouse `engine.rs:346` scans `1..=4`) and lets the V1 parity criterion ("slot-by-slot identical to current simulator") actually hold. Raising the cap is a deliberate experiment, not the default.

**Cap=32 is the spec maximum** (`SLOTS_PER_EPOCH` = max legal attestation inclusion delay). Available as `--lookahead-cap=32` to measure how often raising the cap changes outcomes vs cap=4. Hypothesis: small bias because most attestations land in the immediate next block, but worth measuring rather than assuming.

**Mode B: `strict-source-block-k-minus-1`**

For each `sim_slot N`, the source block is **only** block at slot N+1, if it exists. Otherwise no source.

```python
def source_block_slot(N):
    return N + 1 if block_exists(N + 1) else None
```

Honest about data limitation: during missed-slot runs, FCR evaluations have no fresh attestations and proceed with whatever was already in fork choice. Doesn't "fill the gap" with later-block contents that physically didn't exist yet.

**Why all three:** Mode A with cap=4 is what the existing simulator does — it's the parity baseline. Mode A with cap=32 raises the question "does picking up late inclusions from longer missed-slot runs change confirmation rates measurably." Mode B is the stricter observability model. Running all three and diffing CSVs quantifies the bias of each design choice.

**Why not bucket by `attestation.data.slot`:** that's a fourth option ("gossip is instant") that requires assumptions we can't verify from ERA data alone. Deferred until xatu data is reintegrated, at which point xatu IS the gossip-timing ground truth.

**CLI flags**:
- `--attestation-source-mode={next-non-missed,strict-source-block-k-minus-1}` (default `next-non-missed`)
- `--lookahead-cap=N` (default `4`, only meaningful for Mode A)

### State of FCR across CL clients (reference)

| Client | FCR status | Integration plan |
|---|---|---|
| Lighthouse | sigp/lighthouse#8951 (dapplion), WIP | Existing — slim down in Phase 2 |
| Teku | Nashatyrev/teku:confirmation-2 (Anton, personal fork, no upstream PR yet) | Phase 3 |
| Lodestar | ChainSafe/lodestar#8837 (nazarhussain), most mature non-merged | Future — `@lodestar/fork-choice` is npm-importable |
| Nimbus | ~25 PRs merged into master (Etan Kissling) | Future — adapt their test runner (PR #8131) |
| Prysm | OffchainLabs/prysm#15164 (terencechain), draft, OLD algorithm | Blocked — PR implements adiasg/eth2.0-specs:3e3ef28a, not consensus-specs#4747. See `docs/ENGINES_PRYSM.md` |
| Grandine | grandinetech/grandine#656 (bomanaps), Apr 2026, external contributor | Future — Rust, similar to Lighthouse pattern |

### Codex review takeaways (folded into plan above)

Round 1:
- Teku state-fetch model incompatible with one-state-per-worker — patch Teku to bootstrap once, process blocks internally.
- Per-worker checkpoint, not per-run.
- Lazy ERA indexing, not full in-memory decode.
- Schema owned by orchestrator, engines emit JSONL.
- Attestation source contract must be cross-engine (orchestrator owns selection).
- Divergence-finding tooling from day one.
- Pin Teku SHA, don't track Anton's floating branch.
- Codex suggested Rust orchestrator; user picked Go. Sticking with Go.

Round 2 (after PLAN.md draft):
- Phase 1 milestone "works with unmodified Lighthouse" was meaningless — Lighthouse uses ERA, not HTTP. **Fixed**: Phase 1 includes the slim-Lighthouse work; success criterion is byte-equal slot-by-slot with the current simulator.
- Beacon API was missing `/eth/v2/debug/beacon/states/genesis`; current Lighthouse uses this. **Fixed**: endpoint list updated.
- Checkpoint block at warmup slot can 404 if slot is missed. **Fixed**: orchestrator scans backward for closest prior non-missed block, or rejects with a clear error.
- Inventing a bespoke `List[Attestation]` SSZ codec was a mistake — would need fork-aware updates every Electra/Fulu. **Fixed**: orchestrator returns a per-sim-slot plan (`source_block_slot` per row); engines fetch the source block via existing `/eth/v2/beacon/blocks/{K}` endpoint and extract attestations natively using their existing fork-aware block parsers.
- `AttestationFromBlock::True` is not a detail — it's a specific fork-choice code path. **Fixed**: documented as load-bearing in the engine contract, Teku must use the equivalent path.
- Attestation heuristic was muddled. Scan-forward is NOT "observability-correct"; it's "legacy-optimistic". **Fixed**: two named modes (`legacy-next-non-missed` and `strict-source-block-k-minus-1`), both implemented, default to legacy for backward-compat, measure the bias between them.
- Teku is the real Phase 1 risk, not Phase 3 polish. **Fixed**: Phase 0 added — Teku spike with explicit go/no-go checklist before any Go is written.
- Current Rust JSON output is a JSON-array (not JSONL); current CSV doesn't emit the schema marker `run.sh` checks for. **To fix in Phase 1**: both during the slim-down.

Round 3 (after V1 scope cut + codex re-review):
- 4-vs-32 cap parity conflict surfaced: V1 default must be cap=4 to satisfy parity criterion. **Fixed**: cap is parameterized, default `4`; cap=32 is a measured experiment, not the default.
- Beacon API `block_id` / `state_id` semantics were incomplete — current Lighthouse fetches the checkpoint block by root, not slot. **Fixed**: endpoint spec now requires both slot and root lookups per the beacon-API spec.
- Plan contradicted itself on attestation extraction (some sections said orchestrator served attestations, others said engines extracted from blocks). **Fixed**: single voice — orchestrator owns selection (plan), engines own extraction (native parsing).
- Two "bug fix" line items were stale (`block_root` column and CSV schema marker). **Fixed**: removed; only the JSON-array→JSONL change is a real fix.
- `pkg/attplan` tests were directionally right but missing cap-boundary tests, row-range alignment, and stronger domain invariants. **Fixed**: expanded test list.
- Schema ownership was underspecified — engines wrote JSONL to `--output`, but orchestrator also "owned schema." **Fixed**: explicit pipeline — engine writes intermediate JSONL, orchestrator validates + enriches with metadata + writes final JSONL + CSV.

Phase 1 entry criteria (write before coding):
- `SCHEMA_V3.md` — every JSONL field with type, units, nullable, meaning. Plus the CSV column order.
- `ENGINE_CONTRACT.md` — precise inclusive/exclusive range semantics, error policy (fatal vs warn), required JSONL field list, manifest JSON shape, expected behavior on `block_id`/`state_id` 404s, `AttestationFromBlock::True` requirement.

### Other deferrals (beyond engine deferrals listed above)

- Long-lived engine daemons (subprocess-per-worker is V1; only revisit if a future engine's startup cost becomes painful).
- Bulk attestation/block endpoints if per-slot HTTP shows up as bottleneck (unlikely at localhost).

### Resuming work from this file

If returning cold:
1. Read this file top to bottom.
2. Re-read `lighthouse/fcr-simulator/src/sim/engine.rs` and `main.rs` for current behavior.
3. **V1 is single-engine refactor (Lighthouse only).** Teku and other engines are deferred — see "Deferred to future scope" section.
4. Start with Phase 1 step 1: Go ERA reader. But `pkg/attplan` (step 3) is the lowest-risk, highest-clarity place to start — pure function, exhaustive unit tests, sets the design contract early.
5. Treat the Phase 1 success criterion as load-bearing: slot-by-slot identical to the current simulator on a real range, or the refactor isn't done.

### Load-bearing invariants (do not violate)

Two architectural properties must hold or the project doesn't deliver its value:

1. **Both engines must use the equivalent of `AttestationFromBlock::True` for attestation injection.** Different injection paths produce different fork-choice behavior. Harness-introduced divergence undermines the entire point of multi-engine comparison.

2. **Both engines must use the same bypass set: no BLS verification, no execution-layer notification, no blob/data-column availability checks.** These are replay shortcuts for processing historical canonical chains; if Lighthouse bypasses and Teku doesn't (or vice versa), comparison is invalid.

If either of these can't be satisfied for a given engine, surface it before integration — better to ship without that engine than to ship with a known harness bias.
