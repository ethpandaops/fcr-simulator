package beaconapi

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ethpandaops/fcr-simulator/pkg/attplan"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/blockarchive"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
)

const maxOrphanWalk = 16

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
	if (b.cfg.Mode == attplan.ModeNextNonMissed || b.cfg.Mode == attplan.ModeGreedyLookahead) && b.cfg.LookaheadCap < 1 {
		return nil, fmt.Errorf("lookaheadCap must be >= 1 for mode %d", b.cfg.Mode)
	}
	if b.cfg.Mode != attplan.ModeNextNonMissed && b.cfg.Mode != attplan.ModeStrictKMinus1 && b.cfg.Mode != attplan.ModeGreedyLookahead {
		return nil, fmt.Errorf("unsupported attestation source mode %d", b.cfg.Mode)
	}

	loadStart := from
	if from > 0 {
		loadStart = from - 1
	}
	loadEnd := saturatingAdd(to, b.cfg.LookaheadCap)
	canonicalBySlot, canonicalByRoot, err := b.loadCanonicalBlockInfos(loadStart, loadEnd)
	if err != nil {
		return nil, err
	}

	blockExists := make(map[uint64]bool, len(canonicalBySlot))
	for slot := range canonicalBySlot {
		blockExists[slot] = true
	}

	state := &planBuildState{
		backend:         b,
		canonicalBySlot: canonicalBySlot,
		canonicalByRoot: canonicalByRoot,
		importedRoots:   make(map[[32]byte]bool),
		scheduledRoots:  make(map[[32]byte]bool),
		missingRoots:    make(map[[32]byte]bool),
		ignoredRoots:    make(map[[32]byte]bool),
		fetchedByRoot:   make(map[[32]byte]blockInfo),
	}
	for root := range b.cfg.CheckpointBlocksByRoot {
		state.importedRoots[root] = true
	}
	if from > 0 {
		if info, ok := canonicalBySlot[from-1]; ok {
			state.importedRoots[info.Root] = true
		}
	}

	out := make([]PlanEntry, 0, int(to-from))
	pendingOrphanImports := make(map[uint64][]blockInfo)
	for simSlot := from; simSlot < to; simSlot++ {
		evalSlot := saturatingAdd(simSlot, 1)
		sources := planSourcesForSlot(blockExists, simSlot, b.cfg.Mode, b.cfg.LookaheadCap, evalSlot)
		if sources == nil {
			sources = []PlanAttestationSource{}
		}

		entry := PlanEntry{
			SimSlot:            simSlot,
			EvalSlot:           evalSlot,
			ImportBlocks:       []PlanBlockImport{},
			AttestationSources: sources,
		}
		if len(sources) > 0 {
			source := sources[0].Slot
			entry.SourceBlockSlot = &source
		}

		if info, ok := canonicalBySlot[simSlot]; ok {
			entry.ImportBlocks = append(entry.ImportBlocks, blockImport(info, true))
			state.importedRoots[info.Root] = true
		}

		if pending := pendingOrphanImports[simSlot]; len(pending) > 0 {
			sortBlockInfos(pending)
			for _, info := range pending {
				if state.importedRoots[info.Root] {
					continue
				}
				entry.ImportBlocks = append(entry.ImportBlocks, blockImport(info, false))
				state.importedRoots[info.Root] = true
			}
			delete(pendingOrphanImports, simSlot)
		}

		orphanImports, err := state.orphanImportsForSources(sources, simSlot, evalSlot, from, to, pendingOrphanImports)
		if err != nil {
			return nil, err
		}
		entry.ImportBlocks = append(entry.ImportBlocks, orphanImports...)

		out = append(out, entry)
	}
	return out, nil
}

