# SCHEMA v3 — engine intermediate JSONL + orchestrator final output

## Output pipeline

```
Engine writes:   worker-{N}.jsonl  (intermediate, per-worker)
                       │
                       ▼
Orchestrator reads, validates, enriches with metadata, writes:
                 results.jsonl     (final, single file, full schema)
                 results.csv       (final, single file, with schema marker)
```

## Schema version markers

- JSONL: every record contains `"schema_version": 3`.
- CSV: first line is `# fcr-simulator-csv-schema-version:3`, second line is the header row, subsequent lines are records.

## Fields

Required = engine MUST emit. Orchestrator-added = engine MUST NOT emit (orchestrator fills).

| Field | Type | Required / Added | Nullable | Description |
|---|---|---|---|---|
| `schema_version` | int | Orchestrator-added | no | Always `3` in this version |
| `engine_name` | string | Orchestrator-added | no | e.g. `lighthouse` — captured from engine's `--manifest-json` at startup |
| `engine_version` | string | Orchestrator-added | no | Engine's reported version string |
| `engine_commit` | string | Orchestrator-added | no | Git commit SHA of the engine build, if known; else empty string |
| `slot` | uint64 | Required | no | The sim slot this row describes |
| `epoch` | uint64 | Required | no | `slot / 32` |
| `has_block` | bool | Required | no | True iff a block exists at this slot (canonical chain) |
| `block_root` | string \| null | Required | yes (when `has_block` is false) | Hex-prefixed canonical root of the block at this slot |
| `head_root` | string | Required | no | Hex-prefixed root of fork-choice head after this slot's processing |
| `confirmed_root` | string | Required | no | Hex-prefixed FCR-confirmed root, or `0x000...` if none |
| `confirmed_slot` | uint64 | Required | no | Slot of the confirmed root, or 0 if none |
| `confirmation_delay_slots` | uint64 | Required | no | `(slot + 1) - confirmed_slot` (saturating sub). The `+1` matches the slot the engine recomputed the head at; a same-slot confirmation therefore reports delay=1. Matches Lighthouse `engine.rs:346`. |
| `fast_confirmed` | bool | Required | no | True iff `confirmed_root != 0x000...` AND `confirmed_slot == slot` |
| `strict_one_slot_confirmed` | bool | Required | no | True iff strict 1-slot confirmation rule fired |
| `finalized_epoch` | uint64 | Required | no | Engine's reported finalized epoch at this slot |
| `justified_epoch` | uint64 | Required | no | Engine's reported justified epoch at this slot |
| `source_block_slot` | uint64 \| null | Required | yes | Block slot the orchestrator's plan said to source attestations from; null if no source |
| `num_attestations_injected` | uint64 | Required | no | How many attestations the engine actually injected for this sim_slot |
| `is_epoch_boundary` | bool | Required | no | `slot % 32 == 0` |
| `is_missed_slot` | bool | Required | no | `!has_block` |
| `fcr_eval_duration_us` | uint64 | Required | no | Wall-time microseconds for FCR evaluation at this slot |
| `attestation_source_mode` | string | Orchestrator-added | no | The orchestrator's mode: `next-non-missed` or `strict-source-block-k-minus-1` |
| `lookahead_cap` | uint64 | Orchestrator-added | no | Cap used. 0 if mode is strict |

## CSV column order

```
schema_version, engine_name, engine_version, engine_commit,
slot, epoch, has_block, block_root, head_root,
confirmed_root, confirmed_slot, confirmation_delay_slots,
fast_confirmed, strict_one_slot_confirmed,
finalized_epoch, justified_epoch,
source_block_slot, num_attestations_injected,
is_epoch_boundary, is_missed_slot,
fcr_eval_duration_us,
attestation_source_mode, lookahead_cap
```

## Run manifest sidecar

Path: alongside `results.csv`, named `results.manifest.json`.

```json
{
  "schema_version": 3,
  "fcr_simulator_version": "<git SHA of this repo>",
  "ran_at": "<ISO-8601 UTC>",
  "config": {
    "engine": "lighthouse",
    "network": "mainnet",
    "start_epoch": 435000,
    "end_epoch": 435100,
    "warmup_epochs": 10,
    "parallel": 4,
    "attestation_source_mode": "next-non-missed",
    "lookahead_cap": 4,
    "byzantine_threshold": 25,
    "beacon_node_url": "http://...",
    "era_url": "https://mainnet.era.nimbus.team"
  },
  "engine_manifest": {
    "engine_name": "lighthouse",
    "engine_version": "...",
    "engine_commit": "...",
    "build_flags": ["fake_crypto"],
    "fcr_spec_commit": "<from PLAN if known>"
  },
  "inputs": {
    "era_files": [
      {"era": 1390, "url": "...", "sha256": "..."},
      ...
    ],
    "checkpoint_states": [
      {"worker": 0, "slot": 13920000, "sha256": "..."},
      ...
    ]
  },
  "outputs": {
    "results_jsonl_sha256": "...",
    "results_csv_sha256": "...",
    "total_slots": 3200,
    "fast_confirmed_count": ...
  }
}
```

## JSONL example record

```json
{"schema_version":3,"engine_name":"lighthouse","engine_version":"5.x","engine_commit":"abc123","slot":13920100,"epoch":435003,"has_block":true,"block_root":"0xabc...","head_root":"0xabc...","confirmed_root":"0xabc...","confirmed_slot":13920100,"confirmation_delay_slots":1,"fast_confirmed":true,"strict_one_slot_confirmed":true,"finalized_epoch":435001,"justified_epoch":435002,"source_block_slot":13920101,"num_attestations_injected":128,"is_epoch_boundary":false,"is_missed_slot":false,"fcr_eval_duration_us":42,"attestation_source_mode":"next-non-missed","lookahead_cap":4}
```

## Missing field policy

- Engine emits a record with a missing required field → orchestrator rejects the run with a clear error.
- Engine emits an orchestrator-added field → orchestrator rejects (engines must not preempt those fields).
- Orchestrator MAY add fields the engine didn't know about (so v3 engines forward-compatible with future orchestrator versions).

## Backward compatibility

v3 is NOT compatible with v2 readers. The schema marker bump makes this explicit. Old `run.sh` chunk-merge logic must be updated to recognize `csv-schema-version:3`.
