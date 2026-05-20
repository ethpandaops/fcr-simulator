package beaconapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/fcr-simulator/pkg/attplan"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/blockarchive"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
	"github.com/golang/snappy"
	bitfield "github.com/prysmaticlabs/go-bitfield"
	"github.com/stretchr/testify/require"
)

func TestRealBackendBlockSlotRootAndPlan(t *testing.T) {
	cacheDir := t.TempDir()
	writeTestEraFile(t, cacheDir, 1, 101, 103, 104)
	reader, err := era.New(cacheDir)
	require.NoError(t, err)

	root := testRoot(0x22)
	checkpointBlocks := map[[32]byte][]byte{
		root: []byte("checkpoint-block"),
	}
	backend := NewRealBackend(RealBackendConfig{
		EraReader:              reader,
		Mode:                   attplan.ModeNextNonMissed,
		LookaheadCap:           4,
		CheckpointBlocksByRoot: checkpointBlocks,
	})
	require.Equal(t, "phase0", backend.ConsensusVersionAtSlot(0))

	block, err := backend.BlockSSZBySlot(101)
	require.NoError(t, err)
	require.Equal(t, encodeTestSignedBeaconBlock(101), block)

	missed, err := backend.BlockSSZBySlot(102)
	require.Nil(t, missed)
	require.ErrorIs(t, err, ErrNotFound)

	byRoot, err := backend.BlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("checkpoint-block"), byRoot)
	byRoot[0] = 'X'
	byRootAgain, err := backend.BlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("checkpoint-block"), byRootAgain)

	entries, err := backend.BuildPlan(100, 104)
	require.NoError(t, err)
	requirePlanEntries(t, entries, 100, []*uint64{
		sourceSlot(101),
		sourceSlot(103),
		sourceSlot(103),
		sourceSlot(104),
	})
	require.Empty(t, entries[0].ImportBlocks)
	require.Len(t, entries[1].ImportBlocks, 1)
	require.True(t, entries[1].ImportBlocks[0].Canonical)
	require.Equal(t, uint64(101), entries[1].ImportBlocks[0].Slot)
	require.Len(t, entries[1].AttestationSources, 1)
	require.Equal(t, uint64(103), entries[1].AttestationSources[0].Slot)
	require.Nil(t, entries[1].AttestationSources[0].MaxAttestationSlot)

	entries, err = backend.BuildPlan(104, 100)
	require.Nil(t, entries)
	require.Error(t, err)
}

func TestRealBackendGreedyPlanIncludesWindowSourcesAndOrphanImports(t *testing.T) {
	cacheDir := t.TempDir()

	orphanParentSSZ := encodeTestSignedBeaconBlockWithAttestations(100, [32]byte{}, nil)
	orphanParentRoot := testBlockRoot(t, orphanParentSSZ)
	orphanHeadSSZ := encodeTestSignedBeaconBlockWithAttestations(101, orphanParentRoot, nil)
	orphanHeadRoot := testBlockRoot(t, orphanHeadSSZ)

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	source102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(101, orphanHeadRoot, orphanParentRoot),
		testAttestation(103, testRoot(0x88), testRoot(0x99)),
	})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, source102)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphanParentRoot)+".ssz"), orphanParentSSZ, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphanHeadRoot)+".ssz"), orphanHeadSSZ, 0o644))

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader:    reader,
		Mode:         attplan.ModeGreedyLookahead,
		LookaheadCap: 2,
		BlockArchive: archiveClient,
	})

	entries, err := backend.BuildPlan(101, 102)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, uint64(101), entries[0].SimSlot)
	require.Equal(t, uint64(102), entries[0].EvalSlot)
	require.Len(t, entries[0].AttestationSources, 1)
	require.Equal(t, uint64(102), entries[0].AttestationSources[0].Slot)
	require.NotNil(t, entries[0].AttestationSources[0].MaxAttestationSlot)
	require.Equal(t, uint64(102), *entries[0].AttestationSources[0].MaxAttestationSlot)

	require.Len(t, entries[0].ImportBlocks, 2)
	require.Equal(t, PlanBlockImport{Slot: 101, Root: formatRoot(canonical101Root), Canonical: true}, entries[0].ImportBlocks[0])
	require.Equal(t, PlanBlockImport{Slot: 101, Root: formatRoot(orphanHeadRoot), Canonical: false}, entries[0].ImportBlocks[1])
}

