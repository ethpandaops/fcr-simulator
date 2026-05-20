package beaconapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethpandaops/fcr-simulator/pkg/attplan"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/blockarchive"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
	"github.com/golang/snappy"
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

	entries, err = backend.BuildPlan(104, 100)
	require.Nil(t, entries)
	require.Error(t, err)
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

	var buf bytes.Buffer
	writeTestRecord(t, &buf, 0x3265, nil)
	for _, slot := range slots {
		writeTestRecord(t, &buf, 0x0001, snappyCompress(t, encodeTestSignedBeaconBlock(slot)))
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
	const (
		signedBeaconBlockMessageOffset = 100
		slotSize                       = 8
	)

	ssz := make([]byte, signedBeaconBlockMessageOffset+slotSize)
	binary.LittleEndian.PutUint32(ssz[0:4], signedBeaconBlockMessageOffset)
	binary.LittleEndian.PutUint64(ssz[signedBeaconBlockMessageOffset:signedBeaconBlockMessageOffset+slotSize], slot)
	return ssz
}
