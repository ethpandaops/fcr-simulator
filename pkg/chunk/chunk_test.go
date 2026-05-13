package chunk

import (
	"reflect"
	"testing"
)

func TestSplitUnevenDivision(t *testing.T) {
	got := Split(100, 110, 10, 3)
	want := []Chunk{
		{Index: 0, StartEpoch: 100, EndEpoch: 104, WarmupStartSlot: 90 * SlotsPerEpoch, StartSlot: 100 * SlotsPerEpoch, EndSlot: 104 * SlotsPerEpoch},
		{Index: 1, StartEpoch: 104, EndEpoch: 107, WarmupStartSlot: 94 * SlotsPerEpoch, StartSlot: 104 * SlotsPerEpoch, EndSlot: 107 * SlotsPerEpoch},
		{Index: 2, StartEpoch: 107, EndEpoch: 110, WarmupStartSlot: 97 * SlotsPerEpoch, StartSlot: 107 * SlotsPerEpoch, EndSlot: 110 * SlotsPerEpoch},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Split() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestSplitParallelOne(t *testing.T) {
	got := Split(5, 8, 2, 1)
	want := []Chunk{
		{Index: 0, StartEpoch: 5, EndEpoch: 8, WarmupStartSlot: 3 * SlotsPerEpoch, StartSlot: 5 * SlotsPerEpoch, EndSlot: 8 * SlotsPerEpoch},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Split() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestSplitSmallRangeKeepsWorkerCountAndClampsWarmup(t *testing.T) {
	got := Split(0, 2, 10, 4)
	want := []Chunk{
		{Index: 0, StartEpoch: 0, EndEpoch: 1, WarmupStartSlot: 0, StartSlot: 0, EndSlot: 32},
		{Index: 1, StartEpoch: 1, EndEpoch: 2, WarmupStartSlot: 0, StartSlot: 32, EndSlot: 64},
		{Index: 2, StartEpoch: 2, EndEpoch: 2, WarmupStartSlot: 0, StartSlot: 64, EndSlot: 64},
		{Index: 3, StartEpoch: 2, EndEpoch: 2, WarmupStartSlot: 0, StartSlot: 64, EndSlot: 64},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Split() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestSplitInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		start, end  uint64
		parallel    int
		warmupEpoch uint64
	}{
		{name: "zero workers", start: 1, end: 2, parallel: 0},
		{name: "negative workers", start: 1, end: 2, parallel: -1},
		{name: "empty range", start: 2, end: 2, parallel: 1},
		{name: "reversed range", start: 3, end: 2, parallel: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Split(tc.start, tc.end, tc.warmupEpoch, tc.parallel); got != nil {
				t.Fatalf("Split() = %#v, want nil", got)
			}
		})
	}
}
