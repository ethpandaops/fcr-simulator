package beaconapi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"
)

func TestFirstSeenAttestationSourceEncodesPreElectraStandardSingleAttestations(t *testing.T) {
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
		CommitteeProvider: stubCommitteeProvider{
			committees: map[uint64][]beaconfetch.BeaconCommittee{
				2: {
					{Slot: 64, Index: 3, Validators: []uint64{9, 10, 12}},
				},
			},
		},
	})
	require.NoError(t, err)

	attestations, err := source.AttestationsForSlot(64, func(uint64) string { return "deneb" })
	require.NoError(t, err)
	require.Len(t, attestations, 3)

	require.Equal(t, "0x0a", attestations[0].AggregationBits)
	require.Nil(t, attestations[0].CommitteeBits)
	require.Empty(t, attestations[0].AttestingIndices)
	require.Equal(t, uint64(64), attestations[0].Slot)
	require.Equal(t, uint64(3), attestations[0].Index)
	require.Equal(t, rootWithByte(0x11), attestations[0].BeaconBlockRoot)
	require.Equal(t, uint64(1), attestations[0].SourceEpoch)
	require.Equal(t, rootWithByte(0x01), attestations[0].SourceRoot)
	require.Equal(t, uint64(2), attestations[0].TargetEpoch)
	require.Equal(t, rootWithByte(0x02), attestations[0].TargetRoot)

	require.Equal(t, "0x0a", attestations[1].AggregationBits)
	require.Nil(t, attestations[1].CommitteeBits)
	require.Empty(t, attestations[1].AttestingIndices)
	require.Equal(t, uint64(3), attestations[1].Index)
	require.Equal(t, rootWithByte(0x22), attestations[1].BeaconBlockRoot)

	require.Equal(t, "0x0c", attestations[2].AggregationBits)
	require.Nil(t, attestations[2].CommitteeBits)
	require.Empty(t, attestations[2].AttestingIndices)
	require.Equal(t, uint64(3), attestations[2].Index)
	require.Equal(t, rootWithByte(0x11), attestations[2].BeaconBlockRoot)

	_, err = source.AttestationsForSlot(65, func(uint64) string { return "deneb" })
	require.NoError(t, err)
}

func TestFirstSeenAttestationSourceEncodesElectraStandardSingleAttestation(t *testing.T) {
	base := writeFirstSeenFixture(t, "mainnet", 2, []firstSeenParquetRow{
		firstSeenRow(64, 2, 101, "5", rootWithByte(0x11), 1000),
	})
	source, err := NewFirstSeenAttestationSource(FirstSeenAttestationSourceConfig{
		BasePath:   base,
		Network:    "mainnet",
		DeadlineMS: 12000,
		CacheDir:   t.TempDir(),
		CommitteeProvider: stubCommitteeProvider{
			committees: map[uint64][]beaconfetch.BeaconCommittee{
				2: {
					{Slot: 64, Index: 5, Validators: []uint64{100, 101, 102}},
				},
			},
		},
	})
	require.NoError(t, err)

	attestations, err := source.AttestationsForSlot(64, func(uint64) string { return "electra" })
	require.NoError(t, err)
	require.Len(t, attestations, 1)
	require.Equal(t, uint64(0), attestations[0].Index)
	require.Equal(t, "0x0a", attestations[0].AggregationBits)
	require.NotNil(t, attestations[0].CommitteeBits)
	require.Equal(t, "0x2000000000000000", *attestations[0].CommitteeBits)
	require.Empty(t, attestations[0].AttestingIndices)
}

