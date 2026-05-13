package attplan

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

type planCase struct {
	name        string
	blockExists map[uint64]bool
	simStart    uint64
	simEnd      uint64
	mode        Mode
	cap         uint64
	expected    []*uint64
}

func source(slot uint64) *uint64 {
	return &slot
}

func blockMap(slots ...uint64) map[uint64]bool {
	blockExists := make(map[uint64]bool, len(slots))
	for _, slot := range slots {
		blockExists[slot] = true
	}
	return blockExists
}

func blockRange(start, endInclusive uint64) map[uint64]bool {
	blockExists := make(map[uint64]bool, endInclusive-start+1)
	for slot := start; slot <= endInclusive; slot++ {
		blockExists[slot] = true
	}
	return blockExists
}

func nilSources(count int) []*uint64 {
	return make([]*uint64, count)
}

func requirePlan(t *testing.T, got []Entry, simStart uint64, expected []*uint64) {
	t.Helper()

	require.Len(t, got, len(expected))
	for i, entry := range got {
		require.Equal(t, simStart+uint64(i), entry.SimSlot, "sim_slot at index %d", i)
		if expected[i] == nil {
			require.Nil(t, entry.SourceBlockSlot, "source for sim_slot %d", entry.SimSlot)
			continue
		}
		require.NotNil(t, entry.SourceBlockSlot, "source for sim_slot %d", entry.SimSlot)
		require.Equal(t, *expected[i], *entry.SourceBlockSlot, "source for sim_slot %d", entry.SimSlot)
	}
}

func runPlanCases(t *testing.T, cases []planCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Plan(tc.blockExists, tc.simStart, tc.simEnd, tc.mode, tc.cap)
			require.NoError(t, err)
			requirePlan(t, got, tc.simStart, tc.expected)
		})
	}
}

func randomBlockMap(rng *rand.Rand, start, endInclusive uint64) map[uint64]bool {
	blockExists := make(map[uint64]bool, endInclusive-start+1)
	for slot := start; slot <= endInclusive; slot++ {
		if rng.Intn(2) == 0 {
			blockExists[slot] = true
		}
	}
	return blockExists
}

