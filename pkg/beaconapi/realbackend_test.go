package beaconapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
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

func TestRealBackendBuildSlotEmitsInlineAttestationsAndOrphans(t *testing.T) {
	cacheDir := t.TempDir()

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(101, canonical101Root, canonical101Root),
	})
	canonical102Root := testBlockRoot(t, canonical102)
	orphan102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, nil)
	orphan102Root := testBlockRoot(t, orphan102)
	orphanVote := testAttestation(102, orphan102Root, orphan102Root)
	canonical103 := encodeTestSignedBeaconBlockWithAttestations(103, canonical102Root, []*phase0.Attestation{
		orphanVote,
	})
	canonical103Root := testBlockRoot(t, canonical103)
	canonical104 := encodeTestSignedBeaconBlockWithAttestations(104, canonical103Root, []*phase0.Attestation{
		orphanVote,
	})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102, canonical103, canonical104)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphan102Root)+".ssz"), orphan102, 0o644))

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader:    reader,
		LookaheadCap: 2,
		BlockArchive: archiveClient,
	})

	firstInstruction, err := backend.BuildSlot(101, 100)
	require.NoError(t, err)
	require.Len(t, firstInstruction.Attestations, 1)
	require.Equal(t, uint64(101), firstInstruction.Attestations[0].Data.Slot)
	require.Equal(t, formatRoot(canonical101Root), firstInstruction.Attestations[0].Data.BeaconBlockRoot)

	instruction, err := backend.BuildSlot(102, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(102), instruction.SimSlot)
	require.Equal(t, uint64(103), instruction.EvalSlot)
	require.Equal(t, []PlanBlockImport{
		{Slot: 102, Root: formatRoot(canonical102Root), Canonical: true},
		{Slot: 102, Root: formatRoot(orphan102Root), Canonical: false},
	}, instruction.ImportBlocks)
	require.Len(t, instruction.Attestations, 1)
	for _, attestation := range instruction.Attestations {
		require.Equal(t, uint64(102), attestation.Data.Slot)
		require.Equal(t, formatRoot(orphan102Root), attestation.Data.BeaconBlockRoot)
		require.Equal(t, formatRoot(orphan102Root), attestation.Data.Target.Root)
		require.NotEmpty(t, attestation.AggregationBits)
		require.Nil(t, attestation.CommitteeBits)
	}
}

func TestRealBackendBuildSlotUnionsOrphanSourceBlockAttestations(t *testing.T) {
	cacheDir := t.TempDir()

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, nil)
	canonical102Root := testBlockRoot(t, canonical102)
	canonical103 := encodeTestSignedBeaconBlockWithAttestations(103, canonical102Root, nil)
	orphanSource103 := encodeTestSignedBeaconBlockWithAttestations(103, canonical102Root, []*phase0.Attestation{
		testAttestation(102, canonical102Root, canonical102Root),
	})
	orphanSource103Root := testBlockRoot(t, orphanSource103)
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102, canonical103)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/api/v1/index":
			require.Equal(t, "mainnet", r.URL.Query().Get("network"))
			require.NotEmpty(t, r.URL.Query().Get("slot_min"))
			require.NotEmpty(t, r.URL.Query().Get("slot_max"))
			switch r.URL.Query().Get("offset") {
			case "0":
				fmt.Fprintf(w, `{"index":[{"slot":103,"block_root":"%s"}]}`, formatRoot(orphanSource103Root))
			case "1":
				fmt.Fprint(w, `{"index":[]}`)
			default:
				t.Fatalf("unexpected archive index offset %s", r.URL.Query().Get("offset"))
			}
		case "/mainnet/103/" + formatRoot(orphanSource103Root) + ".ssz":
			_, err := w.Write(orphanSource103)
			require.NoError(t, err)
		default:
			t.Fatalf("unexpected archive path %s", r.URL.String())
		}
	}))
	defer server.Close()

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveDir := filepath.Join(cacheDir, "archive")
	archiveClient, err := blockarchive.New(server.URL, "mainnet", archiveDir)
	require.NoError(t, err)
	warm, err := WarmBlockArchiveCache(RealBackendConfig{
		EraReader:    reader,
		Mode:         attplan.ModeGreedyLookahead,
		LookaheadCap: 2,
		BlockArchive: archiveClient,
		ForkSchedule: ForkSchedule{SlotFork: MainnetForkAtSlot},
	}, [][2]uint64{{101, 104}})
	require.NoError(t, err)
	require.Equal(t, [][32]byte{orphanSource103Root}, warm.OrphanRootsBySlot[103])
	require.Equal(t, 1, warm.Stats.SlotRangeQueries)
	require.Equal(t, 1, warm.Stats.OrphanRootsDiscovered)
	require.FileExists(t, filepath.Join(archiveDir, formatRoot(orphanSource103Root)+".ssz"))

	afterWarmRequests := requests.Load()
	cacheOnlyArchive, err := blockarchive.New(server.URL, "mainnet", archiveDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader:                reader,
		Mode:                     attplan.ModeGreedyLookahead,
		LookaheadCap:             2,
		BlockArchive:             cacheOnlyArchive,
		ArchiveCacheOnly:         true,
		ArchiveOrphanRootsBySlot: warm.OrphanRootsBySlot,
		ForkSchedule:             ForkSchedule{SlotFork: MainnetForkAtSlot},
	})

	instruction, err := backend.BuildSlot(102, 100)
	require.NoError(t, err)
	require.Equal(t, afterWarmRequests, requests.Load(), "BuildSlot must not contact archive HTTP")
	require.Len(t, instruction.Attestations, 1)
	require.Equal(t, uint64(102), instruction.Attestations[0].Data.Slot)
	require.Equal(t, formatRoot(canonical102Root), instruction.Attestations[0].Data.BeaconBlockRoot)
	require.Equal(t, formatRoot(canonical102Root), instruction.Attestations[0].Data.Target.Root)
}

