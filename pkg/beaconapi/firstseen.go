package beaconapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/s3cache"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/parquet-go/parquet-go"
	"github.com/prysmaticlabs/go-bitfield"
)

const (
	defaultFirstSeenEpochCacheSize = 64
	mainnetSlotsPerEpoch           = uint64(32)
	maxCommitteesPerSlot           = uint64(64)
)

type FirstSeenCommitteeProvider interface {
	FetchBeaconCommittees(epoch uint64) ([]beaconfetch.BeaconCommittee, error)
}

type FirstSeenAttestationSourceConfig struct {
	BasePath       string
	Network        string
	DeadlineMS     uint64
	CacheDir       string
	S3Store        s3cache.Store
	Committees     FirstSeenCommitteeProvider
	EpochCacheSize int
}

type FirstSeenAttestationSource struct {
	base       firstSeenBase
	network    string
	deadlineMS uint64
	cacheDir   string
	s3Store    s3cache.Store
	committees FirstSeenCommitteeProvider
	cache      *lru.Cache[uint64, *firstSeenEpoch]

	mu       sync.Mutex
	inflight map[uint64]*firstSeenInflight
}

type firstSeenBase struct {
	raw       string
	localPath string
	s3Prefix  string
	isS3      bool
}

type firstSeenInflight struct {
	done  chan struct{}
	epoch *firstSeenEpoch
	err   error
}

type firstSeenEpoch struct {
	slots map[uint64][]attestationInfo
}

type firstSeenParquetRow struct {
	Slot           uint32 `parquet:"slot"`
	Epoch          uint32 `parquet:"epoch"`
	ValidatorIndex uint32 `parquet:"validator_index"`
	CommitteeIndex string `parquet:"committee_index"`
	BlockRoot      string `parquet:"block_root"`
	SourceEpoch    uint32 `parquet:"source_epoch"`
	SourceRoot     string `parquet:"source_root"`
	TargetEpoch    uint32 `parquet:"target_epoch"`
	TargetRoot     string `parquet:"target_root"`
	RawSeenMS      uint32 `parquet:"raw_seen_ms"`
}

type firstSeenGroupKey struct {
	slot             uint64
	committeeIndex   uint64
	dataIndex        uint64
	beaconBlockRoot  [32]byte
	sourceEpoch      uint64
	sourceRoot       [32]byte
	targetEpoch      uint64
	targetRoot       [32]byte
	committeeBits    string
	hasCommitteeBits bool
}

type firstSeenGroup struct {
	key  firstSeenGroupKey
	bits bitfield.Bitlist
	root [32]byte
}

type slotCommitteeKey struct {
	slot  uint64
	index uint64
}

type firstSeenCommittee struct {
	validators map[uint64]uint64
	size       uint64
}

func NewFirstSeenAttestationSource(cfg FirstSeenAttestationSourceConfig) (*FirstSeenAttestationSource, error) {
	base, err := parseFirstSeenBase(cfg.BasePath)
	if err != nil {
		return nil, err
	}
	network := strings.TrimSpace(cfg.Network)
	if network == "" {
		return nil, fmt.Errorf("first-seen network is required")
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		return nil, fmt.Errorf("first-seen cache dir is required")
	}
	if cfg.Committees == nil {
		return nil, fmt.Errorf("first-seen committee provider is required")
	}
	if base.isS3 && cfg.S3Store == nil {
		return nil, fmt.Errorf("first-seen base %q requires an S3 store", cfg.BasePath)
	}

	cacheSize := cfg.EpochCacheSize
	if cacheSize <= 0 {
		cacheSize = defaultFirstSeenEpochCacheSize
	}
	cache, err := lru.New[uint64, *firstSeenEpoch](cacheSize)
	if err != nil {
		return nil, fmt.Errorf("create first-seen epoch cache: %w", err)
	}

	return &FirstSeenAttestationSource{
		base:       base,
		network:    network,
		deadlineMS: cfg.DeadlineMS,
		cacheDir:   cfg.CacheDir,
		s3Store:    cfg.S3Store,
		committees: cfg.Committees,
		cache:      cache,
		inflight:   make(map[uint64]*firstSeenInflight),
	}, nil
}