func (b *realBackend) loadCanonicalBlockInfos(from, to uint64) (map[uint64]blockInfo, map[[32]byte]blockInfo, error) {
	bySlot := make(map[uint64]blockInfo)
	byRoot := make(map[[32]byte]blockInfo)
	if from > to {
		return bySlot, byRoot, nil
	}

	for slot := from; ; slot++ {
		data, ok, err := b.cfg.EraReader.RawBlockSSZ(slot)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			info, err := parseBlockInfo(data, b.ConsensusVersionAtSlot)
			if err != nil {
				return nil, nil, fmt.Errorf("parse canonical block at slot %d: %w", slot, err)
			}
			bySlot[slot] = info
			byRoot[info.Root] = info
		}
		if slot == to || slot == math.MaxUint64 {
			break
		}
	}

	return bySlot, byRoot, nil
}

func planSourcesForSlot(blockExists map[uint64]bool, simSlot uint64, mode attplan.Mode, lookaheadCap uint64, evalSlot uint64) []PlanAttestationSource {
	switch mode {
	case attplan.ModeNextNonMissed:
		source, ok := firstNonMissedSource(blockExists, simSlot, lookaheadCap)
		if !ok {
			return nil
		}
		return []PlanAttestationSource{{Slot: source}}
	case attplan.ModeStrictKMinus1:
		source, ok := checkedAdd(simSlot, 1)
		if !ok || !blockExists[source] {
			return nil
		}
		return []PlanAttestationSource{{Slot: source}}
	case attplan.ModeGreedyLookahead:
		if lookaheadCap > math.MaxUint64-simSlot {
			return nil
		}
		sources := make([]PlanAttestationSource, 0, lookaheadCap)
		end := simSlot + lookaheadCap
		for slot := simSlot + 1; ; slot++ {
			if blockExists[slot] {
				maxSlot := evalSlot
				sources = append(sources, PlanAttestationSource{
					Slot:               slot,
					MaxAttestationSlot: &maxSlot,
				})
			}
			if slot == end {
				break
			}
		}
		return sources
	default:
		return nil
	}
}

func firstNonMissedSource(blockExists map[uint64]bool, simSlot, lookaheadCap uint64) (uint64, bool) {
	if lookaheadCap > math.MaxUint64-simSlot {
		return 0, false
	}

	end := simSlot + lookaheadCap
	for slot := simSlot + 1; ; slot++ {
		if blockExists[slot] {
			return slot, true
		}
		if slot == end {
			break
		}
	}
	return 0, false
}

type planBuildState struct {
	backend         *realBackend
	canonicalBySlot map[uint64]blockInfo
	canonicalByRoot map[[32]byte]blockInfo
	importedRoots   map[[32]byte]bool
	scheduledRoots  map[[32]byte]bool
	missingRoots    map[[32]byte]bool
	ignoredRoots    map[[32]byte]bool
	fetchedByRoot   map[[32]byte]blockInfo
}

