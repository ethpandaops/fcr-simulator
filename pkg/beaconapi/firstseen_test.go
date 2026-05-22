package beaconapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"
)

func TestFirstSeenAttestationSourceGroupsRowsByVoteTuple(t *testing.T) {
	base := writeFirstSeenFixture(t, "mainnet", 2, []firstSeenParquetRow{
		firstSeenRow(64, 2, 10, "3", rootWithByte(0x11), 1000),
		firstSeenRow(64, 2, 12, "not-a-committee", rootWithByte(0x11), 12000),
		firstSeenRow(64, 2, 11, "3", rootWithByte(0x11), 12001),
		firstSeenRow(64, 2, 10, "3", rootWithByte(0x22), 1000),
	})
	source, err := NewFirstSeenAttestationSource(FirstSeenAttestationSourceConfig{
		BasePath:   base,
		Network:    "mainnet",
		DeadlineMS: 12000,
		CacheDir:   t.TempDir(),
	})
	require.NoError(t, err)

	attestations, err := source.AttestationsForSlot(64, func(uint64) string { return "deneb" })
	require.NoError(t, err)
	require.Len(t, attestations, 2)

	require.Empty(t, attestations[0].AggregationBits)
	require.Nil(t, attestations[0].CommitteeBits)
	require.Equal(t, []uint64{10, 12}, attestations[0].AttestingIndices)
	require.Equal(t, uint64(64), attestations[0].Slot)
	require.Equal(t, uint64(0), attestations[0].Index)
	require.Equal(t, rootWithByte(0x11), attestations[0].BeaconBlockRoot)
	require.Equal(t, uint64(1), attestations[0].SourceEpoch)
	require.Equal(t, rootWithByte(0x01), attestations[0].SourceRoot)
	require.Equal(t, uint64(2), attestations[0].TargetEpoch)
	require.Equal(t, rootWithByte(0x02), attestations[0].TargetRoot)

	require.Empty(t, attestations[1].AggregationBits)
	require.Nil(t, attestations[1].CommitteeBits)
	require.Equal(t, []uint64{10}, attestations[1].AttestingIndices)
	require.Equal(t, uint64(0), attestations[1].Index)
	require.Equal(t, rootWithByte(0x22), attestations[1].BeaconBlockRoot)

	_, err = source.AttestationsForSlot(65, func(uint64) string { return "deneb" })
	require.NoError(t, err)
}

func TestFirstSeenAttestationSourceDeduplicatesValidatorIndices(t *testing.T) {
	base := writeFirstSeenFixture(t, "mainnet", 2, []firstSeenParquetRow{
		firstSeenRow(64, 2, 10, "3", rootWithByte(0x11), 1000),
		firstSeenRow(64, 2, 10, "3", rootWithByte(0x11), 1001),
		firstSeenRow(64, 2, 9, "3", rootWithByte(0x11), 1002),
	})
	source, err := NewFirstSeenAttestationSource(FirstSeenAttestationSourceConfig{
		BasePath:   base,
		Network:    "mainnet",
		DeadlineMS: 12000,
		CacheDir:   t.TempDir(),
	})
	require.NoError(t, err)

	attestations, err := source.AttestationsForSlot(64, func(uint64) string { return "electra" })
	require.NoError(t, err)
	require.Len(t, attestations, 1)
	require.Equal(t, []uint64{9, 10}, attestations[0].AttestingIndices)
}

func writeFirstSeenFixture(t *testing.T, network string, epoch uint64, rows []firstSeenParquetRow) string {
	t.Helper()

	base := t.TempDir()
	dir := filepath.Join(base, firstSeenEpochRelativePath(network, epoch))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(dir, rows); err != nil {
		t.Fatal(err)
	}
	return base
}

func firstSeenRow(slot, epoch uint32, validator uint32, committee string, blockRoot [32]byte, rawSeenMS uint32) firstSeenParquetRow {
	return firstSeenParquetRow{
		Slot:           slot,
		Epoch:          epoch,
		ValidatorIndex: validator,
		CommitteeIndex: committee,
		BlockRoot:      formatRoot(blockRoot),
		SourceEpoch:    1,
		SourceRoot:     formatRoot(rootWithByte(0x01)),
		TargetEpoch:    2,
		TargetRoot:     formatRoot(rootWithByte(0x02)),
		RawSeenMS:      rawSeenMS,
	}
}

func rootWithByte(value byte) [32]byte {
	var root [32]byte
	root[31] = value
	return root
}
