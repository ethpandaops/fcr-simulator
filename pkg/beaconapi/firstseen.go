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
)

type BeaconCommitteeProvider interface {
	FetchBeaconCommittees(epoch uint64) ([]beaconfetch.BeaconCommittee, error)
}

type FirstSeenAttestationSourceConfig struct {
	BasePath          string
	Network           string
	DeadlineMS        uint64
	CacheDir          string
	S3Store           s3cache.Store
	CommitteeProvider BeaconCommitteeProvider
	EpochCacheSize    int
}

type FirstSeenAttestationSource struct {
	base              firstSeenBase
	network           string
	deadlineMS        uint64
	cacheDir          string
	s3Store           s3cache.Store
	committeeProvider BeaconCommitteeProvider
	cache             *lru.Cache[uint64, *firstSeenEpoch]

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

type firstSeenVoteKey struct {
	vote           firstSeenGroupKey
	validatorIndex uint64
}

type firstSeenVote struct {
	key        firstSeenVoteKey
	assignment firstSeenCommitteeAssignment
	root       [32]byte
}

type firstSeenCommitteeAssignment struct {
	committeeIndex uint64
	position       uint64
	committeeSize  uint64
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
	if cfg.CommitteeProvider == nil {
		return nil, fmt.Errorf("first-seen committee provider is required")
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
		base:              base,
		network:           network,
		deadlineMS:        cfg.DeadlineMS,
		cacheDir:          cfg.CacheDir,
		s3Store:           cfg.S3Store,
		committeeProvider: cfg.CommitteeProvider,
		cache:             cache,
		inflight:          make(map[uint64]*firstSeenInflight),
	}, nil
}

func (s *FirstSeenAttestationSource) AttestationsForSlot(slot uint64, forkAtSlot func(uint64) string) ([]attestationInfo, error) {
	if s == nil {
		return nil, fmt.Errorf("first-seen attestation source is not configured")
	}
	if forkAtSlot == nil {
		forkAtSlot = MainnetForkAtSlot
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
	for i, attestation := range attestations {
		out[i] = attestation
		if attestation.CommitteeBits != nil {
			committeeBits := *attestation.CommitteeBits
			out[i].CommitteeBits = &committeeBits
		}
		out[i].AttestingIndices = append([]uint64(nil), attestation.AttestingIndices...)
	}
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
	path, err := s.localParquetPath(epoch)
	if err != nil {
		return nil, err
	}

	rows, err := readFirstSeenRows(path)
	if err != nil {
		return nil, fmt.Errorf("read first-seen parquet for epoch %d: %w", epoch, err)
	}

	committees, err := s.committeeProvider.FetchBeaconCommittees(epoch)
	if err != nil {
		return nil, fmt.Errorf("fetch beacon committees for epoch %d: %w", epoch, err)
	}
	assignments, err := firstSeenCommitteeAssignments(committees)
	if err != nil {
		return nil, fmt.Errorf("build first-seen committee assignments for epoch %d: %w", epoch, err)
	}

	votesByKey := make(map[firstSeenVoteKey]firstSeenVote)
	for _, row := range rows {
		if uint64(row.RawSeenMS) > s.deadlineMS {
			continue
		}
		if uint64(row.Epoch) != epoch {
			return nil, fmt.Errorf("first-seen row slot %d has epoch %d, expected %d", row.Slot, row.Epoch, epoch)
		}

		slot := uint64(row.Slot)
		assignment, ok := firstSeenCommitteeAssignmentForValidator(assignments, slot, uint64(row.ValidatorIndex))
		if !ok {
			return nil, fmt.Errorf("first-seen row slot %d validator %d has no beacon committee assignment", row.Slot, row.ValidatorIndex)
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

		key := firstSeenGroupKey{
			slot:            slot,
			beaconBlockRoot: beaconBlockRoot,
			sourceEpoch:     uint64(row.SourceEpoch),
			sourceRoot:      sourceRoot,
			targetEpoch:     uint64(row.TargetEpoch),
			targetRoot:      targetRoot,
		}
		voteKey := firstSeenVoteKey{
			vote:           key,
			validatorIndex: uint64(row.ValidatorIndex),
		}
		votesByKey[voteKey] = firstSeenVote{
			key:        voteKey,
			assignment: assignment,
			root:       syntheticFirstSeenAttestationRoot(voteKey, assignment),
		}
	}

	epochData := &firstSeenEpoch{slots: make(map[uint64][]attestationInfo)}
	sortedVotes := make([]firstSeenVote, 0, len(votesByKey))
	for _, vote := range votesByKey {
		sortedVotes = append(sortedVotes, vote)
	}
	sort.Slice(sortedVotes, func(i, j int) bool {
		return compareFirstSeenVoteKeys(sortedVotes[i].key, sortedVotes[j].key) < 0
	})
	for _, vote := range sortedVotes {
		attestation, err := firstSeenStandardAttestation(vote, forkAtSlot(vote.key.vote.slot))
		if err != nil {
			return nil, err
		}
		epochData.slots[vote.key.vote.slot] = append(epochData.slots[vote.key.vote.slot], attestation)
	}
	return epochData, nil
}

func firstSeenCommitteeAssignments(committees []beaconfetch.BeaconCommittee) (map[uint64]map[uint64]firstSeenCommitteeAssignment, error) {
	assignments := make(map[uint64]map[uint64]firstSeenCommitteeAssignment)
	for i, committee := range committees {
		if len(committee.Validators) == 0 {
			return nil, fmt.Errorf("committee %d at slot %d index %d has no validators", i, committee.Slot, committee.Index)
		}
		slotAssignments := assignments[committee.Slot]
		if slotAssignments == nil {
			slotAssignments = make(map[uint64]firstSeenCommitteeAssignment)
			assignments[committee.Slot] = slotAssignments
		}
		for position, validator := range committee.Validators {
			if _, exists := slotAssignments[validator]; exists {
				return nil, fmt.Errorf("validator %d has multiple committee assignments for slot %d", validator, committee.Slot)
			}
			slotAssignments[validator] = firstSeenCommitteeAssignment{
				committeeIndex: committee.Index,
				position:       uint64(position),
				committeeSize:  uint64(len(committee.Validators)),
			}
		}
	}
	return assignments, nil
}

func firstSeenCommitteeAssignmentForValidator(assignments map[uint64]map[uint64]firstSeenCommitteeAssignment, slot, validatorIndex uint64) (firstSeenCommitteeAssignment, bool) {
	slotAssignments := assignments[slot]
	if len(slotAssignments) == 0 {
		return firstSeenCommitteeAssignment{}, false
	}
	assignment, ok := slotAssignments[validatorIndex]
	return assignment, ok
}

func firstSeenStandardAttestation(vote firstSeenVote, fork string) (attestationInfo, error) {
	key := vote.key.vote
	aggregationBits, err := singleValidatorAggregationBits(vote.assignment.committeeSize, vote.assignment.position)
	if err != nil {
		return attestationInfo{}, fmt.Errorf("first-seen slot %d validator %d aggregation bits: %w", key.slot, vote.key.validatorIndex, err)
	}

	index := vote.assignment.committeeIndex
	var committeeBits *string
	if firstSeenUsesElectraEncoding(fork) {
		index = 0
		bits, err := singleCommitteeBits(vote.assignment.committeeIndex)
		if err != nil {
			return attestationInfo{}, fmt.Errorf("first-seen slot %d validator %d committee bits: %w", key.slot, vote.key.validatorIndex, err)
		}
		committeeBits = &bits
	}

	return attestationInfo{
		Root:            vote.root,
		AggregationBits: aggregationBits,
		CommitteeBits:   committeeBits,
		Slot:            key.slot,
		Index:           index,
		BeaconBlockRoot: key.beaconBlockRoot,
		SourceEpoch:     key.sourceEpoch,
		SourceRoot:      key.sourceRoot,
		TargetEpoch:     key.targetEpoch,
		TargetRoot:      key.targetRoot,
	}, nil
}

func singleValidatorAggregationBits(committeeSize, position uint64) (string, error) {
	if committeeSize == 0 {
		return "", fmt.Errorf("committee size is zero")
	}
	if position >= committeeSize {
		return "", fmt.Errorf("validator position %d outside committee size %d", position, committeeSize)
	}
	bits := bitfield.NewBitlist(committeeSize)
	bits.SetBitAt(position, true)
	return bytesHex(bits), nil
}

func singleCommitteeBits(committeeIndex uint64) (string, error) {
	bits := bitfield.NewBitvector64()
	if committeeIndex >= bits.Len() {
		return "", fmt.Errorf("committee index %d outside committee_bits size %d", committeeIndex, bits.Len())
	}
	bits.SetBitAt(committeeIndex, true)
	return bytesHex(bits), nil
}

func firstSeenUsesElectraEncoding(fork string) bool {
	switch strings.ToLower(strings.TrimSpace(fork)) {
	case "electra", "fulu":
		return true
	default:
		return false
	}
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

func syntheticFirstSeenAttestationRoot(key firstSeenVoteKey, assignment firstSeenCommitteeAssignment) [32]byte {
	h := sha256.New()
	writeUint64Decimal(h, key.vote.slot)
	writeUint64Decimal(h, key.validatorIndex)
	writeUint64Decimal(h, assignment.committeeIndex)
	writeUint64Decimal(h, assignment.position)
	writeUint64Decimal(h, assignment.committeeSize)
	h.Write(key.vote.beaconBlockRoot[:])
	writeUint64Decimal(h, key.vote.sourceEpoch)
	h.Write(key.vote.sourceRoot[:])
	writeUint64Decimal(h, key.vote.targetEpoch)
	h.Write(key.vote.targetRoot[:])
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

func compareFirstSeenVoteKeys(a, b firstSeenVoteKey) int {
	if a.vote.slot != b.vote.slot {
		return compareUint64(a.vote.slot, b.vote.slot)
	}
	if a.validatorIndex != b.validatorIndex {
		return compareUint64(a.validatorIndex, b.validatorIndex)
	}
	return compareFirstSeenGroupKeys(a.vote, b.vote)
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