func TestRealBackendGreedyPlanSchedulesFutureSlotOrphanAtBlockSlot(t *testing.T) {
	cacheDir := t.TempDir()

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	orphan102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, nil)
	orphan102Root := testBlockRoot(t, orphan102)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(102, orphan102Root, orphan102Root),
	})
	canonical103 := encodeTestSignedBeaconBlockWithAttestations(103, testBlockRoot(t, canonical102), []*phase0.Attestation{
		testAttestation(102, orphan102Root, orphan102Root),
	})
	canonical102Root := testBlockRoot(t, canonical102)

	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102, canonical103)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphan102Root)+".ssz"), orphan102, 0o644))

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader:    reader,
		Mode:         attplan.ModeGreedyLookahead,
		LookaheadCap: 2,
		BlockArchive: archiveClient,
	})

	entries, err := backend.BuildPlan(101, 103)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Len(t, entries[0].ImportBlocks, 1)
	require.Equal(t, PlanBlockImport{Slot: 101, Root: formatRoot(canonical101Root), Canonical: true}, entries[0].ImportBlocks[0])

	require.Len(t, entries[1].ImportBlocks, 2)
	require.Equal(t, PlanBlockImport{Slot: 102, Root: formatRoot(canonical102Root), Canonical: true}, entries[1].ImportBlocks[0])
	require.Equal(t, PlanBlockImport{Slot: 102, Root: formatRoot(orphan102Root), Canonical: false}, entries[1].ImportBlocks[1])
	require.Len(t, entries[1].AttestationSources, 1)
	require.Equal(t, uint64(103), entries[1].AttestationSources[0].Slot)
}

func TestRealBackendFetcherAndGenesisInfo(t *testing.T) {
	cacheDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cacheDir, "states"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "states", "state-100.ssz"), []byte("state-100"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "states", "state-genesis.ssz"), []byte("genesis-state"), 0o644))

	fetcher, err := beaconfetch.New("http://example.invalid", cacheDir)
	require.NoError(t, err)

	info := GenesisInfo{
		GenesisTime:           1,
		GenesisValidatorsRoot: "0xaaaa",
		GenesisForkVersion:    "0x00000000",
	}
	backend := NewRealBackend(RealBackendConfig{
		Fetcher:     fetcher,
		GenesisInfo: info,
		ForkSchedule: ForkSchedule{
			SlotFork: func(uint64) string { return "custom" },
		},
	})

	state, err := backend.StateSSZBySlot(100)
	require.NoError(t, err)
	require.Equal(t, []byte("state-100"), state)

	genesisState, err := backend.GenesisStateSSZ()
	require.NoError(t, err)
	require.Equal(t, []byte("genesis-state"), genesisState)

	gotInfo, err := backend.GenesisInfo()
	require.NoError(t, err)
	require.Equal(t, info, gotInfo)
	require.Equal(t, "custom", backend.ConsensusVersionAtSlot(1))

	missing, err := mapBeaconFetchResult(nil, beaconfetch.ErrNotFound)
	require.Nil(t, missing)
	require.ErrorIs(t, err, ErrNotFound)

	data, err := mapBeaconFetchResult([]byte("x"), nil)
	require.NoError(t, err)
	require.Equal(t, []byte("x"), data)
}

func TestRealBackendConfigurationErrors(t *testing.T) {
	backend := NewRealBackend(RealBackendConfig{})

	block, err := backend.BlockSSZBySlot(1)
	require.Nil(t, block)
	require.Error(t, err)

	state, err := backend.StateSSZBySlot(1)
	require.Nil(t, state)
	require.Error(t, err)

	genesis, err := backend.GenesisStateSSZ()
	require.Nil(t, genesis)
	require.Error(t, err)

	plan, err := backend.BuildPlan(1, 2)
	require.Nil(t, plan)
	require.Error(t, err)

	blockByRoot, err := backend.BlockSSZByRoot(testRoot(0x33))
	require.Nil(t, blockByRoot)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRealBackendBlockSSZByRootFallsBackToBlockArchive(t *testing.T) {
	root := testRoot(0x44)
	rootText := rootHex(root)
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, rootText+".ssz"), []byte("archive-block"), 0o644))

	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", cacheDir)
	require.NoError(t, err)

	backend := NewRealBackend(RealBackendConfig{
		CheckpointBlocksByRoot: map[[32]byte][]byte{},
		BlockArchive:           archiveClient,
	})
	block, err := backend.BlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("archive-block"), block)
}

