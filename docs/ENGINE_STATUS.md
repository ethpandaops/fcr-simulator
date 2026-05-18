# Engine integration status

Tracker for the multi-engine expansion. Updated as per-client engine PRs land.

| Engine | Upstream pin | PR | Smoke (10 ep) | Notes |
|---|---|---|---|---|
| lighthouse | submodule `lighthouse/` | n/a (V1) | baseline-ready | Reference engine. `results/cross-client/lighthouse/` populated by `scripts/cross-client/run.sh lighthouse ...`. |
| teku | `Nashatyrev/teku@c5825d53` (branch `confirmation-2`) | in-flight | pending | `ConfirmationRuleReplay.java` ported to `tech.pegasys.teku.fcrsimulator.Main`; FCR result currently log-only — wrapper must patch `ConfirmationRuleUtil` to expose `getConfirmedRoot()`. |
| lodestar | `ChainSafe/lodestar@549b4c39` (PR #8837) | in-flight | pending | `chain.forkChoice.getConfirmedRoot()` is structured; `onAttestation(att, {fromBlock:true})` is the from-block path. |
| nimbus | `status-im/nimbus-eth2@550c7a3f` (unstable, FCR merged) | in-flight | pending | `fkChoice.backend.confirmed.{root,slot}` is structured. Build needs Nimbus's pinned Nim toolchain (5–10 min one-time bootstrap). |
| prysm | `OffchainLabs/prysm@89053cdb` (PR #15164, draft) | #2 — deferred | n/a | Implements `adiasg/eth2.0-specs:3e3ef28`, **NOT** consensus-specs#4747. Shipping a binary would contaminate the comparison. See `docs/ENGINES_PRYSM.md`. |
| grandine | `bomanaps/grandine@9905f46f` (PR #656) | in-flight | pending | `Context::confirmed_root()` is structured; `AttestationOrigin::Block` is the from-block path. Wrapper crate at `engines/grandine/grandine/fcr-simulator/`. |

## Contract enforcement

Every engine binary MUST satisfy the contract in `docs/ENGINE_CONTRACT.md` and report `fake_crypto` in `--manifest-json` `build_flags`. The orchestrator's `validateEngineManifest()` rejects engines that do not. Run `scripts/cross-client/manifest-check.sh` against each new binary to verify before committing to a 10-epoch smoke.

## Smoke + diff

1. Build each engine binary into `results/fcr-<engine>` (see each engine's README).
2. `scripts/cross-client/manifest-check.sh` — fast contract validation.
3. `scripts/cross-client/run.sh <engine> ./results/fcr-<engine>` — writes per-slot CSVs to `results/cross-client/<engine>/epoch-<N>.csv` for the 10 deterministic epochs from `pick-epochs.py`.
4. `scripts/cross-client/diff.py` — emits `per-slot.csv`, `divergences.csv`, and a per-column disagreement summary. `source_block_slot` is treated as consensus; `num_attestations_injected` and `fcr_eval_duration_us` are diagnostic-only.

A missing `(engine, slot)` row in the per-slot output is a hard divergence.