func (s *FirstSeenAttestationSource) AttestationsForSlot(slot uint64, forkAtSlot func(uint64) string) ([]attestationInfo, error) {
	if s == nil {
		return nil, fmt.Errorf("first-seen attestation source is not configured")
	}
	epoch := slot / mainnetSlotsPerEpoch
	loaded, err := s.epoch(epoch, forkAtSlot)
	if err != nil {
		return nil, err
	}
	attestations := loaded.slots[slot]
	if len(attestations) == 0 {
		return nil, nil
	}
	out := make([]attestationInfo, len(attestations))
	copy(out, attestations)
	return out, nil
}

func (s *FirstSeenAttestationSource) epoch(epoch uint64, forkAtSlot func(uint64) string) (*firstSeenEpoch, error) {
	s.mu.Lock()
	if cached, ok := s.cache.Get(epoch); ok {
		s.mu.Unlock()
		return cached, nil
	}
	if in, ok := s.inflight[epoch]; ok {
		s.mu.Unlock()
		<-in.done
		return in.epoch, in.err
	}
	in := &firstSeenInflight{done: make(chan struct{})}
	s.inflight[epoch] = in
	s.mu.Unlock()

	loaded, err := s.loadEpoch(epoch, forkAtSlot)

	s.mu.Lock()
	if err == nil {
		s.cache.Add(epoch, loaded)
	}
	in.epoch = loaded
	in.err = err
	delete(s.inflight, epoch)
	close(in.done)
	s.mu.Unlock()
	return loaded, err
}

func (s *FirstSeenAttestationSource) loadEpoch(epoch uint64, forkAtSlot func(uint64) string) (*firstSeenEpoch, error) {
	committees, err := s.loadCommittees(epoch)
	if err != nil {
		return nil, err
	}

	path, err := s.localParquetPath(epoch)
	if err != nil {
		return nil, err
	}

	rows, err := readFirstSeenRows(path)
	if err != nil {
		return nil, fmt.Errorf("read first-seen parquet for epoch %d: %w", epoch, err)
	}

	groups := make(map[firstSeenGroupKey]*firstSeenGroup)
	droppedUnassigned := 0
	for _, row := range rows {
		if uint64(row.RawSeenMS) > s.deadlineMS {
			continue
		}
		if uint64(row.Epoch) != epoch {
			return nil, fmt.Errorf("first-seen row slot %d has epoch %d, expected %d", row.Slot, row.Epoch, epoch)
		}

		slot := uint64(row.Slot)
		committeeIndex, err := strconv.ParseUint(strings.TrimSpace(row.CommitteeIndex), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("first-seen row slot %d validator %d committee_index %q: %w", row.Slot, row.ValidatorIndex, row.CommitteeIndex, err)
		}

		committee, ok := committees[slotCommitteeKey{slot: slot, index: committeeIndex}]
		if !ok {
			// Rare post-Electra rows reference a (slot, committee) the validator was not
			// actually assigned to (xatu attesting_validator_committee_index resolution
			// artifact, ~0.003% of rows). Skip the row rather than fail the whole run.
			droppedUnassigned++
			continue
		}
		position, ok := committee.validators[uint64(row.ValidatorIndex)]
		if !ok {
			droppedUnassigned++
			continue
		}

		beaconBlockRoot, err := parseFirstSeenRoot(row.BlockRoot)
		if err != nil {
			return nil, fmt.Errorf("first-seen row slot %d validator %d block_root: %w", row.Slot, row.ValidatorIndex, err)
		}
		sourceRoot, err := parseFirstSeenRoot(row.SourceRoot)
		if err != nil {
			return nil, fmt.Errorf("first-seen row slot %d validator %d source_root: %w", row.Slot, row.ValidatorIndex, err)
		}
		targetRoot, err := parseFirstSeenRoot(row.TargetRoot)
		if err != nil {
			return nil, fmt.Errorf("first-seen row slot %d validator %d target_root: %w", row.Slot, row.ValidatorIndex, err)
		}

		dataIndex := committeeIndex
		var committeeBits string
		var hasCommitteeBits bool
		if isElectraOrLaterFork(forkAtSlot(slot)) {
			dataIndex = 0
			bits, err := encodeCommitteeBits(committeeIndex)
			if err != nil {
				return nil, fmt.Errorf("first-seen row slot %d committee %d: %w", slot, committeeIndex, err)
			}
			committeeBits = bits
			hasCommitteeBits = true
		}

		key := firstSeenGroupKey{
			slot:             slot,
			committeeIndex:   committeeIndex,
			dataIndex:        dataIndex,
			beaconBlockRoot:  beaconBlockRoot,
			sourceEpoch:      uint64(row.SourceEpoch),
			sourceRoot:       sourceRoot,
			targetEpoch:      uint64(row.TargetEpoch),
			targetRoot:       targetRoot,
			committeeBits:    committeeBits,
			hasCommitteeBits: hasCommitteeBits,
		}
		group := groups[key]
		if group == nil {
			group = &firstSeenGroup{
				key:  key,
				bits: bitfield.NewBitlist(committee.size),
			}
			group.root = syntheticFirstSeenAttestationRoot(key)
			groups[key] = group
		}
		group.bits.SetBitAt(position, true)
	}

	if droppedUnassigned > 0 {
		fmt.Fprintf(os.Stderr, "first-seen epoch %d: skipped %d row(s) whose validator was not in the referenced committee (post-Electra resolution artifacts)\n", epoch, droppedUnassigned)
	}

	epochData := &firstSeenEpoch{slots: make(map[uint64][]attestationInfo)}
	sortedGroups := make([]*firstSeenGroup, 0, len(groups))
	for _, group := range groups {
		sortedGroups = append(sortedGroups, group)
	}
	sort.Slice(sortedGroups, func(i, j int) bool {
		return compareFirstSeenGroupKeys(sortedGroups[i].key, sortedGroups[j].key) < 0
	})
	for _, group := range sortedGroups {
		key := group.key
		var committeeBits *string
		if key.hasCommitteeBits {
			bits := key.committeeBits
			committeeBits = &bits
		}
		epochData.slots[key.slot] = append(epochData.slots[key.slot], attestationInfo{
			Root:            group.root,
			AggregationBits: bytesHex([]byte(group.bits)),
			CommitteeBits:   committeeBits,
			Slot:            key.slot,
			Index:           key.dataIndex,
			BeaconBlockRoot: key.beaconBlockRoot,
			SourceEpoch:     key.sourceEpoch,
			SourceRoot:      key.sourceRoot,
			TargetEpoch:     key.targetEpoch,
			TargetRoot:      key.targetRoot,
		})
	}
	return epochData, nil
}

