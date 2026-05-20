package attplan

import (
	"fmt"
	"math"
)

// Mode selects the attestation source heuristic.
type Mode int

const (
	// ModeNextNonMissed scans forward to the first non-missed block within cap.
	ModeNextNonMissed Mode = iota
	// ModeStrictKMinus1 only uses the block at sim_slot + 1.
	ModeStrictKMinus1
	// ModeGreedyLookahead lets engines consume every non-missed block in the
	// lookahead window. The single-source planner entry remains a representative
	// first source for the existing HTTP schema.
	ModeGreedyLookahead
)

// Entry is the per-sim-slot attestation source mapping.
//
// SourceBlockSlot is nil when no source is found within the configured window
// or mode rules.
type Entry struct {
	SimSlot         uint64
	SourceBlockSlot *uint64
}

// Plan returns one Entry per sim_slot in [simStart, simEnd), in ascending order.
//
// blockExists is treated as false for missing keys. ModeNextNonMissed and
// ModeGreedyLookahead require lookaheadCap >= 1. ModeStrictKMinus1 ignores
// lookaheadCap.
func Plan(blockExists map[uint64]bool, simStart, simEnd uint64, mode Mode, lookaheadCap uint64) ([]Entry, error) {
	if simStart > simEnd {
		return nil, fmt.Errorf("simStart %d is after simEnd %d", simStart, simEnd)
	}
	if (mode == ModeNextNonMissed || mode == ModeGreedyLookahead) && lookaheadCap < 1 {
		return nil, fmt.Errorf("lookaheadCap must be >= 1 for mode %d", mode)
	}
	if mode != ModeNextNonMissed && mode != ModeStrictKMinus1 && mode != ModeGreedyLookahead {
		return nil, fmt.Errorf("unsupported attestation source mode %d", mode)
	}

	entries := make([]Entry, 0, int(simEnd-simStart))
	for simSlot := simStart; simSlot < simEnd; simSlot++ {
		var source *uint64
		switch mode {
		case ModeNextNonMissed, ModeGreedyLookahead:
			source = nextNonMissedSource(blockExists, simSlot, lookaheadCap)
		case ModeStrictKMinus1:
			source = strictKMinus1Source(blockExists, simSlot)
		}

		entries = append(entries, Entry{
			SimSlot:         simSlot,
			SourceBlockSlot: source,
		})
	}

	return entries, nil
}

func nextNonMissedSource(blockExists map[uint64]bool, simSlot, lookaheadCap uint64) *uint64 {
	// If simSlot + lookaheadCap would overflow uint64, no valid source can exist
	// in the requested window. Return nil rather than wrapping arithmetic.
	if lookaheadCap > math.MaxUint64-simSlot {
		return nil
	}

	start := simSlot + 1
	end := simSlot + lookaheadCap

	// Avoid `slot <= end`: when end is MaxUint64, incrementing slot would wrap
	// to 0 and continue forever.
	for slot := start; ; slot++ {
		if blockExists[slot] {
			source := slot
			return &source
		}
		if slot == end {
			break
		}
	}

	return nil
}

func strictKMinus1Source(blockExists map[uint64]bool, simSlot uint64) *uint64 {
	source := simSlot + 1
	if !blockExists[source] {
		return nil
	}

	return &source
}