func TestFirstSeenAttestationSourceDeduplicatesExactValidatorVotesDeterministically(t *testing.T) {
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
		CommitteeProvider: stubCommitteeProvider{
			committees: map[uint64][]beaconfetch.BeaconCommittee{
				2: {
					{Slot: 64, Index: 3, Validators: []uint64{9, 10}},
				},
			},
		},
	})
	require.NoError(t, err)

	attestations, err := source.AttestationsForSlot(64, func(uint64) string { return "deneb" })
	require.NoError(t, err)
	require.Len(t, attestations, 2)
	require.Equal(t, "0x05", attestations[0].AggregationBits)
	require.Equal(t, rootWithByte(0x11), attestations[0].BeaconBlockRoot)
	require.Equal(t, "0x06", attestations[1].AggregationBits)
	require.Equal(t, rootWithByte(0x11), attestations[1].BeaconBlockRoot)
}

func TestFirstSeenAttestationSourceDropsAndLogsLowRatePhantomRows(t *testing.T) {
	rows := make([]firstSeenParquetRow, 0, 101)
	validators := make([]uint64, 0, 100)
	for i := 0; i < 100; i++ {
		validator := uint32(1000 + i)
		rows = append(rows, firstSeenRow(64, 2, validator, "3", rootWithByte(0x11), 1000))
		validators = append(validators, uint64(validator))
	}
	rows = append(rows, firstSeenRow(64, 2, 9999, "3", rootWithByte(0x11), 1000))

	base := writeFirstSeenFixture(t, "mainnet", 2, rows)
	var logs bytes.Buffer
	source, err := NewFirstSeenAttestationSource(FirstSeenAttestationSourceConfig{
		BasePath:   base,
		Network:    "mainnet",
		DeadlineMS: 12000,
		CacheDir:   t.TempDir(),
		CommitteeProvider: stubCommitteeProvider{
			committees: map[uint64][]beaconfetch.BeaconCommittee{
				2: {
					{Slot: 64, Index: 3, Validators: validators},
				},
			},
		},
		LogWriter: &logs,
	})
	require.NoError(t, err)

	attestations, err := source.AttestationsForSlot(64, func(uint64) string { return "deneb" })
	require.NoError(t, err)
	require.Len(t, attestations, 100)
	require.Equal(t, mustAggregationBits(t, 100, 0), attestations[0].AggregationBits)
	require.Equal(t, mustAggregationBits(t, 100, 99), attestations[99].AggregationBits)
	require.Contains(t, logs.String(), "first-seen: dropped 1/101 phantom rows (0.9901%) with no committee assignment for epoch 2")
}

func TestFirstSeenAttestationSourceRejectsHighRatePhantomRows(t *testing.T) {
	base := writeFirstSeenFixture(t, "mainnet", 2, []firstSeenParquetRow{
		firstSeenRow(64, 2, 10, "3", rootWithByte(0x11), 1000),
		firstSeenRow(64, 2, 12, "3", rootWithByte(0x11), 1000),
	})
	source, err := NewFirstSeenAttestationSource(FirstSeenAttestationSourceConfig{
		BasePath:   base,
		Network:    "mainnet",
		DeadlineMS: 12000,
		CacheDir:   t.TempDir(),
		CommitteeProvider: stubCommitteeProvider{
			committees: map[uint64][]beaconfetch.BeaconCommittee{
				2: {
					{Slot: 64, Index: 3, Validators: []uint64{10}},
				},
			},
		},
	})
	require.NoError(t, err)

	_, err = source.AttestationsForSlot(64, func(uint64) string { return "deneb" })
	require.Error(t, err)
	require.Contains(t, err.Error(), "phantom row fraction 50.0000% exceeds 1.0000% guardrail")
	require.Contains(t, err.Error(), "1/2 rows have no committee assignment")
}

type stubCommitteeProvider struct {
	committees map[uint64][]beaconfetch.BeaconCommittee
}

func (p stubCommitteeProvider) FetchBeaconCommittees(epoch uint64) ([]beaconfetch.BeaconCommittee, error) {
	return p.committees[epoch], nil
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

func mustAggregationBits(t *testing.T, committeeSize, position uint64) string {
	t.Helper()

	bits, err := singleValidatorAggregationBits(committeeSize, position)
	require.NoError(t, err)
	return bits
}
