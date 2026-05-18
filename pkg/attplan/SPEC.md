# TEST_SPEC: pkg/attplan

This is the exhaustive test specification for the attestation source planner. Codex implements both the tests (table-driven, all scenarios below) and the implementation, with this document as the authoritative behavior contract.

**No test in this document is optional.** All must be present and passing. Any deviation needs documented justification.

## Public API

```go
package attplan

type Mode int

const (
    ModeNextNonMissed Mode = iota   // legacy-next-non-missed, parameterized by cap
    ModeStrictKMinus1               // strict-source-block-k-minus-1
)

// Plan is the per-sim-slot attestation source mapping.
// SourceBlockSlot is nil when no source is found within the window or per mode rules.
type Entry struct {
    SimSlot          uint64
    SourceBlockSlot  *uint64
}

// Inputs:
//   blockExists: map[slot]bool. True iff a block exists at that slot.
//                Callers must populate for at least [simStart, simEnd+lookaheadCap].
//   simStart, simEnd: range of sim_slots to plan for. Half-open [simStart, simEnd).
//   mode: which heuristic to apply.
//   lookaheadCap: only meaningful for ModeNextNonMissed. Must be >= 1.
//                 (Ignored for ModeStrictKMinus1 but tests pass it anyway.)
//
// Output: one Entry per sim_slot in [simStart, simEnd), in ascending order.
//
// Errors:
//   - mode == ModeNextNonMissed && lookaheadCap < 1: error
//   - simStart > simEnd: error
//   - simStart == simEnd: empty result (not an error)
func Plan(blockExists map[uint64]bool, simStart, simEnd uint64, mode Mode, lookaheadCap uint64) ([]Entry, error)
```

## Semantic contract (from PLAN.md, restated)

### ModeNextNonMissed
For each sim_slot `N`, the source is the first `K` in `[N+1, N+lookaheadCap]` such that `blockExists[K] == true`. If no such `K`, source is nil.

```python
def source(N, cap):
    for K in range(N+1, N+1+cap):
        if blockExists[K]:
            return K
    return None
```

### ModeStrictKMinus1
For each sim_slot `N`, source is `N+1` if `blockExists[N+1] == true`, else nil. `lookaheadCap` is ignored.

```python
def source(N):
    return N+1 if blockExists[N+1] else None
```

## Test scenarios

Use Go table-driven tests. Each scenario gets a row in a `cases` slice with `name`, `blockExists`, `simStart`, `simEnd`, `mode`, `cap`, `expected` (slice of expected sources, indexed by `simSlot - simStart`).

`expected[i] == nil` means sim_slot `simStart+i` has no source. `expected[i] == &v` means it has source `v`.

### Section A: ModeNextNonMissed — basic missed-slot scenarios

**A1. TestA_NoMisses_Cap4**
- blockExists: {100,101,102,103,104,105} all true
- simRange: [100,104)
- mode: ModeNextNonMissed, cap=4
- expected: 100→101, 101→102, 102→103, 103→104

**A2. TestA_NoMisses_Cap32**
- Same as A1 but cap=32.
- Same expected: cap doesn't matter when no misses.

**A3. TestA_OneMiss_Cap4**
- blockExists: 100,101,103,104 true; 102 false
- simRange: [100,104)
- cap=4
- expected: 100→101, 101→103 (102 missed, next is 103), 102→103, 103→104

**A4. TestA_OneMiss_AtStart_Cap4**
- blockExists: 100,102,103,104,105 true; 101 false
- simRange: [100,104)
- cap=4
- expected: 100→102, 101→102, 102→103, 103→104

**A5. TestA_OneMiss_AtEnd_Cap4**
- blockExists: 100,101,102,103,104,106 true; 105 false
- simRange: [100,104)
- cap=4
- expected: 100→101, 101→102, 102→103, 103→104
  (sim_slot 103 looks at 104,105,106,107: 104 exists, source=104)

### Section B: ModeNextNonMissed — consecutive missed slots within cap