func (s *planBuildState) orphanImportsForSources(
	sources []PlanAttestationSource,
	simSlot uint64,
	evalSlot uint64,
	minImportSlot uint64,
	planEnd uint64,
	pending map[uint64][]blockInfo,
) ([]PlanBlockImport, error) {
	if s.backend.cfg.Mode != attplan.ModeGreedyLookahead {
		return nil, nil
	}

	roots := make(map[[32]byte]bool)
	for _, source := range sources {
		info, ok := s.canonicalBySlot[source.Slot]
		if !ok {
			return nil, fmt.Errorf("attestation source slot %d is not a canonical block", source.Slot)
		}
		for _, attestation := range info.Attestations {
			if source.MaxAttestationSlot != nil && attestation.Slot > *source.MaxAttestationSlot {
				continue
			}
			for _, root := range [][32]byte{attestation.TargetRoot, attestation.BeaconBlockRoot} {
				if isZeroRoot(root) || s.rootKnown(root) || s.scheduledRoots[root] || s.missingRoots[root] || s.ignoredRoots[root] {
					continue
				}
				roots[root] = true
			}
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}

	sortedRoots := sortedRootKeys(roots)
	infos := make([]blockInfo, 0, len(sortedRoots))
	for _, root := range sortedRoots {
		if s.rootKnown(root) || s.scheduledRoots[root] || s.missingRoots[root] || s.ignoredRoots[root] {
			continue
		}
		chain, err := s.resolveOrphanChain(root, evalSlot)
		if err != nil {
			return nil, err
		}
		for _, info := range chain {
			if info.Slot < minImportSlot {
				s.ignoredRoots[info.Root] = true
				continue
			}
			if s.rootKnown(info.Root) || s.scheduledRoots[info.Root] {
				continue
			}
			targetSimSlot := info.Slot
			if targetSimSlot < simSlot {
				targetSimSlot = simSlot
			}
			if targetSimSlot >= planEnd {
				s.ignoredRoots[info.Root] = true
				continue
			}
			if targetSimSlot == simSlot {
				s.importedRoots[info.Root] = true
				infos = append(infos, info)
			} else {
				s.scheduledRoots[info.Root] = true
				pending[targetSimSlot] = append(pending[targetSimSlot], info)
			}
		}
	}

	sortBlockInfos(infos)

	imports := make([]PlanBlockImport, 0, len(infos))
	for _, info := range infos {
		imports = append(imports, blockImport(info, false))
	}
	return imports, nil
}

func sortBlockInfos(infos []blockInfo) {
	sort.SliceStable(infos, func(i, j int) bool {
		if infos[i].Slot != infos[j].Slot {
			return infos[i].Slot < infos[j].Slot
		}
		return bytes.Compare(infos[i].Root[:], infos[j].Root[:]) < 0
	})
}

func (s *planBuildState) resolveOrphanChain(root [32]byte, evalSlot uint64) ([]blockInfo, error) {
	current := root
	seen := make(map[[32]byte]bool)
	chain := make([]blockInfo, 0, maxOrphanWalk)

	for depth := 0; depth < maxOrphanWalk; depth++ {
		if isZeroRoot(current) || s.rootKnown(current) || s.missingRoots[current] || seen[current] {
			break
		}
		seen[current] = true

		info, err := s.fetchBlockInfoByRoot(current)
		if errors.Is(err, ErrNotFound) {
			s.missingRoots[current] = true
			break
		}
		if err != nil {
			return nil, err
		}
		if info.Slot > evalSlot {
			break
		}

		chain = append(chain, info)
		current = info.ParentRoot
	}

	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return chain, nil
}

func (s *planBuildState) fetchBlockInfoByRoot(root [32]byte) (blockInfo, error) {
	if info, ok := s.fetchedByRoot[root]; ok {
		return info, nil
	}

	data, err := s.backend.fetchBlockSSZByRootForPlan(root)
	if err != nil {
		return blockInfo{}, err
	}
	info, err := parseBlockInfo(data, s.backend.ConsensusVersionAtSlot)
	if err != nil {
		return blockInfo{}, fmt.Errorf("parse block %s from archive: %w", formatRoot(root), err)
	}
	if info.Root != root {
		return blockInfo{}, fmt.Errorf("block fetched for %s has root %s", formatRoot(root), formatRoot(info.Root))
	}

	s.fetchedByRoot[root] = info
	return info, nil
}

func (b *realBackend) fetchBlockSSZByRootForPlan(root [32]byte) ([]byte, error) {
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

func (s *planBuildState) rootKnown(root [32]byte) bool {
	if s.importedRoots[root] {
		return true
	}
	_, ok := s.canonicalByRoot[root]
	return ok
}

func blockImport(info blockInfo, canonical bool) PlanBlockImport {
	return PlanBlockImport{
		Slot:      info.Slot,
		Root:      formatRoot(info.Root),
		Canonical: canonical,
	}
}

func sortedRootKeys(roots map[[32]byte]bool) [][32]byte {
	out := make([][32]byte, 0, len(roots))
	for root := range roots {
		out = append(out, root)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out
}

func formatRoot(root [32]byte) string {
	return "0x" + hex.EncodeToString(root[:])
}

func isZeroRoot(root [32]byte) bool {
	return root == [32]byte{}
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