func TestRealBackendBuildSlotDedupesExactAggregatesPreservesDistinctBits(t *testing.T) {
	cacheDir := t.TempDir()

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, nil)
	canonical102Root := testBlockRoot(t, canonical102)
	duplicate := testAttestationWithAggregationBits(102, canonical102Root, canonical102Root, 2, 0)
	distinctBits := testAttestationWithAggregationBits(102, canonical102Root, canonical102Root, 2, 1)
	canonical103 := encodeTestSignedBeaconBlockWithAttestations(103, canonical102Root, []*phase0.Attestation{
		duplicate,
	})
	orphan103 := encodeTestSignedBeaconBlockWithAttestations(103, canonical102Root, []*phase0.Attestation{
		duplicate,
		distinctBits,
	})
	orphan103Root := testBlockRoot(t, orphan103)
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102, canonical103)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, formatRoot(orphan103Root)+".ssz"), orphan103, 0o644))

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader:                reader,
		Mode:                     attplan.ModeGreedyLookahead,
		LookaheadCap:             2,
		BlockArchive:             archiveClient,
		ArchiveCacheOnly:         true,
		ArchiveOrphanRootsBySlot: map[uint64][][32]byte{103: [][32]byte{orphan103Root}},
	})

	instruction, err := backend.BuildSlot(102, 100)
	require.NoError(t, err)
	require.Len(t, instruction.Attestations, 2)
	gotBits := map[string]bool{}
	for _, attestation := range instruction.Attestations {
		require.Equal(t, uint64(102), attestation.Data.Slot)
		require.Equal(t, formatRoot(canonical102Root), attestation.Data.BeaconBlockRoot)
		gotBits[attestation.AggregationBits] = true
	}
	require.Len(t, gotBits, 2)
}

func TestRealBackendBuildSlotDropsPreWarmupOrphanParents(t *testing.T) {
	cacheDir := t.TempDir()

	orphanParent100 := encodeTestSignedBeaconBlockWithAttestations(100, [32]byte{}, nil)
	orphanParent100Root := testBlockRoot(t, orphanParent100)
	orphanHead101 := encodeTestSignedBeaconBlockWithAttestations(101, orphanParent100Root, nil)
	orphanHead101Root := testBlockRoot(t, orphanHead101)
	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(101, orphanHead101Root, orphanHead101Root),
	})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphanParent100Root)+".ssz"), orphanParent100, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphanHead101Root)+".ssz"), orphanHead101, 0o644))

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader:    reader,
		LookaheadCap: 2,
		BlockArchive: archiveClient,
	})

	instruction, err := backend.BuildSlot(101, 100)
	require.NoError(t, err)
	require.Equal(t, []PlanBlockImport{
		{Slot: 101, Root: formatRoot(canonical101Root), Canonical: true},
		{Slot: 101, Root: formatRoot(orphanHead101Root), Canonical: false},
	}, instruction.ImportBlocks)
}

func TestRealBackendBuildSlotStrictModeUsesNextBlockWithZeroConfiguredLookahead(t *testing.T) {
	cacheDir := t.TempDir()

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(100, canonical101Root, canonical101Root),
		testAttestation(101, canonical101Root, canonical101Root),
	})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102)

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	backend := NewRealBackend(RealBackendConfig{
		EraReader: reader,
		Mode:      attplan.ModeStrictKMinus1,
	})

	instruction, err := backend.BuildSlot(101, 100)
	require.NoError(t, err)
	require.Len(t, instruction.Attestations, 2)
	require.Equal(t, []uint64{100, 101}, []uint64{
		instruction.Attestations[0].Data.Slot,
		instruction.Attestations[1].Data.Slot,
	})
}