**B1. TestA_TwoConsecutiveMisses_Cap4**
- blockExists: 100,101,104,105 true; 102,103 false
- simRange: [100,104)
- cap=4
- expected: 100→101, 101→104 (next after gap), 102→104, 103→104

**B2. TestA_ThreeConsecutiveMisses_Cap4**
- blockExists: 100,101,105 true; 102,103,104 false
- simRange: [100,104)
- cap=4
- expected: 100→101, 101→105 (window 102..105 includes 105), 102→105 (window 103..106 includes 105), 103→105 (window 104..107 includes 105), 104→105 (window 105..108 includes 105 at boundary)

**B3. TestA_FourConsecutiveMisses_Cap4**
- blockExists: 100,101,106 true; 102..105 false
- simRange: [100,105)
- cap=4
- expected: 100→101, 101→nil (window 102..105 all missed), 102→106, 103→106, 104→106

### Section C: ModeNextNonMissed — cap boundary tests (CRITICAL)

These directly test the cap=4 default and the +N boundary semantics. **The exact value of `range(N+1, N+1+cap)` is the contract — off-by-one here is the most likely bug.**

**C1. TestA_CapBoundary_Cap4_BlockAtPlus4**
- blockExists: 100, 104 true; 101,102,103 false
- simRange: [100,101)
- cap=4
- expected: 100→104
  (sim_slot 100 scans slots 101,102,103,104 — that's 4 slots, all in cap. Block at 104 found at offset +4.)

**C2. TestA_CapBoundary_Cap4_BlockAtPlus5_ShouldBeNil**
- blockExists: 100, 105 true; 101,102,103,104 false
- simRange: [100,101)
- cap=4
- expected: 100→nil
  (sim_slot 100 scans 101..104. Block at 105 is at offset +5, OUTSIDE cap. Source = nil.)

**C3. TestA_CapBoundary_Cap32_BlockAtPlus32**
- blockExists: 100, 132 true; everything between false
- simRange: [100,101)
- cap=32
- expected: 100→132
  (offset +32 is the boundary, inclusive.)

**C4. TestA_CapBoundary_Cap32_BlockAtPlus33_ShouldBeNil**
- blockExists: 100, 133 true; everything between false
- simRange: [100,101)
- cap=32
- expected: 100→nil
  (offset +33 is outside cap=32.)

**C5. TestA_CapBoundary_Cap1_Equivalent_to_StrictB**
- For any input, ModeNextNonMissed with cap=1 must produce identical output to ModeStrictKMinus1.
- Property test: random missed-slot pattern, assert outputs equal.

### Section D: ModeNextNonMissed — exceeded cap → nil

**D1. TestA_AllMissedInRange**
- blockExists: only 100 true; 101..200 all false
- simRange: [100,150)
- cap=4
- expected: all nil

**D2. TestA_ChainStall_Cap32_NoBlockInWindow**
- blockExists: 100 true; 101..140 false
- simRange: [100,109)
- cap=32
- expected: all nil
  (sim_slot 100 scans 101..132, sim_slot 108 scans 109..140 — still nothing.)

**D3. TestA_ChainStall_Cap32_BlockJustOutsideWindow**
- blockExists: 100, 141 true; everything between false
- simRange: [100,109)
- cap=32
- expected: 100→nil, 101→nil, 102→nil, 103→nil, 104→nil, 105→nil, 106→nil, 107→nil, 108→nil
  (sim_slot 108 scans 109..140 — 141 is at +33, just outside cap.)
- Then add sim_slot 109 to the range:
  - simRange: [100,110), cap=32
  - expected[9] (sim_slot 109): 141
  - (sim_slot 109 scans 110..141 — block at 141 is at +32, inclusive.)

### Section E: ModeNextNonMissed — range boundary behavior

**E1. TestA_FinalSimSlot_ScansBeyondEnd**
- blockExists: 100..104 true, 105..110 true (so blocks exist past end_slot)
- simRange: [100,105)
- cap=4
- expected: 100→101, 101→102, 102→103, 103→104, 104→105
  (Final sim_slot 104 sources block at 105, which is BEYOND simEnd. Planner must look past simEnd.)

**E2. TestA_FinalSimSlot_BlocksNotProvidedPastEnd**
- blockExists: 100..104 true; 105..136 NOT IN MAP (treated as false)
- simRange: [100,105)
- cap=4
- expected: 100→101, 101→102, 102→103, 103→104, 104→nil
  (Final sim_slot has no block in its scan window. Returns nil. Caller is responsible for providing blockExists data past simEnd if they want the tail covered.)

**E3. TestA_EmptyRange**
- simRange: [100,100)
- expected: empty slice
- Not an error.

**E4. TestA_SingleSlotRange**
- blockExists: 100,101 true
- simRange: [100,101)
- expected: 100→101

### Section F: ModeStrictKMinus1

**F1. TestB_NoMisses**
- blockExists: 100..105 true
- simRange: [100,104)
- mode: ModeStrictKMinus1
- expected: 100→101, 101→102, 102→103, 103→104

**F2. TestB_OneMiss**
- blockExists: 100,101,103,104 true; 102 false
- simRange: [100,104)
- expected: 100→101, 101→nil (block 102 missed), 102→103, 103→104

**F3. TestB_OneMiss_AtStart**
- blockExists: 100,102,103 true; 101 false
- simRange: [100,103)
- expected: 100→nil, 101→102, 102→103

**F4. TestB_ConsecutiveMisses**
- blockExists: 100,105 true; 101..104 false
- simRange: [100,105)
- expected: 100→nil, 101→nil, 102→nil, 103→nil, 104→105

**F5. TestB_AllMissed**
- blockExists: 100 true; rest false
- simRange: [100,110)
- expected: all nil

**F6. TestB_CapParameterIgnored**
- For any input, ModeStrictKMinus1 must produce same output regardless of `lookaheadCap` value.
- Pass cap=1, cap=4, cap=32, cap=1000 — all must produce identical results.

### Section G: Cross-mode invariants (property tests)

**G1. TestInv_NoMissesAgreement**
- For any input WITHOUT missed slots in [simStart+1, simEnd+1]:
  - ModeNextNonMissed (any cap >= 1) == ModeStrictKMinus1 == "source = sim_slot+1 always"
- Use a fuzz-style or random-input property test.

**G2. TestInv_ModeBIsSubsetOfModeA**
- For any input, every non-nil entry in ModeStrictKMinus1 output is also present (and equal) in ModeNextNonMissed (cap >= 1) output at the same sim_slot index.
- Conversely, every nil in ModeStrictKMinus1 either matches nil OR a later source in ModeNextNonMissed.

**G3. TestInv_ModeASourceInWindow**
- For any input, in ModeNextNonMissed output: if `source[i]` is non-nil, then `simStart+i+1 <= source[i] <= simStart+i+cap`.
- Source is never <= sim_slot or > sim_slot+cap.

**G4. TestInv_DeterministicGivenSameInput**
- Call Plan twice with same inputs. Outputs must be equal.

### Section H: Error handling

**H1. TestErr_Cap0_ModeA**
- simRange: [100,104), mode=ModeNextNonMissed, cap=0
- expected: error (no useful semantics for cap=0)

**H2. TestErr_SimStartAfterSimEnd**
- simRange: [105,100), mode=ModeNextNonMissed, cap=4
- expected: error

**H3. TestErr_Cap0_ModeB_NoError**
- simRange: [100,104), mode=ModeStrictKMinus1, cap=0
- expected: NO error (cap is ignored in ModeB)

### Section I: Numerical edge cases

**I1. TestNum_SlotZero**
- blockExists: 0,1,2,3,4 true
- simRange: [0,4)
- mode: ModeNextNonMissed, cap=4
- expected: 0→1, 1→2, 2→3, 3→4
- (Genesis-adjacent ranges. In practice we won't simulate this, but the planner shouldn't reject slot 0.)

**I2. TestNum_LargeSlotNumbers**
- blockExists: 10_000_000, 10_000_001 true
- simRange: [10_000_000, 10_000_001)
- expected: 10_000_000 → 10_000_001
- (Far-future slot numbers, no overflow.)

**I3. TestNum_NoUInt64Overflow_AtMaxSlot**
- simRange: [math.MaxUint64-100, math.MaxUint64-90), cap=32
- This shouldn't be a real use case, but the planner shouldn't panic.
- expected: implementation choice — either error on overflow or return nil for sources that would overflow. Document and test.
- Recommended: return nil (sim_slot+cap would overflow → no valid source possible).

### Section J: Real-world fixture parity test (CRITICAL for V1 success criterion)

**J1. TestA_Cap4_ReproducesCurrentLighthouseBehavior**
- This is the most important test. Sample 5-10 real ranges from mainnet with known missed slots, fork transitions, and chain density.
- For each range, compute the planner output with ModeNextNonMissed, cap=4.
- Assert it matches a golden file generated from the current simulator's behavior — specifically, the attestation source pattern observed in current `inject_next_block_attestations` (engine.rs:328-403).

Implementation note: we don't have a direct hook for "what source did current Lighthouse pick" in CSV output. **Add one** as part of the slim-Lighthouse work (`source_block_slot` column in JSONL v3 — emit it from both old and new code paths during the parity test). Then this test reads the old simulator's output and asserts the new planner agrees.

If we can't add that column to the old simulator easily, fallback: extract the "would-have-been-source" logic into a separate Rust function in the current codebase, run it offline, dump a CSV of (sim_slot, source_block_slot) pairs, use that as the golden file.

### Section K: Plan output ordering

**K1. TestPlan_OutputIsAscendingBySimSlot**
- Property: for any input, output `[]Entry` is ordered by `SimSlot` ascending with no gaps.

**K2. TestPlan_OutputCoversFullRange**
- Property: len(output) == simEnd - simStart (assuming simStart <= simEnd).
- Every sim_slot in [simStart, simEnd) appears exactly once.

## Performance requirement

`Plan` should complete in O((simEnd - simStart) × cap) time. For typical V1 inputs (~100k sim_slots, cap=4 or 32), this is fast (millions of map lookups in a few ms).

Add a benchmark:
```go
func BenchmarkPlan_100k_Cap4(b *testing.B) { ... }
func BenchmarkPlan_100k_Cap32(b *testing.B) { ... }
```

No specific budget — just ensure it's not pathologically slow.

## Test file structure (codex MUST follow)

```
pkg/attplan/
├── attplan.go              # implementation
├── attplan_test.go         # all unit tests above, table-driven
├── attplan_fuzz_test.go    # property tests (G-series)
├── attplan_bench_test.go   # benchmarks
└── doc.go                  # package-level docstring referencing this spec
```

Use `testify/require` for assertions (concise diffs). Use `quick.Check` or `testing/fuzz.Fuzz` for property tests.

## Required coverage

`go test ./pkg/attplan -cover` must report **>= 95% line coverage**. Anything lower means there's a code path without a test. Codex should add tests until the threshold is met or document why the uncovered lines are unreachable.

## Self-verification checklist for codex

Before declaring done:
- [ ] All table-driven test cases listed above have a corresponding `case{...}` entry in the test slice. Names match.
- [ ] All property tests (G-series) are implemented as standalone test functions.
- [ ] All error cases (H-series) return non-nil errors and are tested via `require.Error`.
- [ ] Benchmark exists and runs without error.
- [ ] `go test ./pkg/attplan/... -race` passes.
- [ ] `go vet ./pkg/attplan/...` clean.
- [ ] Coverage >= 95%.
- [ ] J1 fixture parity test is wired even if the golden file is a TODO (place a `t.Skip` with a clear reason if the fixture can't be generated yet).
