package attplan

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInv_NoMissesAgreement(t *testing.T) {
	rng := rand.New(rand.NewSource(10))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(100_000))
		length := uint64(rng.Intn(128) + 1)
		cap := uint64(rng.Intn(64) + 1)
		simEnd := simStart + length

		blockExists := make(map[uint64]bool, length+cap)
		for slot := simStart + 1; slot <= simEnd+cap; slot++ {
			blockExists[slot] = true
		}

		next, err := Plan(blockExists, simStart, simEnd, ModeNextNonMissed, cap)
		require.NoError(t, err)
		strict, err := Plan(blockExists, simStart, simEnd, ModeStrictKMinus1, 0)
		require.NoError(t, err)
		require.Equal(t, strict, next)

		for i, entry := range next {
			require.NotNil(t, entry.SourceBlockSlot)
			require.Equal(t, simStart+uint64(i)+1, *entry.SourceBlockSlot)
		}
	}
}

func TestInv_ModeBIsSubsetOfModeA(t *testing.T) {
	rng := rand.New(rand.NewSource(11))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(100_000))
		length := uint64(rng.Intn(128) + 1)
		cap := uint64(rng.Intn(64) + 1)
		simEnd := simStart + length
		blockExists := randomBlockMap(rng, simStart, simEnd+cap)

		next, err := Plan(blockExists, simStart, simEnd, ModeNextNonMissed, cap)
		require.NoError(t, err)
		strict, err := Plan(blockExists, simStart, simEnd, ModeStrictKMinus1, 0)
		require.NoError(t, err)
		require.Len(t, next, len(strict))

		for i := range strict {
			if strict[i].SourceBlockSlot != nil {
				require.NotNil(t, next[i].SourceBlockSlot)
				require.Equal(t, *strict[i].SourceBlockSlot, *next[i].SourceBlockSlot)
				continue
			}

			if next[i].SourceBlockSlot != nil {
				require.Greater(t, *next[i].SourceBlockSlot, strict[i].SimSlot+1)
			}
		}
	}
}

func TestInv_ModeASourceInWindow(t *testing.T) {
	rng := rand.New(rand.NewSource(12))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(100_000))
		length := uint64(rng.Intn(128) + 1)
		cap := uint64(rng.Intn(64) + 1)
		simEnd := simStart + length
		blockExists := randomBlockMap(rng, simStart, simEnd+cap)

		next, err := Plan(blockExists, simStart, simEnd, ModeNextNonMissed, cap)
		require.NoError(t, err)
		for _, entry := range next {
			if entry.SourceBlockSlot == nil {
				continue
			}
			require.GreaterOrEqual(t, *entry.SourceBlockSlot, entry.SimSlot+1)
			require.LessOrEqual(t, *entry.SourceBlockSlot, entry.SimSlot+cap)
		}
	}
}

func TestInv_DeterministicGivenSameInput(t *testing.T) {
	rng := rand.New(rand.NewSource(13))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(100_000))
		length := uint64(rng.Intn(128) + 1)
		cap := uint64(rng.Intn(64) + 1)
		simEnd := simStart + length
		blockExists := randomBlockMap(rng, simStart, simEnd+cap)

		first, err := Plan(blockExists, simStart, simEnd, ModeNextNonMissed, cap)
		require.NoError(t, err)
		second, err := Plan(blockExists, simStart, simEnd, ModeNextNonMissed, cap)
		require.NoError(t, err)
		require.Equal(t, first, second)
	}
}