func (s *FirstSeenAttestationSource) loadCommittees(epoch uint64) (map[slotCommitteeKey]firstSeenCommittee, error) {
	committees, err := s.committees.FetchBeaconCommittees(epoch)
	if err != nil {
		return nil, fmt.Errorf("fetch beacon committees for first-seen epoch %d: %w", epoch, err)
	}
	out := make(map[slotCommitteeKey]firstSeenCommittee, len(committees))
	for _, committee := range committees {
		key := slotCommitteeKey{slot: committee.Slot, index: committee.Index}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate beacon committee for slot %d index %d", committee.Slot, committee.Index)
		}
		positions := make(map[uint64]uint64, len(committee.Validators))
		for position, validator := range committee.Validators {
			positions[validator] = uint64(position)
		}
		out[key] = firstSeenCommittee{
			validators: positions,
			size:       uint64(len(committee.Validators)),
		}
	}
	return out, nil
}

func (s *FirstSeenAttestationSource) localParquetPath(epoch uint64) (string, error) {
	rel := firstSeenEpochRelativePath(s.network, epoch)
	if !s.base.isS3 {
		return filepath.Join(s.base.localPath, rel), nil
	}

	cachePath := filepath.Join(s.cacheDir, "attestation-first-seen", s.network, fmt.Sprintf("epoch-%d.parquet", epoch))
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat cached first-seen parquet %q: %w", cachePath, err)
	}

	key := pathpkg.Join(s.base.s3Prefix, filepath.ToSlash(rel))
	ok, err := s.s3Store.DownloadFile(context.Background(), key, cachePath)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("first-seen parquet object not found: s3://%s/%s", s.base.raw, key)
	}
	return cachePath, nil
}

func firstSeenEpochRelativePath(network string, epoch uint64) string {
	return filepath.Join(
		fmt.Sprintf("network=%s", network),
		"source=raw",
		fmt.Sprintf("epoch=%d", epoch),
		"data.parquet",
	)
}

