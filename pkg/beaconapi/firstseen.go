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

	"github.com/ethpandaops/fcr-simulator/pkg/s3cache"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/parquet-go/parquet-go"
)

const (
	defaultFirstSeenEpochCacheSize = 64
	mainnetSlotsPerEpoch           = uint64(32)
)

type FirstSeenAttestationSourceConfig struct {
	BasePath       string
	Network        string
	DeadlineMS     uint64
	CacheDir       string
	S3Store        s3cache.Store
	EpochCacheSize int
}

type FirstSeenAttestationSource struct {
	base       firstSeenBase
	network    string
	deadlineMS uint64
	cacheDir   string
	s3Store    s3cache.Store
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
	slot            uint64
	beaconBlockRoot [32]byte
	sourceEpoch     uint64
	sourceRoot      [32]byte
	targetEpoch     uint64
	targetRoot      [32]byte
}

type firstSeenGroup struct {
	key              firstSeenGroupKey
	attestingIndices []uint64
	root             [32]byte
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
		cache:      cache,
		inflight:   make(map[uint64]*firstSeenInflight),
	}, nil
}

func (s *FirstSeenAttestationSource) AttestationsForSlot(slot uint64, _ func(uint64) string) ([]attestationInfo, error) {
	if s == nil {
		return nil, fmt.Errorf("first-seen attestation source is not configured")
	}
	epoch := slot / mainnetSlotsPerEpoch
	loaded, err := s.epoch(epoch)
	if err != nil {
		return nil, err
	}
	attestations := loaded.slots[slot]
	if len(attestations) == 0 {
		return nil, nil
	}
	out := make([]attestationInfo, len(attestations))
	for i, attestation := range attestations {
		out[i] = attestation
		out[i].AttestingIndices = append([]uint64(nil), attestation.AttestingIndices...)
	}
	return out, nil
}

func (s *FirstSeenAttestationSource) epoch(epoch uint64) (*firstSeenEpoch, error) {
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

	loaded, err := s.loadEpoch(epoch)

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

func (s *FirstSeenAttestationSource) loadEpoch(epoch uint64) (*firstSeenEpoch, error) {
	path, err := s.localParquetPath(epoch)
	if err != nil {
		return nil, err
	}

	rows, err := readFirstSeenRows(path)
	if err != nil {
		return nil, fmt.Errorf("read first-seen parquet for epoch %d: %w", epoch, err)
	}

	groups := make(map[firstSeenGroupKey]*firstSeenGroup)
	for _, row := range rows {
		if uint64(row.RawSeenMS) > s.deadlineMS {
			continue
		}
		if uint64(row.Epoch) != epoch {
			return nil, fmt.Errorf("first-seen row slot %d has epoch %d, expected %d", row.Slot, row.Epoch, epoch)
		}

		slot := uint64(row.Slot)

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

		key := firstSeenGroupKey{
			slot:            slot,
			beaconBlockRoot: beaconBlockRoot,
			sourceEpoch:     uint64(row.SourceEpoch),
			sourceRoot:      sourceRoot,
			targetEpoch:     uint64(row.TargetEpoch),
			targetRoot:      targetRoot,
		}
		group := groups[key]
		if group == nil {
			group = &firstSeenGroup{
				key: key,
			}
			group.root = syntheticFirstSeenAttestationRoot(key)
			groups[key] = group
		}
		group.attestingIndices = append(group.attestingIndices, uint64(row.ValidatorIndex))
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
		attestingIndices := sortedUniqueUint64s(group.attestingIndices)
		epochData.slots[key.slot] = append(epochData.slots[key.slot], attestationInfo{
			Root:             group.root,
			Slot:             key.slot,
			Index:            0,
			BeaconBlockRoot:  key.beaconBlockRoot,
			SourceEpoch:      key.sourceEpoch,
			SourceRoot:       key.sourceRoot,
			TargetEpoch:      key.targetEpoch,
			TargetRoot:       key.targetRoot,
			AttestingIndices: attestingIndices,
		})
	}
	return epochData, nil
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

func syntheticFirstSeenAttestationRoot(key firstSeenGroupKey) [32]byte {
	h := sha256.New()
	writeUint64Decimal(h, key.slot)
	h.Write(key.beaconBlockRoot[:])
	writeUint64Decimal(h, key.sourceEpoch)
	h.Write(key.sourceRoot[:])
	writeUint64Decimal(h, key.targetEpoch)
	h.Write(key.targetRoot[:])
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
	return 0
}

func sortedUniqueUint64s(values []uint64) []uint64 {
	if len(values) == 0 {
		return nil
	}
	out := append([]uint64(nil), values...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 1
	for _, value := range out[1:] {
		if value != out[n-1] {
			out[n] = value
			n++
		}
	}
	return out[:n]
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