func TestPlan_TableDrivenSpecScenarios(t *testing.T) {
	cases := []planCase{
		{
			name:        "TestA_NoMisses_Cap4",
			blockExists: blockMap(100, 101, 102, 103, 104, 105),
			simStart:    100,
			simEnd:      104,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(102), source(103), source(104)},
		},
		{
			name:        "TestA_NoMisses_Cap32",
			blockExists: blockMap(100, 101, 102, 103, 104, 105),
			simStart:    100,
			simEnd:      104,
			mode:        ModeNextNonMissed,
			cap:         32,
			expected:    []*uint64{source(101), source(102), source(103), source(104)},
		},
		{
			name:        "TestA_OneMiss_Cap4",
			blockExists: blockMap(100, 101, 103, 104),
			simStart:    100,
			simEnd:      104,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(103), source(103), source(104)},
		},
		{
			name:        "TestA_OneMiss_AtStart_Cap4",
			blockExists: blockMap(100, 102, 103, 104, 105),
			simStart:    100,
			simEnd:      104,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(102), source(102), source(103), source(104)},
		},
		{
			name:        "TestA_OneMiss_AtEnd_Cap4",
			blockExists: blockMap(100, 101, 102, 103, 104, 106),
			simStart:    100,
			simEnd:      104,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(102), source(103), source(104)},
		},
		{
			name:        "TestA_TwoConsecutiveMisses_Cap4",
			blockExists: blockMap(100, 101, 104, 105),
			simStart:    100,
			simEnd:      104,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(104), source(104), source(104)},
		},
		{
			name:        "TestA_ThreeConsecutiveMisses_Cap4",
			blockExists: blockMap(100, 101, 105),
			simStart:    100,
			simEnd:      105,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(105), source(105), source(105), source(105)},
		},
		{
			name:        "TestA_FourConsecutiveMisses_Cap4",
			blockExists: blockMap(100, 101, 106),
			simStart:    100,
			simEnd:      105,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), nil, source(106), source(106), source(106)},
		},
		{
			name:        "TestA_CapBoundary_Cap4_BlockAtPlus4",
			blockExists: blockMap(100, 104),
			simStart:    100,
			simEnd:      101,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(104)},
		},
		{
			name:        "TestA_CapBoundary_Cap4_BlockAtPlus5_ShouldBeNil",
			blockExists: blockMap(100, 105),
			simStart:    100,
			simEnd:      101,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{nil},
		},
		{
			name:        "TestA_CapBoundary_Cap32_BlockAtPlus32",
			blockExists: blockMap(100, 132),
			simStart:    100,
			simEnd:      101,
			mode:        ModeNextNonMissed,
			cap:         32,
			expected:    []*uint64{source(132)},
		},
		{
			name:        "TestA_CapBoundary_Cap32_BlockAtPlus33_ShouldBeNil",
			blockExists: blockMap(100, 133),
			simStart:    100,
			simEnd:      101,
			mode:        ModeNextNonMissed,
			cap:         32,
			expected:    []*uint64{nil},
		},
		{
			name:        "TestA_AllMissedInRange",
			blockExists: blockMap(100),
			simStart:    100,
			simEnd:      150,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    nilSources(50),
		},
		{
			name:        "TestA_ChainStall_Cap32_NoBlockInWindow",
			blockExists: blockMap(100),
			simStart:    100,
			simEnd:      109,
			mode:        ModeNextNonMissed,
			cap:         32,
			expected:    nilSources(9),
		},
		{
			name:        "TestA_ChainStall_Cap32_BlockJustOutsideWindow",
			blockExists: blockMap(100, 141),
			simStart:    100,
			simEnd:      109,
			mode:        ModeNextNonMissed,
			cap:         32,
			expected:    nilSources(9),
		},
		{
			name:        "TestA_ChainStall_Cap32_BlockAtBoundary_WithExtendedRange",
			blockExists: blockMap(100, 141),
			simStart:    100,
			simEnd:      110,
			mode:        ModeNextNonMissed,
			cap:         32,
			expected:    []*uint64{nil, nil, nil, nil, nil, nil, nil, nil, nil, source(141)},
		},
		{
			name:        "TestA_FinalSimSlot_ScansBeyondEnd",
			blockExists: blockRange(100, 110),
			simStart:    100,
			simEnd:      105,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(102), source(103), source(104), source(105)},
		},
		{
			name:        "TestA_FinalSimSlot_BlocksNotProvidedPastEnd",
			blockExists: blockRange(100, 104),
			simStart:    100,
			simEnd:      105,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101), source(102), source(103), source(104), nil},
		},
		{
			name:        "TestA_EmptyRange",
			blockExists: blockMap(100, 101),
			simStart:    100,
			simEnd:      100,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{},
		},
		{
			name:        "TestA_SingleSlotRange",
			blockExists: blockMap(100, 101),
			simStart:    100,
			simEnd:      101,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(101)},
		},
		{
			name:        "TestB_NoMisses",
			blockExists: blockRange(100, 105),
			simStart:    100,
			simEnd:      104,
			mode:        ModeStrictKMinus1,
			cap:         4,
			expected:    []*uint64{source(101), source(102), source(103), source(104)},
		},
		{
			name:        "TestB_OneMiss",
			blockExists: blockMap(100, 101, 103, 104),
			simStart:    100,
			simEnd:      104,
			mode:        ModeStrictKMinus1,
			cap:         4,
			expected:    []*uint64{source(101), nil, source(103), source(104)},
		},
		{
			name:        "TestB_OneMiss_AtStart",
			blockExists: blockMap(100, 102, 103),
			simStart:    100,
			simEnd:      103,
			mode:        ModeStrictKMinus1,
			cap:         4,
			expected:    []*uint64{nil, source(102), source(103)},
		},
		{
			name:        "TestB_ConsecutiveMisses",
			blockExists: blockMap(100, 105),
			simStart:    100,
			simEnd:      105,
			mode:        ModeStrictKMinus1,
			cap:         4,
			expected:    []*uint64{nil, nil, nil, nil, source(105)},
		},
		{
			name:        "TestB_AllMissed",
			blockExists: blockMap(100),
			simStart:    100,
			simEnd:      110,
			mode:        ModeStrictKMinus1,
			cap:         4,
			expected:    nilSources(10),
		},
		{
			name:        "TestNum_SlotZero",
			blockExists: blockRange(0, 4),
			simStart:    0,
			simEnd:      4,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(1), source(2), source(3), source(4)},
		},
		{
			name:        "TestNum_LargeSlotNumbers",
			blockExists: blockMap(10_000_000, 10_000_001),
			simStart:    10_000_000,
			simEnd:      10_000_001,
			mode:        ModeNextNonMissed,
			cap:         4,
			expected:    []*uint64{source(10_000_001)},
		},
	}

	runPlanCases(t, cases)
}

func TestA_CapBoundary_Cap1_Equivalent_to_StrictB(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(10_000))
		length := uint64(rng.Intn(64) + 1)
		blockExists := randomBlockMap(rng, simStart, simStart+length+1)

		next, err := Plan(blockExists, simStart, simStart+length, ModeNextNonMissed, 1)
		require.NoError(t, err)
		strict, err := Plan(blockExists, simStart, simStart+length, ModeStrictKMinus1, 0)
		require.NoError(t, err)
		require.Equal(t, strict, next)
	}
}