func TestRealBackendBuildSlotWarmupScopeUnaffectedByIndexWarmth(t *testing.T) {
	cacheDir := t.TempDir()

	orphanParent100 := encodeTestSignedBeaconBlockWithAttestations(100, [32]byte{}, nil)
	orphanParent100Root := testBlockRoot(t, orphanParent100)
	orphanHead101 := encodeTestSignedBeaconBlockWithAttestations(101, orphanParent100Root, nil)
	orphanHead101Root := testBlockRoot(t, orphanHead101)
	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(101, orphanHead101Root, orphanHead101Root),
	})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphanParent100Root)+".ssz"), orphanParent100, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphanHead101Root)+".ssz"), orphanHead101, 0o644))

	newBackend := func(t *testing.T) Backend {
		t.Helper()
		reader, err := era.New(cacheDir)
		require.NoError(t, err)
		archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
		require.NoError(t, err)
		return NewRealBackend(RealBackendConfig{
			EraReader:    reader,
			LookaheadCap: 2,
			BlockArchive: archiveClient,
		})
	}

	warmed := newBackend(t)
	warmedWithEarlierWarmup, err := warmed.BuildSlot(101, 99)
	require.NoError(t, err)
	require.Contains(t, warmedWithEarlierWarmup.ImportBlocks, PlanBlockImport{Slot: 100, Root: formatRoot(orphanParent100Root), Canonical: false})

	got, err := warmed.BuildSlot(101, 100)
	require.NoError(t, err)
	want, err := newBackend(t).BuildSlot(101, 100)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []PlanBlockImport{
		{Slot: 101, Root: formatRoot(canonical101Root), Canonical: true},
		{Slot: 101, Root: formatRoot(orphanHead101Root), Canonical: false},
	}, got.ImportBlocks)
}

func TestRealBackendBuildSlotConcurrentMatchesFreshResults(t *testing.T) {
	cacheDir := t.TempDir()

	canonical101 := encodeTestSignedBeaconBlock(101)
	canonical101Root := testBlockRoot(t, canonical101)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, []*phase0.Attestation{
		testAttestation(101, canonical101Root, canonical101Root),
	})
	canonical102Root := testBlockRoot(t, canonical102)
	orphan102 := encodeTestSignedBeaconBlockWithAttestations(102, canonical101Root, nil)
	orphan102Root := testBlockRoot(t, orphan102)
	orphanVote := testAttestation(102, orphan102Root, orphan102Root)
	canonical103 := encodeTestSignedBeaconBlockWithAttestations(103, canonical102Root, []*phase0.Attestation{orphanVote})
	canonical103Root := testBlockRoot(t, canonical103)
	canonical104 := encodeTestSignedBeaconBlockWithAttestations(104, canonical103Root, []*phase0.Attestation{orphanVote})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical101, canonical102, canonical103, canonical104)

	archiveDir := filepath.Join(cacheDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, rootHex(orphan102Root)+".ssz"), orphan102, 0o644))

	newBackend := func(t *testing.T) Backend {
		t.Helper()
		reader, err := era.New(cacheDir)
		require.NoError(t, err)
		archiveClient, err := blockarchive.New("http://archive.test", "mainnet", archiveDir)
		require.NoError(t, err)
		return NewRealBackend(RealBackendConfig{
			EraReader:    reader,
			LookaheadCap: 2,
			BlockArchive: archiveClient,
		})
	}

	slots := []uint64{101, 102, 103}
	expected := make(map[uint64]SlotInstruction, len(slots))
	for _, slot := range slots {
		instruction, err := newBackend(t).BuildSlot(slot, 100)
		require.NoError(t, err)
		expected[slot] = instruction
	}

	shared := newBackend(t)
	var wg sync.WaitGroup
	errs := make(chan error, len(slots)*8)
	for i := 0; i < 8; i++ {
		for _, slot := range slots {
			wg.Add(1)
			go func(slot uint64) {
				defer wg.Done()
				got, err := shared.BuildSlot(slot, 100)
				if err != nil {
					errs <- err
					return
				}
				if !reflect.DeepEqual(got, expected[slot]) {
					errs <- fmt.Errorf("slot %d instruction mismatch", slot)
				}
			}(slot)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
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

func TestRealBackendBlockSSZByRootReadsBlockArchiveCache(t *testing.T) {
	root := testRoot(0x44)
	rootText := rootHex(root)
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, rootText+".ssz"), []byte("archive-block"), 0o644))

	archiveClient, err := blockarchive.New("http://archive.test", "mainnet", cacheDir)
	require.NoError(t, err)

	backend := NewRealBackend(RealBackendConfig{
		CheckpointBlocksByRoot: map[[32]byte][]byte{},
		BlockArchive:           archiveClient,
		ArchiveCacheOnly:       true,
	})
	block, err := backend.BlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("archive-block"), block)
}