func readFirstSeenRows(path string) (rows []firstSeenParquetRow, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("open parquet reader: %v", recovered)
		}
	}()

	reader := parquet.NewGenericReader[firstSeenParquetRow](file)
	defer reader.Close()

	batch := make([]firstSeenParquetRow, 8192)
	for {
		n, readErr := reader.Read(batch)
		if n > 0 {
			rows = append(rows, batch[:n]...)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return rows, nil
}

func parseFirstSeenBase(raw string) (firstSeenBase, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return firstSeenBase{}, fmt.Errorf("first-seen base path is required")
	}
	if !strings.HasPrefix(raw, "s3://") {
		return firstSeenBase{raw: raw, localPath: raw}, nil
	}

	withoutScheme := strings.TrimPrefix(raw, "s3://")
	if strings.Contains(withoutScheme, "?") || strings.Contains(withoutScheme, "#") {
		return firstSeenBase{}, fmt.Errorf("first-seen S3 base must not include query or fragment")
	}
	parts := strings.SplitN(withoutScheme, "/", 2)
	bucket := strings.TrimSpace(parts[0])
	if bucket == "" {
		return firstSeenBase{}, fmt.Errorf("first-seen S3 base must include a bucket")
	}
	prefix := ""
	if len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	return firstSeenBase{raw: bucket, s3Prefix: prefix, isS3: true}, nil
}

func parseFirstSeenRoot(value string) ([32]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return [32]byte{}, fmt.Errorf("empty root")
	}
	value = strings.TrimPrefix(value, "0x")
	value = strings.TrimPrefix(value, "0X")
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return [32]byte{}, err
	}
	if len(decoded) != 32 {
		return [32]byte{}, fmt.Errorf("expected 32 bytes, got %d", len(decoded))
	}
	var root [32]byte
	copy(root[:], decoded)
	return root, nil
}

func encodeCommitteeBits(committeeIndex uint64) (string, error) {
	if committeeIndex >= maxCommitteesPerSlot {
		return "", fmt.Errorf("committee index exceeds max committees per slot")
	}
	bits := bitfield.NewBitvector64()
	bits.SetBitAt(committeeIndex, true)
	return bytesHex(bits.Bytes()), nil
}

func isElectraOrLaterFork(fork string) bool {
	switch strings.ToLower(strings.TrimSpace(fork)) {
	case "electra", "fulu":
		return true
	default:
		return false
	}
}

func syntheticFirstSeenAttestationRoot(key firstSeenGroupKey) [32]byte {
	h := sha256.New()
	writeUint64Decimal(h, key.slot)
	writeUint64Decimal(h, key.committeeIndex)
	writeUint64Decimal(h, key.dataIndex)
	h.Write(key.beaconBlockRoot[:])
	writeUint64Decimal(h, key.sourceEpoch)
	h.Write(key.sourceRoot[:])
	writeUint64Decimal(h, key.targetEpoch)
	h.Write(key.targetRoot[:])
	if key.hasCommitteeBits {
		h.Write([]byte(key.committeeBits))
	}
	sum := h.Sum(nil)
	var root [32]byte
	copy(root[:], sum)
	return root
}

func writeUint64Decimal(w io.Writer, value uint64) {
	_, _ = w.Write([]byte(strconv.FormatUint(value, 10)))
	_, _ = w.Write([]byte{0})
}

func compareFirstSeenGroupKeys(a, b firstSeenGroupKey) int {
	if a.slot != b.slot {
		return compareUint64(a.slot, b.slot)
	}
	if a.committeeIndex != b.committeeIndex {
		return compareUint64(a.committeeIndex, b.committeeIndex)
	}
	if c := compareRoot(a.beaconBlockRoot, b.beaconBlockRoot); c != 0 {
		return c
	}
	if a.sourceEpoch != b.sourceEpoch {
		return compareUint64(a.sourceEpoch, b.sourceEpoch)
	}
	if c := compareRoot(a.sourceRoot, b.sourceRoot); c != 0 {
		return c
	}
	if a.targetEpoch != b.targetEpoch {
		return compareUint64(a.targetEpoch, b.targetEpoch)
	}
	if c := compareRoot(a.targetRoot, b.targetRoot); c != 0 {
		return c
	}
	return compareUint64(a.dataIndex, b.dataIndex)
}

func compareUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareRoot(a, b [32]byte) int {
	return strings.Compare(hex.EncodeToString(a[:]), hex.EncodeToString(b[:]))
}