func TestB_CapParameterIgnored(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(10_000))
		length := uint64(rng.Intn(64) + 1)
		blockExists := randomBlockMap(rng, simStart, simStart+length+1)

		baseline, err := Plan(blockExists, simStart, simStart+length, ModeStrictKMinus1, 1)
		require.NoError(t, err)
		for _, cap := range []uint64{1, 4, 32, 1000} {
			got, err := Plan(blockExists, simStart, simStart+length, ModeStrictKMinus1, cap)
			require.NoError(t, err)
			require.Equal(t, baseline, got)
		}
	}
}

func TestErr_Cap0_ModeA(t *testing.T) {
	got, err := Plan(blockMap(100, 101), 100, 104, ModeNextNonMissed, 0)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestErr_SimStartAfterSimEnd(t *testing.T) {
	got, err := Plan(blockMap(100, 101), 105, 100, ModeNextNonMissed, 4)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestErr_Cap0_ModeB_NoError(t *testing.T) {
	got, err := Plan(blockMap(100, 101), 100, 104, ModeStrictKMinus1, 0)
	require.NoError(t, err)
	requirePlan(t, got, 100, []*uint64{source(101), nil, nil, nil})
}

func TestErr_UnsupportedMode(t *testing.T) {
	got, err := Plan(blockMap(100, 101), 100, 104, Mode(99), 4)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestNum_NoUInt64Overflow_AtMaxSlot(t *testing.T) {
	t.Run("spec range near max does not panic", func(t *testing.T) {
		got, err := Plan(map[uint64]bool{}, math.MaxUint64-100, math.MaxUint64-90, ModeNextNonMissed, 32)
		require.NoError(t, err)
		requirePlan(t, got, math.MaxUint64-100, nilSources(10))
	})

	t.Run("valid max-ending lookahead window finds source", func(t *testing.T) {
		got, err := Plan(blockMap(math.MaxUint64-3, math.MaxUint64-2), math.MaxUint64-4, math.MaxUint64-3, ModeNextNonMissed, 4)
		require.NoError(t, err)
		requirePlan(t, got, math.MaxUint64-4, []*uint64{source(math.MaxUint64 - 3)})
	})

	t.Run("overflowing lookahead window returns nil source", func(t *testing.T) {
		got, err := Plan(blockMap(math.MaxUint64-3, math.MaxUint64-2), math.MaxUint64-3, math.MaxUint64-2, ModeNextNonMissed, 4)
		require.NoError(t, err)
		requirePlan(t, got, math.MaxUint64-3, []*uint64{nil})
	})

	t.Run("empty max-ending lookahead window terminates", func(t *testing.T) {
		got, err := Plan(map[uint64]bool{}, math.MaxUint64-4, math.MaxUint64-3, ModeNextNonMissed, 4)
		require.NoError(t, err)
		requirePlan(t, got, math.MaxUint64-4, []*uint64{nil})
	})

	t.Run("max lookahead cap overflowing from nonzero slot returns nil source", func(t *testing.T) {
		got, err := Plan(blockMap(2), 1, 2, ModeNextNonMissed, math.MaxUint64)
		require.NoError(t, err)
		requirePlan(t, got, 1, []*uint64{nil})
	})
}

func TestA_Cap4_ReproducesCurrentLighthouseBehavior(t *testing.T) {
	t.Skip("golden fixture file not yet generated — TODO: produce from current Lighthouse simulator's source-block decisions")
}

func TestPlan_OutputIsAscendingBySimSlot(t *testing.T) {
	rng := rand.New(rand.NewSource(3))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(10_000))
		length := uint64(rng.Intn(128))
		blockExists := randomBlockMap(rng, simStart, simStart+length+32)

		got, err := Plan(blockExists, simStart, simStart+length, ModeNextNonMissed, 32)
		require.NoError(t, err)
		for i, entry := range got {
			require.Equal(t, simStart+uint64(i), entry.SimSlot)
		}
	}
}

func TestPlan_OutputCoversFullRange(t *testing.T) {
	rng := rand.New(rand.NewSource(4))

	for iter := 0; iter < 200; iter++ {
		simStart := uint64(rng.Intn(10_000))
		length := uint64(rng.Intn(128))
		blockExists := randomBlockMap(rng, simStart, simStart+length+32)

		got, err := Plan(blockExists, simStart, simStart+length, ModeNextNonMissed, 32)
		require.NoError(t, err)
		require.Len(t, got, int(length))

		seen := make(map[uint64]bool, len(got))
		for _, entry := range got {
			require.GreaterOrEqual(t, entry.SimSlot, simStart)
			require.Less(t, entry.SimSlot, simStart+length)
			require.False(t, seen[entry.SimSlot])
			seen[entry.SimSlot] = true
		}
	}
}