func TestRealBackendUtilityEdgeCases(t *testing.T) {
	value, ok := checkedAdd(^uint64(0), 1)
	require.False(t, ok)
	require.Zero(t, value)

	require.Equal(t, ^uint64(0), saturatingAdd(^uint64(0)-1, 10))
	require.Nil(t, cloneBytes(nil))

	data, err := mapBeaconFetchResult(nil, errFakeBackend)
	require.Nil(t, data)
	require.ErrorIs(t, err, errFakeBackend)
}

func requirePlanEntries(t *testing.T, got []PlanEntry, simStart uint64, expected []*uint64) {
	t.Helper()

	require.Len(t, got, len(expected))
	for i, entry := range got {
		require.Equal(t, simStart+uint64(i), entry.SimSlot)
		if expected[i] == nil {
			require.Nil(t, entry.SourceBlockSlot)
			continue
		}
		require.NotNil(t, entry.SourceBlockSlot)
		require.Equal(t, *expected[i], *entry.SourceBlockSlot)
	}
}

func sourceSlot(slot uint64) *uint64 {
	return &slot
}

func writeTestEraFile(t *testing.T, dir string, eraNumber uint64, slots ...uint64) {
	t.Helper()

	blocks := make([][]byte, 0, len(slots))
	for _, slot := range slots {
		blocks = append(blocks, encodeTestSignedBeaconBlock(slot))
	}
	writeTestEraFileWithBlocks(t, dir, eraNumber, blocks...)
}

func writeTestEraFileWithBlocks(t *testing.T, dir string, eraNumber uint64, blocks ...[]byte) {
	t.Helper()

	var buf bytes.Buffer
	writeTestRecord(t, &buf, 0x3265, nil)
	for _, block := range blocks {
		writeTestRecord(t, &buf, 0x0001, snappyCompress(t, block))
	}

	path := filepath.Join(dir, fmt.Sprintf("mainnet-%05d-deadbeef.era", eraNumber))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func writeTestRecord(t *testing.T, buf *bytes.Buffer, recordType uint16, data []byte) {
	t.Helper()

	var header [8]byte
	binary.LittleEndian.PutUint16(header[0:2], recordType)
	binary.LittleEndian.PutUint32(header[2:6], uint32(len(data)))
	_, err := buf.Write(header[:])
	require.NoError(t, err)
	_, err = buf.Write(data)
	require.NoError(t, err)
}

func snappyCompress(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := snappy.NewBufferedWriter(&buf)
	_, err := writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buf.Bytes()
}

func encodeTestSignedBeaconBlock(slot uint64) []byte {
	return encodeTestSignedBeaconBlockWithAttestations(slot, [32]byte{}, nil)
}

func encodeTestSignedBeaconBlockWithAttestations(slot uint64, parentRoot [32]byte, attestations []*phase0.Attestation) []byte {
	block := &phase0.SignedBeaconBlock{
		Message: &phase0.BeaconBlock{
			Slot:          phase0.Slot(slot),
			ProposerIndex: 1,
			ParentRoot:    phase0.Root(parentRoot),
			StateRoot:     phase0.Root(testRoot(byte(slot))),
			Body: &phase0.BeaconBlockBody{
				ETH1Data: &phase0.ETH1Data{
					BlockHash: make([]byte, 32),
				},
				ProposerSlashings: []*phase0.ProposerSlashing{},
				AttesterSlashings: []*phase0.AttesterSlashing{},
				Attestations:      attestations,
				Deposits:          []*phase0.Deposit{},
				VoluntaryExits:    []*phase0.SignedVoluntaryExit{},
			},
		},
	}
	ssz, err := block.MarshalSSZ()
	if err != nil {
		panic(err)
	}
	return ssz
}

func testAttestation(slot uint64, headRoot, targetRoot [32]byte) *phase0.Attestation {
	bits := bitfield.NewBitlist(1)
	bits.SetBitAt(0, true)
	return &phase0.Attestation{
		AggregationBits: bits,
		Data: &phase0.AttestationData{
			Slot:            phase0.Slot(slot),
			BeaconBlockRoot: phase0.Root(headRoot),
			Source:          &phase0.Checkpoint{},
			Target: &phase0.Checkpoint{
				Epoch: phase0.Epoch(slot / 32),
				Root:  phase0.Root(targetRoot),
			},
		},
	}
}

func testBlockRoot(t *testing.T, ssz []byte) [32]byte {
	t.Helper()

	info, err := parseBlockInfo(ssz, MainnetForkAtSlot)
	require.NoError(t, err)
	return info.Root
}
