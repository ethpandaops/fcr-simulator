package attplan

import "testing"

func benchmarkPlan100K(b *testing.B, cap uint64) {
	const (
		simStart = uint64(10_000_000)
		length   = uint64(100_000)
	)

	blockExists := make(map[uint64]bool, length+cap+1)
	for slot := simStart; slot <= simStart+length+cap; slot++ {
		if slot%17 != 0 && slot%43 != 0 {
			blockExists[slot] = true
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := Plan(blockExists, simStart, simStart+length, ModeNextNonMissed, cap)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != int(length) {
			b.Fatalf("got len %d, want %d", len(got), length)
		}
	}
}

func BenchmarkPlan_100k_Cap4(b *testing.B) {
	benchmarkPlan100K(b, 4)
}

func BenchmarkPlan_100k_Cap32(b *testing.B) {
	benchmarkPlan100K(b, 32)
}
