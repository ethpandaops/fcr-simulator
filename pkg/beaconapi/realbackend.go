package beaconapi

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/ethpandaops/fcr-simulator/pkg/attplan"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/blockarchive"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	maxOrphanWalk = 16

	canonicalLoadChunkSize    = 256
	archiveSlotRangeBatchSize = 1024
	maxArchiveOrphansPerSlot  = 8

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

	// ArchiveOrphanRootsBySlot is produced by WarmBlockArchiveCache from
	// block-archive slot-range index queries. It lets cache-only serving discover
	// orphan source blocks by slot without contacting the archive.
	ArchiveOrphanRootsBySlot map[uint64][][32]byte

	// DecodedBlockCacheSize bounds decoded blockInfo caches. Zero disables
	// decoded caching.
	DecodedBlockCacheSize int
}

type realBackend struct {
	cfg            RealBackendConfig
	canonicalIndex *canonicalBlockIndex
	decodedBySlot  *lru.Cache[uint64, blockInfo]
	decodedByRoot  *lru.Cache[[32]byte, blockInfo]
}

func NewRealBackend(cfg RealBackendConfig) Backend {
	if cfg.ForkSchedule.SlotFork == nil {
		cfg.ForkSchedule.SlotFork = MainnetForkAtSlot
	}
	cfg.ArchiveOrphanRootsBySlot = cloneOrphanRootsBySlot(cfg.ArchiveOrphanRootsBySlot)
	cacheSize := cfg.DecodedBlockCacheSize

	backend := &realBackend{cfg: cfg}
	backend.canonicalIndex = newCanonicalBlockIndex(backend)
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

type CanonicalRootLookup struct {
	backend *realBackend
	from    uint64
	to      uint64
}

func NewCanonicalRootLookup(cfg RealBackendConfig, from, to uint64) *CanonicalRootLookup {
	cfg.BlockArchive = nil
	cfg.ArchiveOrphanRootsBySlot = nil
	cfg.ArchiveCacheOnly = true
	backend, _ := NewRealBackend(cfg).(*realBackend)
	return &CanonicalRootLookup{backend: backend, from: from, to: to}
}

func (l *CanonicalRootLookup) Lookup(slot uint64) (root string, exists bool, known bool, err error) {
	if l == nil || l.backend == nil || slot < l.from || slot > l.to {
		return "", false, false, nil
	}
	info, ok, err := l.backend.canonicalInfoAtSlot(slot)
	if err != nil {
		return "", false, true, err
	}
	if !ok {
		return "", false, true, nil
	}
	return formatRoot(info.Root), true, true, nil
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
	return b.fetchBlockSSZByRootForPlan(root)
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
		backend:           b,
		canonical:         &canonicalBlockMap{bySlot: canonicalBySlot, byRoot: canonicalByRoot},
		importedRoots:     make(map[[32]byte]bool),
		scheduledRoots:    make(map[[32]byte]bool),
		missingRoots:      make(map[[32]byte]bool),
		ignoredRoots:      make(map[[32]byte]bool),
		fetchedByRoot:     make(map[[32]byte]blockInfo),
		orphanRootsBySlot: cloneOrphanRootsBySlot(b.cfg.ArchiveOrphanRootsBySlot),
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

		slotOrphanImports, err := state.archiveOrphanImportsForSlot(simSlot, from)
		if err != nil {
			return nil, err
		}
		entry.ImportBlocks = append(entry.ImportBlocks, slotOrphanImports...)

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
	if err := b.canonicalIndex.ensureRange(minImportSlot, loadEnd); err != nil {
		return SlotInstruction{}, err
	}
	canonical := b.canonicalIndex.view(minImportSlot, loadEnd)

	state := &planBuildState{
		backend:           b,
		canonical:         canonical,
		importedRoots:     make(map[[32]byte]bool),
		scheduledRoots:    make(map[[32]byte]bool),
		missingRoots:      make(map[[32]byte]bool),
		ignoredRoots:      make(map[[32]byte]bool),
		fetchedByRoot:     make(map[[32]byte]blockInfo),
		orphanRootsBySlot: cloneOrphanRootsBySlot(b.cfg.ArchiveOrphanRootsBySlot),
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

	if info, ok := canonical.infoBySlot(simSlot); ok {
		instruction.ImportBlocks = append(instruction.ImportBlocks, blockImport(info, true))
		state.importedRoots[info.Root] = true
	}

	slotOrphanImports, err := state.archiveOrphanImportsForSlot(simSlot, minImportSlot)
	if err != nil {
		return SlotInstruction{}, err
	}
	instruction.ImportBlocks = append(instruction.ImportBlocks, slotOrphanImports...)

	slotAttestations, err := state.attestationsMadeInWindow(simSlot, b.cfg.LookaheadCap)
	if err != nil {
		return SlotInstruction{}, err
	}
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

type ArchiveWarmStats struct {
	WorkerRanges             int
	SlotRangeQueries         int
	OrphanRootsDiscovered    int
	HighOrphanFanoutSlots    int
	RootsResolvedCached      int
	RootsArchiveMissing      int
	TransientFailuresRetried int
	ChainsTruncated          int
	ChainsPreWarmupParent    int
}

type ArchiveWarmResult struct {
	Stats             ArchiveWarmStats
	OrphanRootsBySlot map[uint64][][32]byte
}

type archiveWarmAccumulator struct {
	stats             ArchiveWarmStats
	resolved          map[[32]byte]bool
	missing           map[[32]byte]bool
	orphanRootsBySlot map[uint64][][32]byte
	highFanoutSlots   map[uint64]bool
}

func newArchiveWarmAccumulator() *archiveWarmAccumulator {
	return &archiveWarmAccumulator{
		resolved:          make(map[[32]byte]bool),
		missing:           make(map[[32]byte]bool),
		orphanRootsBySlot: make(map[uint64][][32]byte),
		highFanoutSlots:   make(map[uint64]bool),
	}
}

func (a *archiveWarmAccumulator) result() ArchiveWarmResult {
	if a == nil {
		return ArchiveWarmResult{}
	}
	return ArchiveWarmResult{
		Stats:             a.stats,
		OrphanRootsBySlot: cloneOrphanRootsBySlot(a.orphanRootsBySlot),
	}
}

func (a *archiveWarmAccumulator) addSlotRangeQuery() {
	if a == nil {
		return
	}
	a.stats.SlotRangeQueries++
}

func (a *archiveWarmAccumulator) addOrphanRoot(slot uint64, root [32]byte) bool {
	if a == nil {
		return false
	}
	added, highFanout := appendOrphanRoot(a.orphanRootsBySlot, slot, root)
	if highFanout {
		a.addHighFanoutSlot(slot)
	}
	if added {
		a.stats.OrphanRootsDiscovered++
	}
	return added
}

func (a *archiveWarmAccumulator) addHighFanoutSlot(slot uint64) {
	if a == nil || a.highFanoutSlots[slot] {
		return
	}
	a.highFanoutSlots[slot] = true
	a.stats.HighOrphanFanoutSlots++
}

func (a *archiveWarmAccumulator) addResolved(root [32]byte) {
	if a == nil || a.resolved[root] {
		return
	}
	a.resolved[root] = true
	a.stats.RootsResolvedCached++
}

func (a *archiveWarmAccumulator) addMissing(root [32]byte) {
	if a == nil || a.missing[root] {
		return
	}
	a.missing[root] = true
	a.stats.RootsArchiveMissing++
}

func (a *archiveWarmAccumulator) addTransientRetries(count int) {
	if a == nil {
		return
	}
	a.stats.TransientFailuresRetried += count
}

func (a *archiveWarmAccumulator) addTruncatedChain() {
	if a == nil {
		return
	}
	a.stats.ChainsTruncated++
}

func (a *archiveWarmAccumulator) observeChain(chain []blockInfo, minSlot uint64) {
	if a == nil {
		return
	}
	for _, info := range chain {
		if info.Slot < minSlot {
			a.stats.ChainsPreWarmupParent++
			return
		}
	}
}

// WarmBlockArchiveCache resolves and disk-caches every orphan block that the
// per-slot hot loop could request for the given sim-slot ranges. This is the
// only place the block archive is contacted over HTTP; the serving backend runs
// with ArchiveCacheOnly so it reads exclusively from the cache this warms. Each
// range is [from, to) in slots, matching one worker's import window.
func WarmBlockArchiveCache(cfg RealBackendConfig, ranges [][2]uint64) (ArchiveWarmResult, error) {
	cfg.ArchiveCacheOnly = false
	backend, ok := NewRealBackend(cfg).(*realBackend)
	if !ok {
		return ArchiveWarmResult{}, fmt.Errorf("warm block archive cache: unexpected backend type")
	}
	acc := newArchiveWarmAccumulator()
	for _, r := range ranges {
		if err := backend.warmArchiveCache(r[0], r[1], acc); err != nil {
			return acc.result(), err
		}
	}
	return acc.result(), nil
}

func (b *realBackend) warmArchiveCache(from, to uint64, acc *archiveWarmAccumulator) error {
	if b.cfg.BlockArchive == nil {
		return nil
	}
	if b.cfg.EraReader == nil {
		return fmt.Errorf("era reader is not configured")
	}
	if from >= to {
		return nil
	}
	if acc != nil {
		acc.stats.WorkerRanges++
	}

	loadEnd := saturatingAdd(to, b.cfg.LookaheadCap)
	canonicalBySlot, canonicalByRoot, err := b.loadCanonicalBlockInfos(from, loadEnd)
	if err != nil {
		return err
	}

	state := &planBuildState{
		backend:           b,
		canonical:         &canonicalBlockMap{bySlot: canonicalBySlot, byRoot: canonicalByRoot},
		importedRoots:     make(map[[32]byte]bool),
		scheduledRoots:    make(map[[32]byte]bool),
		missingRoots:      make(map[[32]byte]bool),
		ignoredRoots:      make(map[[32]byte]bool),
		fetchedByRoot:     make(map[[32]byte]blockInfo),
		orphanRootsBySlot: make(map[uint64][][32]byte),
		warm:              acc,
	}
	for root := range b.cfg.CheckpointBlocksByRoot {
		state.importedRoots[root] = true
	}

	if b.cfg.Mode == attplan.ModeGreedyLookahead {
		if err := b.warmSlotRangeOrphans(state, from, loadEnd, acc); err != nil {
			return err
		}
	}

	roots := make(map[[32]byte]bool)
	for _, info := range canonicalBySlot {
		state.collectUnknownAttestationRoots(info, roots)
	}
	for slot := range state.orphanRootsBySlot {
		infos, err := state.archiveOrphanInfosBySlot(slot)
		if err != nil {
			return err
		}
		for _, info := range infos {
			state.collectUnknownAttestationRoots(info, roots)
		}
	}

	for _, root := range sortedRootKeys(roots) {
		chain, err := state.resolveOrphanChain(root, loadEnd)
		if err != nil {
			return err
		}
		acc.observeChain(chain, from)
	}
	return nil
}

func (b *realBackend) warmSlotRangeOrphans(state *planBuildState, from, to uint64, acc *archiveWarmAccumulator) error {
	if b.cfg.BlockArchive == nil || from > to {
		return nil
	}

	for batchFrom := from; ; {
		batchTo := to
		if maxBatchTo, ok := checkedAdd(batchFrom, archiveSlotRangeBatchSize-1); ok && maxBatchTo < batchTo {
			batchTo = maxBatchTo
		}
		limit := int(batchTo - batchFrom + 1)

		acc.addSlotRangeQuery()
		entries, fetchStats, err := b.cfg.BlockArchive.ListIndexBySlotRangeWithStats(batchFrom, batchTo, limit)
		acc.addTransientRetries(fetchStats.TransientFailuresRetried)
		if errors.Is(err, blockarchive.ErrNotFound) {
			entries = nil
		} else if err != nil {
			return err
		}

		for _, entry := range entries {
			root, err := blockarchive.ParseRoot(entry.BlockRoot)
			if err != nil {
				return fmt.Errorf("parse block archive index root for slot %d: %w", entry.Slot, err)
			}
			if isZeroRoot(root) || state.rootKnown(root) {
				continue
			}
			if info, ok := state.fetchedByRoot[root]; ok {
				if info.Slot == entry.Slot {
					canonical, err := state.isCanonicalBlockInfo(info)
					if err != nil {
						return err
					}
					if !canonical {
						state.rememberArchiveOrphanInfo(info)
					}
				}
				continue
			}

			data, fetchStats, err := b.cfg.BlockArchive.FetchBlockSSZBySlotAndRootWithStats(entry.Slot, root)
			acc.addTransientRetries(fetchStats.TransientFailuresRetried)
			if errors.Is(err, blockarchive.ErrNotFound) {
				acc.addMissing(root)
				continue
			}
			if err != nil {
				return err
			}
			info, err := b.decodeBlockInfoByRoot(root, data)
			if err != nil {
				return fmt.Errorf("parse slot-range orphan block %s at slot %d: %w", formatRoot(root), entry.Slot, err)
			}
			if info.Slot != entry.Slot {
				return fmt.Errorf("block archive index said root %s was at slot %d, decoded slot %d", formatRoot(root), entry.Slot, info.Slot)
			}
			canonical, err := state.isCanonicalBlockInfo(info)
			if err != nil {
				return err
			}
			if canonical {
				continue
			}

			state.fetchedByRoot[root] = info
			acc.addResolved(root)
			state.rememberArchiveOrphanInfo(info)
		}

		if batchTo == to || batchTo == math.MaxUint64 {
			break
		}
		batchFrom = batchTo + 1
	}
	return nil
}

type slotRange struct {
	from uint64
	to   uint64
}

type canonicalBlockIndex struct {
	backend *realBackend

	mu      sync.RWMutex
	cond    *sync.Cond
	loaded  []slotRange
	loading []slotRange
	bySlot  map[uint64]blockInfo
	byRoot  map[[32]byte]blockInfo
}

func newCanonicalBlockIndex(backend *realBackend) *canonicalBlockIndex {
	index := &canonicalBlockIndex{
		backend: backend,
		bySlot:  make(map[uint64]blockInfo),
		byRoot:  make(map[[32]byte]blockInfo),
	}
	index.cond = sync.NewCond(&index.mu)
	return index
}

func (i *canonicalBlockIndex) ensureRange(from, to uint64) error {
	if from > to {
		return nil
	}

	for {
		i.mu.Lock()
		gap, wait, ok := i.nextGapLocked(from, to)
		if !ok {
			i.mu.Unlock()
			return nil
		}
		if wait {
			i.cond.Wait()
			i.mu.Unlock()
			continue
		}
		i.loading = insertSlotRange(i.loading, gap)
		i.mu.Unlock()

		infos, err := i.loadRange(gap)

		i.mu.Lock()
		i.loading = removeSlotRange(i.loading, gap)
		if err == nil {
			for _, info := range infos {
				i.bySlot[info.Slot] = info
				i.byRoot[info.Root] = info
			}
			i.loaded = addSlotRange(i.loaded, gap)
		}
		i.cond.Broadcast()
		i.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (i *canonicalBlockIndex) loadRange(r slotRange) ([]blockInfo, error) {
	infos := make([]blockInfo, 0)
	for slot := r.from; ; slot++ {
		data, ok, err := i.backend.cfg.EraReader.RawBlockSSZ(slot)
		if err != nil {
			return nil, err
		}
		if ok {
			info, err := i.backend.decodeCanonicalBlockInfo(slot, data)
			if err != nil {
				return nil, fmt.Errorf("parse canonical block at slot %d: %w", slot, err)
			}
			infos = append(infos, info)
		}
		if slot == r.to || slot == math.MaxUint64 {
			break
		}
	}
	return infos, nil
}

func (i *canonicalBlockIndex) nextGapLocked(from, to uint64) (slotRange, bool, bool) {
	for slot := from; ; {
		if r, ok := rangeContainingSlot(i.loaded, slot); ok {
			if r.to == math.MaxUint64 {
				return slotRange{}, false, false
			}
			slot = r.to + 1
			if slot > to {
				return slotRange{}, false, false
			}
			continue
		}
		if _, ok := rangeContainingSlot(i.loading, slot); ok {
			return slotRange{}, true, true
		}

		gap := slotRange{from: slot, to: to}
		for _, r := range append(append([]slotRange{}, i.loaded...), i.loading...) {
			if r.from > slot && r.from-1 < gap.to {
				gap.to = r.from - 1
			}
		}
		if maxTo, ok := checkedAdd(slot, canonicalLoadChunkSize-1); ok && maxTo < gap.to {
			gap.to = maxTo
		}
		return gap, false, true
	}
}

func (i *canonicalBlockIndex) infoBySlot(slot uint64) (blockInfo, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	info, ok := i.bySlot[slot]
	return info, ok
}

func (i *canonicalBlockIndex) infoByRoot(root [32]byte) (blockInfo, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	info, ok := i.byRoot[root]
	return info, ok
}

func (i *canonicalBlockIndex) snapshotRange(from, to uint64) (map[uint64]blockInfo, map[[32]byte]blockInfo) {
	bySlot := make(map[uint64]blockInfo)
	byRoot := make(map[[32]byte]blockInfo)

	i.mu.RLock()
	defer i.mu.RUnlock()
	for slot, info := range i.bySlot {
		if slot < from || slot > to {
			continue
		}
		bySlot[slot] = info
		byRoot[info.Root] = info
	}
	return bySlot, byRoot
}

func (i *canonicalBlockIndex) view(from, to uint64) *canonicalBlockView {
	return &canonicalBlockView{index: i, from: from, to: to}
}

func rangeContainingSlot(ranges []slotRange, slot uint64) (slotRange, bool) {
	for _, r := range ranges {
		if slot < r.from {
			return slotRange{}, false
		}
		if slot <= r.to {
			return r, true
		}
	}
	return slotRange{}, false
}

func addSlotRange(ranges []slotRange, next slotRange) []slotRange {
	out := make([]slotRange, 0, len(ranges)+1)
	inserted := false
	for _, current := range ranges {
		if rangeBefore(current, next) {
			out = append(out, current)
			continue
		}
		if rangeBefore(next, current) {
			if !inserted {
				out = append(out, next)
				inserted = true
			}
			out = append(out, current)
			continue
		}
		if current.from < next.from {
			next.from = current.from
		}
		if current.to > next.to {
			next.to = current.to
		}
	}
	if !inserted {
		out = append(out, next)
	}
	return out
}

func insertSlotRange(ranges []slotRange, next slotRange) []slotRange {
	out := make([]slotRange, 0, len(ranges)+1)
	inserted := false
	for _, current := range ranges {
		if !inserted && next.from < current.from {
			out = append(out, next)
			inserted = true
		}
		out = append(out, current)
	}
	if !inserted {
		out = append(out, next)
	}
	return out
}

func removeSlotRange(ranges []slotRange, target slotRange) []slotRange {
	out := ranges[:0]
	for _, r := range ranges {
		if r == target {
			continue
		}
		out = append(out, r)
	}
	return out
}

func rangeBefore(left, right slotRange) bool {
	if left.to == math.MaxUint64 {
		return false
	}
	return left.to+1 < right.from
}

func (b *realBackend) loadCanonicalBlockInfos(from, to uint64) (map[uint64]blockInfo, map[[32]byte]blockInfo, error) {
	if from > to {
		return make(map[uint64]blockInfo), make(map[[32]byte]blockInfo), nil
	}

	if err := b.canonicalIndex.ensureRange(from, to); err != nil {
		return nil, nil, err
	}
	bySlot, byRoot := b.canonicalIndex.snapshotRange(from, to)
	return bySlot, byRoot, nil
}

func (b *realBackend) canonicalInfoAtSlot(slot uint64) (blockInfo, bool, error) {
	if err := b.canonicalIndex.ensureRange(slot, slot); err != nil {
		return blockInfo{}, false, err
	}
	info, ok := b.canonicalIndex.infoBySlot(slot)
	return info, ok, nil
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

type canonicalBlocks interface {
	infoBySlot(slot uint64) (blockInfo, bool)
	rootKnown(root [32]byte) bool
	add(info blockInfo)
}

type canonicalBlockMap struct {
	bySlot map[uint64]blockInfo
	byRoot map[[32]byte]blockInfo
}

func (m *canonicalBlockMap) infoBySlot(slot uint64) (blockInfo, bool) {
	info, ok := m.bySlot[slot]
	return info, ok
}

func (m *canonicalBlockMap) rootKnown(root [32]byte) bool {
	_, ok := m.byRoot[root]
	return ok
}

func (m *canonicalBlockMap) add(info blockInfo) {
	m.bySlot[info.Slot] = info
	m.byRoot[info.Root] = info
}

type canonicalBlockView struct {
	index *canonicalBlockIndex
	from  uint64
	to    uint64

	extraBySlot map[uint64]blockInfo
	extraByRoot map[[32]byte]blockInfo
}

func (v *canonicalBlockView) infoBySlot(slot uint64) (blockInfo, bool) {
	if slot >= v.from && slot <= v.to {
		if info, ok := v.index.infoBySlot(slot); ok {
			return info, true
		}
	}
	if v.extraBySlot != nil {
		info, ok := v.extraBySlot[slot]
		return info, ok
	}
	return blockInfo{}, false
}

func (v *canonicalBlockView) rootKnown(root [32]byte) bool {
	if v.extraByRoot != nil {
		if _, ok := v.extraByRoot[root]; ok {
			return true
		}
	}
	info, ok := v.index.infoByRoot(root)
	if !ok {
		return false
	}
	return info.Slot >= v.from && info.Slot <= v.to
}

func (v *canonicalBlockView) add(info blockInfo) {
	if v.extraBySlot == nil {
		v.extraBySlot = make(map[uint64]blockInfo)
		v.extraByRoot = make(map[[32]byte]blockInfo)
	}
	v.extraBySlot[info.Slot] = info
	v.extraByRoot[info.Root] = info
}

type planBuildState struct {
	backend           *realBackend
	canonical         canonicalBlocks
	importedRoots     map[[32]byte]bool
	scheduledRoots    map[[32]byte]bool
	missingRoots      map[[32]byte]bool
	ignoredRoots      map[[32]byte]bool
	fetchedByRoot     map[[32]byte]blockInfo
	orphanRootsBySlot map[uint64][][32]byte
	warm              *archiveWarmAccumulator
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
		sourceInfos, err := s.sourceBlockInfosBySlot(source.Slot)
		if err != nil {
			return nil, err
		}
		if len(sourceInfos) == 0 {
			return nil, fmt.Errorf("attestation source slot %d is not a canonical block", source.Slot)
		}
		for _, info := range sourceInfos {
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

func (s *planBuildState) attestationsMadeInWindow(madeSlot, lookaheadCap uint64) ([]attestationInfo, error) {
	if lookaheadCap == 0 || madeSlot == math.MaxUint64 {
		return nil, nil
	}

	end, ok := checkedAdd(madeSlot, lookaheadCap)
	if !ok {
		return nil, nil
	}

	start := madeSlot + 1
	out := make([]attestationInfo, 0)
	seen := make(map[[32]byte]bool)
	for sourceSlot := start; ; sourceSlot++ {
		sourceInfos, err := s.sourceBlockInfosBySlot(sourceSlot)
		if err != nil {
			return nil, err
		}
		for _, info := range sourceInfos {
			for _, attestation := range info.Attestations {
				if attestation.Slot != madeSlot {
					continue
				}
				// De-duplicate only exact aggregate repeats. Different
				// aggregates for the same attestation data carry different
				// validator bits and must all be injected; fork choice
				// de-duplicates at the validator vote level.
				if seen[attestation.Root] {
					continue
				}
				seen[attestation.Root] = true
				out = append(out, attestation)
			}
		}
		if sourceSlot == end {
			break
		}
	}
	return out, nil
}

func (s *planBuildState) sourceBlockInfosBySlot(slot uint64) ([]blockInfo, error) {
	infos := make([]blockInfo, 0, 1)
	if info, ok := s.canonical.infoBySlot(slot); ok {
		infos = append(infos, info)
	}
	orphanInfos, err := s.archiveOrphanInfosBySlot(slot)
	if err != nil {
		return nil, err
	}
	infos = append(infos, orphanInfos...)
	return infos, nil
}

func (s *planBuildState) archiveOrphanInfosBySlot(slot uint64) ([]blockInfo, error) {
	roots := s.orphanRootsBySlot[slot]
	if len(roots) == 0 {
		return nil, nil
	}

	infos := make([]blockInfo, 0, len(roots))
	seen := make(map[[32]byte]bool, len(roots))
	for _, root := range roots {
		if isZeroRoot(root) || seen[root] || s.missingRoots[root] || s.ignoredRoots[root] {
			continue
		}
		seen[root] = true

		info, err := s.fetchBlockInfoByRoot(root)
		if errors.Is(err, ErrNotFound) {
			s.missingRoots[root] = true
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Slot != slot {
			s.ignoredRoots[root] = true
			continue
		}
		canonical, err := s.isCanonicalBlockInfo(info)
		if err != nil {
			return nil, err
		}
		if canonical {
			s.ignoredRoots[root] = true
			continue
		}
		infos = append(infos, info)
	}

	sortBlockInfos(infos)
	return infos, nil
}

func (s *planBuildState) archiveOrphanImportsForSlot(slot, minImportSlot uint64) ([]PlanBlockImport, error) {
	infos, err := s.archiveOrphanInfosBySlot(slot)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, nil
	}

	imports := make([]PlanBlockImport, 0, len(infos))
	for _, info := range infos {
		if info.Slot < minImportSlot {
			s.ignoredRoots[info.Root] = true
			continue
		}
		if s.rootKnown(info.Root) || s.scheduledRoots[info.Root] {
			continue
		}
		s.importedRoots[info.Root] = true
		imports = append(imports, blockImport(info, false))
	}
	return imports, nil
}

func (s *planBuildState) collectUnknownAttestationRoots(info blockInfo, roots map[[32]byte]bool) {
	for _, attestation := range info.Attestations {
		for _, root := range [][32]byte{attestation.TargetRoot, attestation.BeaconBlockRoot} {
			if isZeroRoot(root) || s.rootKnown(root) || s.scheduledRoots[root] || s.missingRoots[root] || s.ignoredRoots[root] {
				continue
			}
			roots[root] = true
		}
	}
}

func (s *planBuildState) rememberArchiveOrphanInfo(info blockInfo) {
	s.rememberArchiveOrphanRoot(info.Slot, info.Root)
}

func (s *planBuildState) rememberArchiveOrphanRoot(slot uint64, root [32]byte) {
	if isZeroRoot(root) {
		return
	}
	if s.orphanRootsBySlot == nil {
		s.orphanRootsBySlot = make(map[uint64][][32]byte)
	}
	added, highFanout := appendOrphanRoot(s.orphanRootsBySlot, slot, root)
	if highFanout {
		s.warm.addHighFanoutSlot(slot)
		return
	}
	if added {
		s.warm.addOrphanRoot(slot, root)
	}
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
	truncated := true

	for depth := 0; depth < maxOrphanWalk; depth++ {
		if isZeroRoot(current) || s.rootKnown(current) || s.missingRoots[current] || seen[current] {
			truncated = false
			break
		}
		seen[current] = true

		info, err := s.fetchBlockInfoByRoot(current)
		if errors.Is(err, ErrNotFound) {
			s.missingRoots[current] = true
			truncated = false
			break
		}
		if err != nil {
			return nil, err
		}
		if info.Slot > evalSlot {
			truncated = false
			break
		}
		canonical, err := s.isCanonicalBlockInfo(info)
		if err != nil {
			return nil, err
		}
		if canonical {
			truncated = false
			break
		}

		chain = append(chain, info)
		s.rememberArchiveOrphanInfo(info)
		current = info.ParentRoot
	}
	if truncated {
		s.warm.addTruncatedChain()
	}

	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return chain, nil
}

func (s *planBuildState) isCanonicalBlockInfo(info blockInfo) (bool, error) {
	if canonical, ok := s.canonical.infoBySlot(info.Slot); ok {
		return canonical.Root == info.Root, nil
	}
	if s.backend.cfg.EraReader == nil {
		return false, nil
	}

	canonical, ok, err := s.backend.canonicalInfoAtSlot(info.Slot)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	s.canonical.add(canonical)
	return canonical.Root == info.Root, nil
}

func (s *planBuildState) fetchBlockInfoByRoot(root [32]byte) (blockInfo, error) {
	if info, ok := s.fetchedByRoot[root]; ok {
		return info, nil
	}

	data, fetchStats, err := s.backend.fetchBlockSSZByRootForPlanWithStats(root)
	s.warm.addTransientRetries(fetchStats.TransientFailuresRetried)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.warm.addMissing(root)
		}
		return blockInfo{}, err
	}
	info, err := s.backend.decodeBlockInfoByRoot(root, data)
	if err != nil {
		return blockInfo{}, fmt.Errorf("parse block %s from archive: %w", formatRoot(root), err)
	}

	s.fetchedByRoot[root] = info
	s.warm.addResolved(root)
	return info, nil
}

func (b *realBackend) fetchBlockSSZByRootForPlan(root [32]byte) ([]byte, error) {
	data, _, err := b.fetchBlockSSZByRootForPlanWithStats(root)
	return data, err
}

func (b *realBackend) fetchBlockSSZByRootForPlanWithStats(root [32]byte) ([]byte, blockarchive.FetchStats, error) {
	var stats blockarchive.FetchStats
	if b.cfg.CheckpointBlocksByRoot != nil {
		if data, ok := b.cfg.CheckpointBlocksByRoot[root]; ok {
			return cloneBytes(data), stats, nil
		}
	}
	if b.cfg.BlockArchive == nil {
		return nil, stats, ErrNotFound
	}

	var data []byte
	var err error
	if b.cfg.ArchiveCacheOnly {
		data, err = b.cfg.BlockArchive.ReadCachedSSZByRoot(root)
	} else {
		data, stats, err = b.cfg.BlockArchive.FetchBlockSSZByRootWithStats(root)
	}
	if errors.Is(err, blockarchive.ErrNotFound) {
		return nil, stats, ErrNotFound
	}
	if err != nil {
		return nil, stats, err
	}
	return cloneBytes(data), stats, nil
}

func (s *planBuildState) rootKnown(root [32]byte) bool {
	if s.importedRoots[root] {
		return true
	}
	return s.canonical.rootKnown(root)
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

func appendOrphanRoot(rootsBySlot map[uint64][][32]byte, slot uint64, root [32]byte) (bool, bool) {
	if rootsBySlot == nil || isZeroRoot(root) {
		return false, false
	}
	roots := rootsBySlot[slot]
	for _, existing := range roots {
		if existing == root {
			return false, false
		}
	}
	if len(roots) >= maxArchiveOrphansPerSlot {
		return false, true
	}
	rootsBySlot[slot] = append(roots, root)
	return true, false
}

func cloneOrphanRootsBySlot(in map[uint64][][32]byte) map[uint64][][32]byte {
	if len(in) == 0 {
		return nil
	}
	out := make(map[uint64][][32]byte, len(in))
	for slot, roots := range in {
		if len(roots) == 0 {
			continue
		}
		cloned := append([][32]byte(nil), roots...)
		sort.Slice(cloned, func(i, j int) bool {
			return bytes.Compare(cloned[i][:], cloned[j][:]) < 0
		})
		out[slot] = cloned
	}
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