func TestRealBackendBlockSSZByRootCacheOnlyDoesNotContactArchive(t *testing.T) {
	root := testRoot(0x45)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected archive request", http.StatusInternalServerError)
	}))
	defer server.Close()

	archiveClient, err := blockarchive.New(server.URL, "mainnet", t.TempDir())
	require.NoError(t, err)

	backend := NewRealBackend(RealBackendConfig{
		BlockArchive:     archiveClient,
		ArchiveCacheOnly: true,
	})
	block, err := backend.BlockSSZByRoot(root)
	require.Nil(t, block)
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, requests.Load())
}

func TestWarmBlockArchiveCacheRetriesAndSummarizes(t *testing.T) {
	cacheDir := t.TempDir()

	orphanParent100 := encodeTestSignedBeaconBlockWithAttestations(100, [32]byte{}, nil)
	orphanParent100Root := testBlockRoot(t, orphanParent100)
	orphanHead101 := encodeTestSignedBeaconBlockWithAttestations(101, orphanParent100Root, nil)
	orphanHead101Root := testBlockRoot(t, orphanHead101)
	missingRoot := testRoot(0x77)
	canonical102 := encodeTestSignedBeaconBlockWithAttestations(102, [32]byte{}, []*phase0.Attestation{
		testAttestation(101, orphanHead101Root, orphanHead101Root),
		testAttestation(101, missingRoot, missingRoot),
	})
	writeTestEraFileWithBlocks(t, cacheDir, 1, canonical102)

	var orphanHeadIndexRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/index":
			rootText := r.URL.Query().Get("block_root_prefix")
			switch rootText {
			case formatRoot(orphanHead101Root):
				if orphanHeadIndexRequests.Add(1) == 1 {
					http.Error(w, "temporary origin failure", http.StatusInternalServerError)
					return
				}
				fmt.Fprintf(w, `{"index":[{"slot":101,"block_root":"%s"}]}`, formatRoot(orphanHead101Root))
			case formatRoot(orphanParent100Root):
				fmt.Fprintf(w, `{"index":[{"slot":100,"block_root":"%s"}]}`, formatRoot(orphanParent100Root))
			case formatRoot(missingRoot):
				http.NotFound(w, r)
			default:
				t.Fatalf("unexpected archive index root %s", rootText)
			}
		case "/mainnet/101/" + formatRoot(orphanHead101Root) + ".ssz":
			_, err := w.Write(orphanHead101)
			require.NoError(t, err)
		case "/mainnet/100/" + formatRoot(orphanParent100Root) + ".ssz":
			_, err := w.Write(orphanParent100)
			require.NoError(t, err)
		default:
			t.Fatalf("unexpected archive path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	reader, err := era.New(cacheDir)
	require.NoError(t, err)
	archiveClient, err := blockarchive.New(server.URL, "mainnet", filepath.Join(cacheDir, "archive"))
	require.NoError(t, err)

	warm, err := WarmBlockArchiveCache(RealBackendConfig{
		EraReader:    reader,
		LookaheadCap: 1,
		BlockArchive: archiveClient,
		ForkSchedule: ForkSchedule{SlotFork: MainnetForkAtSlot},
	}, [][2]uint64{{101, 102}})
	require.NoError(t, err)
	require.Equal(t, ArchiveWarmStats{
		WorkerRanges:             1,
		OrphanRootsDiscovered:    2,
		RootsResolvedCached:      2,
		RootsArchiveMissing:      1,
		TransientFailuresRetried: 1,
		ChainsPreWarmupParent:    1,
	}, warm.Stats)
	require.Equal(t, int32(2), orphanHeadIndexRequests.Load())
	require.FileExists(t, filepath.Join(cacheDir, "archive", formatRoot(orphanHead101Root)+".ssz"))
	require.FileExists(t, filepath.Join(cacheDir, "archive", formatRoot(orphanParent100Root)+".ssz"))
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
	return testAttestationWithAggregationBits(slot, headRoot, targetRoot, 1, 0)
}

func testAttestationWithAggregationBits(slot uint64, headRoot, targetRoot [32]byte, bitLen uint64, setBits ...uint64) *phase0.Attestation {
	bits := bitfield.NewBitlist(bitLen)
	for _, bit := range setBits {
		bits.SetBitAt(bit, true)
	}
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
