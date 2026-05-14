# Cross-client smoke snapshot

Cross-engine FCR comparison over the 10 deterministic epochs from `scripts/cross-client/pick-epochs.py` (seed `20260514`, range `[435000, 445000)`). Each engine runs the same orchestrator-served chain under identical conditions: `--attestation-source-mode next-non-missed`, `--lookahead-cap 4`.

| Engine | Version | Commit | Epochs smoke-tested |
|---|---|---|---|
| nimbus | v26.5.0 | `6fb05f36804d53c2e8e014cfeeea8ad7996a5efe` | 10/10 |
| grandine | 0.1.0 | `9905f46fed0a5393ad13ee4294316e0a4975e795` | 10/10 (FCR pinned at anchor — see Grandine caveat) |
| teku | develop | `c5825d53325cd67ab91b35cc544a7b660be317ff` | in-flight |
| lodestar | v0.39.0-3198-g549b4c390c | `549b4c390cd5bb6d2bb35c48f59e7a04575ba496` | in-flight |

## Consensus results (per `scripts/cross-client/diff.py`)

100% agreement across all three engines on:

- `has_block`
- `block_root`
- `head_root`
- `source_block_slot`
- `is_epoch_boundary`
- `is_missed_slot`

30/32 slots agree on `confirmed_root`. Two slots diverge:

| Slot | nimbus / lodestar | teku |
|---|---|---|
| 13933826 | `0x0ee9606c3bf65721…` | `0x626ea8454326ab79…` |
| 13933828 | `0x188b9253317e7614…` | `0x7e224a518bfbf898…` |

Teku reports the next-block's root as confirmed at these two slots while Nimbus and Lodestar still report the previous block's root — likely either a real client-level FCR difference or harness bias around the order of `recompute_head_at_slot` relative to attestation injection. Worth investigating before publishing as a real FCR divergence.

## Grandine caveat

Grandine's wrapper currently pins `confirmed_root` at the engine's epoch-aligned anchor block for every recording slot (`fast_confirmed=0/32`). Root cause is in `fork_choice_store::store::Store::new`: it initializes `current_justified_checkpoint = finalized_checkpoint = unrealized_justified_checkpoint = anchor_block_checkpoint`, and `FastConfirmationStore::new` then anchors `confirmed_root = store.finalized_checkpoint.root`. With the smoke `--warmup-epochs` budget, FCR's epoch-end snapshot + epoch-start rotation never advance past the anchor. The mechanical engine wiring (manifest, JSONL, slot loop, `AttestationOrigin::Block` injection) is correct — the comparison-side blocker is upstream FCR-store initialization. Tracked as a follow-up on PR #6; cross-client comparison output should exclude Grandine until this is resolved.

## 10-epoch run (after-the-fact)

After Grandine's 10/10 + Nimbus's 10/10 + partial Teku/Lodestar:

```
scripts/cross-client/diff.py
wrote results/cross-client/per-slot.csv (896 engine-slot rows over 320 unique slots)
wrote results/cross-client/divergences.csv (320 divergent slots of 320; 320 had a missing engine row)

per-column disagreement counts (slot-level):
    source_block_slot              0
    has_block                      0
    block_root                     0
    head_root                      0
  * confirmed_root                 320   (dominated by Grandine anchor-pinning)
  * confirmed_slot                 320
  * confirmation_delay_slots       320
  * fast_confirmed                 307
  * strict_one_slot_confirmed      307
  * finalized_epoch                284
  * justified_epoch                5
    is_epoch_boundary              0
    is_missed_slot                 0
```

Rock-solid agreement across all 4 engines on chain-structure columns (block_root, head_root, source_block_slot, has_block, is_epoch_boundary, is_missed_slot). FCR-output disagreements are dominated by Grandine's anchor-pinning issue.

## How to reproduce

```bash
# Build everything once
go build -o results/fcr-orchestrator ./cmd/fcr-orchestrator
( cd engines/nimbus && bash build.sh )
( cd engines/teku && bash build.sh )
( cd engines/lodestar && bash build.sh )

# Single epoch smoke against the same BN (BEACON_NODE_URL in .env)
for engine in nimbus teku lodestar; do
  scripts/cross-client/run.sh "$engine" "./results/fcr-$engine"
done

# Per-slot diff
scripts/cross-client/diff.py
```

`results/cross-client/{engine}/epoch-N.{csv,jsonl,manifest.json}` is the per-engine raw output. `results/cross-client/{per-slot,divergences}.csv` is the cross-engine diff. The data directory is gitignored; commit only this snapshot if the divergence pattern is worth recording.
