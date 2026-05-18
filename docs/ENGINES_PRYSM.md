# Prysm engine — integration status

**Status as of 2026-05-14: BLOCKED on upstream rebase.** No `fcr-prysm` binary is shipped.

This document records the investigation. The intent is that the next person who picks up Prysm integration does not redo the homework.

## Upstream PR

- [OffchainLabs/prysm#15164](https://github.com/OffchainLabs/prysm/pull/15164) "Implement fast confirmation" by terencechain
- Branch: `fast-confirmation`
- Head SHA at investigation: `89053cdb70154d09e9c40a4f3867b5ad6e2a421b`
- State: draft, last code update 2026-03-07
- Feature flag: `--safe-head-fcu`

No other Prysm FCR work exists. Older branches `fast-confirmations` and `fast-confirmations-no-el` on the same repo are from 2024 and predate this PR. terencechain's personal fork has not been pushed since 2022.

## Algorithm mismatch — the blocker

The PR description explicitly cites the algorithm from [adiasg/eth2.0-specs:3e3ef28a/fork_choice/confirmation-rule.md](https://github.com/adiasg/eth2.0-specs/blob/3e3ef28ace725b7b883f1908189406c890a569c3/fork_choice/confirmation-rule.md). This is the **old** confirmation rule.

The fcr-simulator's reference engine (Lighthouse) implements [consensus-specs#4747](https://github.com/ethereum/consensus-specs/pull/4747) — the current FCR spec. See `lighthouse/beacon_node/beacon_chain/src/fast_confirmation.rs` line 3: `"Implements the algorithm from consensus-specs PR #4747."`

Concretely the differences are:

| Aspect | consensus-specs#4747 (Lighthouse) | adiasg-3e3ef28a (Prysm PR #15164) |
|---|---|---|
| Output | `confirmed_root` = deepest block satisfying chain-safety (walks back from head) | `safeHeadRoot` = head of `bestConfirmedDescendant` chain from justified root |
| Confirmation predicate | `is_one_confirmed` + chain-safety check across all ancestors | `confirmed()` (one-confirmation only) per node; the chain forms transitively via `bestConfirmedDescendant` |
| Safety check | Combines `is_one_confirmed` with `is_confirmed_chain_safe` (multi-block safety) | Only the one-confirmation predicate; no separate chain-safety pass |
| Adversarial weight model | Uses spec's `get_attestation_score` | Uses `maxPossibleSupport` forward-projection |
| Cadence | Per-slot evaluation on every `recompute_head_at_slot` | Only fires when `highestReceivedNode.slot == CurrentSlot(genesisTime)` AND `secondsSinceSlotStart < 4s` |

Shipping a `fcr-prysm` engine that drives this old algorithm and emits records into the same JSONL stream as `fcr-lighthouse` would produce slot-level disagreement that does not reflect Prysm-vs-Lighthouse implementation differences within a fixed algorithm. It would reflect algorithm-level differences. That breaks the load-bearing invariant in `PLAN.md`: cross-engine divergence must surface real client differences, not harness-introduced bias.

## What was checked beyond the algorithm

The mechanical integration questions are answerable; they are not the blocker:

- **Attestation injection without slot-timing validation**: Prysm's `ForkChoice.ProcessAttestation(ctx, validatorIndices, blockRoot, targetEpoch)` (`beacon-chain/forkchoice/doubly-linked-tree/forkchoice.go:187`) takes raw validator indices and does no slot-timing checks. This is a timing-bypass equivalent to Lighthouse's `AttestationFromBlock::True` path. It does not enforce target/head root presence in the proto-array up front; root-presence handling falls out later in balance application. No patch needed to inject, but the engine must take care that the source block's root is already known to the fork-choice store before injection (it will be, since the engine has just processed that block via `InsertNode`).
- **BLS bypass**: Prysm exposes `transition.ExecuteStateTransitionNoVerifyAnySig(...)` for state transition without signature verification.
- **EL notification bypass**: Driving `ForkChoice.InsertNode(ctx, state, roblock)` directly avoids `blockchain.Service.ReceiveBlock`, which is what notifies the EL.
- **DA / blob bypass**: Same — direct fork-choice driving sidesteps the blob root verifier.
- **Wall-clock dependency for safe-head update**: Prysm's `Head()` only invokes `updateSafeHead` when `highestReceivedNode.slot == slots.CurrentSlot(genesisTime)`. Inside `updateBestDescendant`, `bestConfirmedDescendant` is only recomputed when `secondsSinceSlotStart < SecondsPerSlot/IntervalsPerSlot`. Because the safe-head check compares against `prevSlot = currentSlot - 1`, the replay engine must drive `SetGenesisTime` so that at sim_slot N, `CurrentSlot(genesisTime) == N` (matching the just-inserted block's slot) and `secondsSinceSlotStart` is in the first interval. Sim-slot N's recorded confirmation refers to block N itself (via `prevSlot = N - 1` semantics of the predicate, applied to `bestChild` of justified root walking down — confirmation is "is this child's tip safe given the votes accumulated as of the previous slot").
- **`safeHeadRoot` accessor**: Not publicly exposed. Minimum patch on the Prysm fork would add `func (f *ForkChoice) SafeHeadRoot() [32]byte`.
- **Build system**: Prysm has a working `go.mod` (`github.com/OffchainLabs/prysm/v6`); Bazel is not required. A wrapper Go module under `engines/prysm/` with `replace github.com/OffchainLabs/prysm/v6 => <fork>@<sha>` works.

These are all solvable. The algorithm mismatch is what makes the engine not useful.

## What to do when Prysm rebases

When OffchainLabs/prysm#15164 (or a successor) rebases onto consensus-specs#4747, the integration plan is:

1. Fork `OffchainLabs/prysm` at the new SHA into `ethpandaops/prysm` (similar to the existing Lighthouse + Teku patterns).
2. Add `SafeHeadRoot()` / `ConfirmedRoot()` getter as a thin patch.
3. Build wrapper `engines/prysm/cmd/fcr-prysm` against the fork via `replace` in `engines/prysm/go.mod`.
4. Implement the engine per `docs/ENGINE_CONTRACT.md`:
   - Bootstrap: load checkpoint state into a `BalancesByRooter` that can serve the checkpoint's justified/finalized roots; seed forkchoice with `InsertNode(checkpointState, checkpointBlock)`; ensure `justifiedCheckpoint.Root` and `finalizedCheckpoint.Root` resolve to nodes (insert ancestor blocks back to finalized as needed, or rely on Prysm's genesis-special-case if applicable).
   - Per block: run `transition.ExecuteStateTransitionNoVerifyAnySig` to produce post-state; `InsertNode(postState, roblock)`.
   - Per sim-slot: extract attesting indices via `helpers.AttestationCommitteesFromState` + `attestation.AttestingIndices`, call `ProcessAttestation`.
   - Once #4747 lands, the Lighthouse-equivalent driver is `recompute_head_at_slot(sim_slot+1)`. The Prysm equivalent will need whatever the rebased PR's API exposes; the wall-clock gymnastics described in the previous section are only required for the old-algorithm shape. Refer to the post-rebase code, not to this section.
   - Read `SafeHeadRoot()` (or `ConfirmedRoot()` once #4747 lands), look up node slot via `ForkChoice.Slot(root)`, emit JSONL per schema v3.
5. `build_flags`: the orchestrator currently requires the literal `"fake_crypto"` marker (Lighthouse's bypass-name carried over as a manifest convention). Include it plus Prysm-specific markers: `["fake_crypto","prysm:NoVerifyAnySig","no_el_notify","no_da_check"]`. The orchestrator should be updated to also accept a more general bypass-set marker; until then, including the literal token is the compatibility path.
6. Manifest `engine_name: "prysm"`, `fcr_spec_commit: <consensus-specs#4747 head SHA when merged>`.

## Why this is in the repo and not just a comment

Future contributors should not need to read the Prysm PR description, diff the algorithm against the spec, and re-derive the conclusion. The investigation conclusion is the artifact.
