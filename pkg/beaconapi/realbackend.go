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
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	maxOrphanWalk = 16

	// DefaultDecodedBlockCacheSize is the orchestrator default for decoded
	// blockInfo LRU caches used by per-slot simulation planning.
	DefaultDecodedBlockCacheSize = 4096
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

	// ArchiveCacheOnly makes orphan-block resolution read the local block-archive
	// disk cache only, never contacting the archive over HTTP. The serving hot
	// loop sets this so a slow or unavailable archive can never stall or fail a
	// simulation; the prep pass warms the cache up front via WarmBlockArchiveCache.
	ArchiveCacheOnly bool

	// DecodedBlockCacheSize bounds decoded blockInfo caches. Zero disables
	// decoded caching.
	DecodedBlockCacheSize int
}

type realBackend struct {
	cfg           RealBackendConfig
	decodedBySlot *lru.Cache[uint64, blockInfo]
	decodedByRoot *lru.Cache[[32]byte, blockInfo]
}

func NewRealBackend(cfg RealBackendConfig) Backend {
	if cfg.ForkSchedule.SlotFork == nil {
		cfg.ForkSchedule.SlotFork = MainnetForkAtSlot
	}
	cacheSize := cfg.DecodedBlockCacheSize

	backend := &realBackend{cfg: cfg}
	if cacheSize > 0 {
		if cache, err := lru.New[uint64, blockInfo](cacheSize); err == nil {
			backend.decodedBySlot = cache
		}
		if cache, err := lru.New[[32]byte, blockInfo](cacheSize); err == nil {
			backend.decodedByRoot = cache
		}
	}
	return backend
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

func (b *realBackend) BuildSlot(simSlot, warmupStartSlot uint64) (SlotInstruction, error) {
	if b.cfg.EraReader == nil {
		return SlotInstruction{}, fmt.Errorf("era reader is not configured")
	}
	if simSlot <= warmupStartSlot {
		return SlotInstruction{}, fmt.Errorf("sim slot %d must be greater than warmup start slot %d", simSlot, warmupStartSlot)
	}
	if b.cfg.LookaheadCap < 1 {
		return SlotInstruction{}, fmt.Errorf("lookaheadCap must be >= 1 for slot instructions")
	}
	minImportSlot, ok := checkedAdd(warmupStartSlot, 1)
	if !ok {
		return SlotInstruction{}, fmt.Errorf("warmup start slot %d cannot be incremented", warmupStartSlot)
	}

	loadEnd := saturatingAdd(simSlot, b.cfg.LookaheadCap)
	canonicalBySlot, canonicalByRoot, err := b.loadCanonicalBlockInfos(minImportSlot, loadEnd)
	if err != nil {
		return SlotInstruction{}, err
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

	instruction := SlotInstruction{
		SimSlot:      simSlot,
		EvalSlot:     saturatingAdd(simSlot, 1),
		ImportBlocks: []PlanBlockImport{},
		Attestations: []PlanAttestation{},
	}

	if info, ok := canonicalBySlot[simSlot]; ok {
		instruction.ImportBlocks = append(instruction.ImportBlocks, blockImport(info, true))
		state.importedRoots[info.Root] = true
	}

	slotAttestations := attestationsMadeInWindow(canonicalBySlot, simSlot, b.cfg.LookaheadCap)
	orphanImports, err := state.orphanImportsForSlotAttestations(slotAttestations, simSlot, minImportSlot)
	if err != nil {
		return SlotInstruction{}, err
	}
	instruction.ImportBlocks = append(instruction.ImportBlocks, orphanImports...)

	for _, attestation := range slotAttestations {
		instruction.Attestations = append(instruction.Attestations, planAttestation(attestation))
	}

	return instruction, nil
}

// WarmBlockArchiveCache resolves and disk-caches every orphan block that the
// per-slot hot loop could request for the given sim-slot ranges. This is the
// only place the block archive is contacted over HTTP; the serving backend runs
// with ArchiveCacheOnly so it reads exclusively from the cache this warms. Each
// range is [from, to) in slots, matching one worker's import window.
func WarmBlockArchiveCache(cfg RealBackendConfig, ranges [][2]uint64) error {
	cfg.ArchiveCacheOnly = false
	backend, ok := NewRealBackend(cfg).(*realBackend)
	if !ok {
		return fmt.Errorf("warm block archive cache: unexpected backend type")
	}
	for _, r := range ranges {
		if err := backend.warmArchiveCache(r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}

func (b *realBackend) warmArchiveCache(from, to uint64) error {
	if b.cfg.BlockArchive == nil {
		return nil
	}
	if b.cfg.EraReader == nil {
		return fmt.Errorf("era reader is not configured")
	}
	if from >= to {
		return nil
	}

	loadEnd := saturatingAdd(to, b.cfg.LookaheadCap)
	canonicalBySlot, canonicalByRoot, err := b.loadCanonicalBlockInfos(from, loadEnd)
	if err != nil {
		return err
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

	roots := make(map[[32]byte]bool)
	for _, info := range canonicalBySlot {
		for _, attestation := range info.Attestations {
			for _, root := range [][32]byte{attestation.TargetRoot, attestation.BeaconBlockRoot} {
				if isZeroRoot(root) || state.rootKnown(root) {
					continue
				}
				roots[root] = true
			}
		}
	}

	for _, root := range sortedRootKeys(roots) {
		if _, err := state.resolveOrphanChain(root, loadEnd); err != nil {
			return err
		}
	}
	return nil
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
			info, err := b.decodeCanonicalBlockInfo(slot, data)
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

func (b *realBackend) decodeCanonicalBlockInfo(slot uint64, data []byte) (blockInfo, error) {
	if b.decodedBySlot != nil {
		if info, ok := b.decodedBySlot.Get(slot); ok {
			return info, nil
		}
	}

	info, err := parseBlockInfo(data, b.ConsensusVersionAtSlot)
	if err != nil {
		return blockInfo{}, err
	}
	if b.decodedBySlot != nil {
		b.decodedBySlot.Add(slot, info)
	}
	if b.decodedByRoot != nil {
		b.decodedByRoot.Add(info.Root, info)
	}
	return info, nil
}

func (b *realBackend) decodeBlockInfoByRoot(root [32]byte, data []byte) (blockInfo, error) {
	if b.decodedByRoot != nil {
		if info, ok := b.decodedByRoot.Get(root); ok {
			return info, nil
		}
	}

	info, err := parseBlockInfo(data, b.ConsensusVersionAtSlot)
	if err != nil {
		return blockInfo{}, err
	}
	if info.Root != root {
		return blockInfo{}, fmt.Errorf("block fetched for %s has root %s", formatRoot(root), formatRoot(info.Root))
	}
	if b.decodedByRoot != nil {
		b.decodedByRoot.Add(root, info)
	}
	return info, nil
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

func attestationsMadeInWindow(canonicalBySlot map[uint64]blockInfo, madeSlot, lookaheadCap uint64) []attestationInfo {
	if lookaheadCap == 0 || madeSlot == math.MaxUint64 {
		return nil
	}

	end, ok := checkedAdd(madeSlot, lookaheadCap)
	if !ok {
		return nil
	}

	start := madeSlot + 1
	out := make([]attestationInfo, 0)
	for sourceSlot := start; ; sourceSlot++ {
		if info, ok := canonicalBySlot[sourceSlot]; ok {
			for _, attestation := range info.Attestations {
				if attestation.Slot != madeSlot {
					continue
				}
				// Preserve every aggregate for this attested slot. Multiple
				// inclusion blocks can carry overlapping aggregates with the
				// same attestation data/root, and fork choice de-duplicates at
				// the validator vote level.
				out = append(out, attestation)
			}
		}
		if sourceSlot == end {
			break
		}
	}
	return out
}

func (s *planBuildState) orphanImportsForSlotAttestations(attestations []attestationInfo, simSlot, minImportSlot uint64) ([]PlanBlockImport, error) {
	roots := make(map[[32]byte]bool)
	for _, attestation := range attestations {
		for _, root := range [][32]byte{attestation.TargetRoot, attestation.BeaconBlockRoot} {
			if isZeroRoot(root) || s.rootKnown(root) || s.scheduledRoots[root] || s.missingRoots[root] || s.ignoredRoots[root] {
				continue
			}
			roots[root] = true
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}

	infos := make([]blockInfo, 0, len(roots))
	for _, root := range sortedRootKeys(roots) {
		if s.rootKnown(root) || s.scheduledRoots[root] || s.missingRoots[root] || s.ignoredRoots[root] {
			continue
		}
		chain, err := s.resolveOrphanChain(root, simSlot)
		if err != nil {
			return nil, err
		}
		if len(chain) == 0 {
			continue
		}

		// Stateless per-slot scheduling emits an orphan root only at its own
		// block slot. Earlier roots in the chain are included only as parents
		// needed to make that slot's orphan importable.
		if chain[len(chain)-1].Root != root || chain[len(chain)-1].Slot != simSlot {
			s.ignoredRoots[root] = true
			continue
		}

		for _, info := range chain {
			if info.Slot < minImportSlot {
				s.ignoredRoots[info.Root] = true
				continue
			}
			if info.Slot > simSlot || s.rootKnown(info.Root) || s.scheduledRoots[info.Root] {
				continue
			}
			s.scheduledRoots[info.Root] = true
			infos = append(infos, info)
		}
	}

	sortBlockInfos(infos)

	imports := make([]PlanBlockImport, 0, len(infos))
	for _, info := range infos {
		imports = append(imports, blockImport(info, false))
	}
	return imports, nil
}

func planAttestation(attestation attestationInfo) PlanAttestation {
	return PlanAttestation{
		AggregationBits: attestation.AggregationBits,
		CommitteeBits:   attestation.CommitteeBits,
		Data: PlanAttestationData{
			Slot:            attestation.Slot,
			Index:           attestation.Index,
			BeaconBlockRoot: formatRoot(attestation.BeaconBlockRoot),
			Source: PlanCheckpoint{
				Epoch: attestation.SourceEpoch,
				Root:  formatRoot(attestation.SourceRoot),
			},
			Target: PlanCheckpoint{
				Epoch: attestation.TargetEpoch,
				Root:  formatRoot(attestation.TargetRoot),
			},
		},
	}
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
		canonical, err := s.isCanonicalBlockInfo(info)
		if err != nil {
			return nil, err
		}
		if canonical {
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

func (s *planBuildState) isCanonicalBlockInfo(info blockInfo) (bool, error) {
	if canonical, ok := s.canonicalBySlot[info.Slot]; ok {
		return canonical.Root == info.Root, nil
	}
	if s.backend.cfg.EraReader == nil {
		return false, nil
	}

	data, ok, err := s.backend.cfg.EraReader.RawBlockSSZ(info.Slot)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	canonical, err := s.backend.decodeCanonicalBlockInfo(info.Slot, data)
	if err != nil {
		return false, fmt.Errorf("parse canonical block at slot %d: %w", info.Slot, err)
	}
	s.canonicalBySlot[canonical.Slot] = canonical
	s.canonicalByRoot[canonical.Root] = canonical
	return canonical.Root == info.Root, nil
}

func (s *planBuildState) fetchBlockInfoByRoot(root [32]byte) (blockInfo, error) {
	if info, ok := s.fetchedByRoot[root]; ok {
		return info, nil
	}

	data, err := s.backend.fetchBlockSSZByRootForPlan(root)
	if err != nil {
		return blockInfo{}, err
	}
	info, err := s.backend.decodeBlockInfoByRoot(root, data)
	if err != nil {
		return blockInfo{}, fmt.Errorf("parse block %s from archive: %w", formatRoot(root), err)
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

	var data []byte
	var err error
	if b.cfg.ArchiveCacheOnly {
		data, err = b.cfg.BlockArchive.ReadCachedSSZByRoot(root)
	} else {
		data, err = b.cfg.BlockArchive.FetchBlockSSZByRoot(root)
	}
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
