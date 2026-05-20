package beaconapi

import (
	"errors"
	"fmt"
	"math"

	"github.com/ethpandaops/fcr-simulator/pkg/attplan"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/blockarchive"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
)

type RealBackendConfig struct {
	EraReader *era.Reader
	Fetcher   *beaconfetch.Fetcher

	GenesisInfo  GenesisInfo
	ForkSchedule ForkSchedule

	Mode         attplan.Mode
	LookaheadCap uint64

	// CheckpointBlocksByRoot is built by the orchestrator during worker setup.
	// The HTTP server only performs this lookup; it does not compute roots.
	CheckpointBlocksByRoot map[[32]byte][]byte

	BlockArchive *blockarchive.Client
}

type realBackend struct {
	cfg RealBackendConfig
}

func NewRealBackend(cfg RealBackendConfig) Backend {
	if cfg.ForkSchedule.SlotFork == nil {
		cfg.ForkSchedule.SlotFork = MainnetForkAtSlot
	}
	return &realBackend{cfg: cfg}
}

func (b *realBackend) BlockSSZBySlot(slot uint64) ([]byte, error) {
	if b.cfg.EraReader == nil {
		return nil, fmt.Errorf("era reader is not configured")
	}

	data, ok, err := b.cfg.EraReader.RawBlockSSZ(slot)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}

	return data, nil
}

func (b *realBackend) BlockSSZByRoot(root [32]byte) ([]byte, error) {
	if b.cfg.CheckpointBlocksByRoot != nil {
		if data, ok := b.cfg.CheckpointBlocksByRoot[root]; ok {
			return cloneBytes(data), nil
		}
	}

	if b.cfg.BlockArchive == nil {
		return nil, ErrNotFound
	}

	data, err := b.cfg.BlockArchive.FetchBlockSSZByRoot(root)
	if errors.Is(err, blockarchive.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return cloneBytes(data), nil
}

func (b *realBackend) StateSSZBySlot(slot uint64) ([]byte, error) {
	if b.cfg.Fetcher == nil {
		return nil, fmt.Errorf("beacon fetcher is not configured")
	}

	data, err := b.cfg.Fetcher.FetchStateSSZAtSlot(slot)
	return mapBeaconFetchResult(data, err)
}

func (b *realBackend) GenesisStateSSZ() ([]byte, error) {
	if b.cfg.Fetcher == nil {
		return nil, fmt.Errorf("beacon fetcher is not configured")
	}

	data, err := b.cfg.Fetcher.FetchGenesisStateSSZ()
	return mapBeaconFetchResult(data, err)
}

func (b *realBackend) GenesisInfo() (GenesisInfo, error) {
	return b.cfg.GenesisInfo, nil
}

func (b *realBackend) ConsensusVersionAtSlot(slot uint64) string {
	if b.cfg.ForkSchedule.SlotFork == nil {
		return MainnetForkAtSlot(slot)
	}
	return b.cfg.ForkSchedule.SlotFork(slot)
}

func (b *realBackend) BuildPlan(from, to uint64) ([]PlanEntry, error) {
	if b.cfg.EraReader == nil {
		return nil, fmt.Errorf("era reader is not configured")
	}
	if from > to {
		return nil, fmt.Errorf("from %d is after to %d", from, to)
	}

	blockExists := make(map[uint64]bool)
	if start, ok := checkedAdd(from, 1); ok {
		end := saturatingAdd(to, b.cfg.LookaheadCap)
		for slot := start; slot <= end; slot++ {
			exists, err := b.cfg.EraReader.BlockExists(slot)
			if err != nil {
				return nil, err
			}
			if exists {
				blockExists[slot] = true
			}
			if slot == math.MaxUint64 {
				break
			}
		}
	}

	entries, err := attplan.Plan(blockExists, from, to, b.cfg.Mode, b.cfg.LookaheadCap)
	if err != nil {
		return nil, err
	}

	out := make([]PlanEntry, len(entries))
	for i, entry := range entries {
		out[i] = PlanEntry{
			SimSlot:         entry.SimSlot,
			SourceBlockSlot: entry.SourceBlockSlot,
		}
	}
	return out, nil
}

func mapBeaconFetchResult(data []byte, err error) ([]byte, error) {
	if errors.Is(err, beaconfetch.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func checkedAdd(value, delta uint64) (uint64, bool) {
	if delta > math.MaxUint64-value {
		return 0, false
	}
	return value + delta, true
}

func saturatingAdd(value, delta uint64) uint64 {
	if delta > math.MaxUint64-value {
		return math.MaxUint64
	}
	return value + delta
}

func cloneBytes(data []byte) []byte {
	if data == nil {
		return nil
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out
}
