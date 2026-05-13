package era

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/golang/snappy"
	lru "github.com/hashicorp/golang-lru/v2"
)

const rawBlockCacheSize = 64

// Reader provides lazy access to blocks from an ERA file cache.
type Reader struct {
	cacheDir string

	mu    sync.Mutex
	eras  map[uint64]*eraIndex
	cache *lru.Cache[uint64, []byte]
}

type eraIndex struct {
	path    string
	blocks  map[uint64]recordRef
	numRecs int
}

type recordRef struct {
	offset int64
	length uint32
}

// New returns a Reader backed by the given on-disk cache directory.
//
// Files are expected to be named like "mainnet-NNNNN-*.era". Downloader
// populates this cache; Reader only reads from it.
func New(cacheDir string) (*Reader, error) {
	info, err := os.Stat(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("stat ERA cache directory %q: %w", cacheDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ERA cache path %q is not a directory", cacheDir)
	}

	cache, err := lru.New[uint64, []byte](rawBlockCacheSize)
	if err != nil {
		return nil, fmt.Errorf("create ERA block cache: %w", err)
	}

	return &Reader{
		cacheDir: cacheDir,
		eras:     make(map[uint64]*eraIndex),
		cache:    cache,
	}, nil
}

// RawBlockSSZ returns the snappy-decompressed SSZ bytes for the block at slot.
//
// It returns (nil, false, nil) if the slot is missed, and
// (nil, false, err) on I/O or parse errors.
func (r *Reader) RawBlockSSZ(slot uint64) ([]byte, bool, error) {
	if r == nil {
		return nil, false, fmt.Errorf("nil ERA reader")
	}

	if cached, ok := r.cache.Get(slot); ok {
		return cloneBytes(cached), true, nil
	}

	eraNumber := EraNumberForSlot(slot)
	r.mu.Lock()
	index, err := r.ensureEraIndexLocked(eraNumber)
	if err != nil {
		r.mu.Unlock()
		return nil, false, err
	}
	ref, ok := index.blocks[slot]
	path := index.path
	r.mu.Unlock()
	if !ok {
		return nil, false, nil
	}

	compressed, err := readRecordDataAt(path, ref)
	if err != nil {
		return nil, false, err
	}

	ssz, err := decompressSnappy(compressed)
	if err != nil {
		return nil, false, err
	}

	r.cache.Add(slot, ssz)
	return cloneBytes(ssz), true, nil
}

// BlockExists checks if a block exists at the slot without decompressing it
// beyond the one-time era indexing pass.
func (r *Reader) BlockExists(slot uint64) (bool, error) {
	if r == nil {
		return false, fmt.Errorf("nil ERA reader")
	}

	eraNumber := EraNumberForSlot(slot)
	r.mu.Lock()
	defer r.mu.Unlock()

	index, err := r.ensureEraIndexLocked(eraNumber)
	if err != nil {
		return false, err
	}

	_, ok := index.blocks[slot]
	return ok, nil
}

// PreloadEras opens and indexes every era in [startEra, endEra] inclusive.
func (r *Reader) PreloadEras(startEra, endEra uint64) error {
	if r == nil {
		return fmt.Errorf("nil ERA reader")
	}
	if startEra > endEra {
		return fmt.Errorf("start era %d is after end era %d", startEra, endEra)
	}

	for eraNumber := startEra; ; eraNumber++ {
		r.mu.Lock()
		_, err := r.ensureEraIndexLocked(eraNumber)
		r.mu.Unlock()
		if err != nil {
			return err
		}
		if eraNumber == endEra {
			break
		}
	}

	return nil
}

func (r *Reader) ensureEraIndexLocked(eraNumber uint64) (*eraIndex, error) {
	if index, ok := r.eras[eraNumber]; ok {
		return index, nil
	}

	index, err := r.buildEraIndex(eraNumber)
	if err != nil {
		return nil, err
	}
	r.eras[eraNumber] = index
	return index, nil
}

func (r *Reader) buildEraIndex(eraNumber uint64) (*eraIndex, error) {
	path, err := findEraFile(r.cacheDir, eraNumber)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ERA file %q: %w", path, err)
	}
	defer file.Close()

	var pos int64
	version, ok, err := readRecordHeader(file)
	if err != nil {
		return nil, fmt.Errorf("read version record header from %q: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("missing version record in ERA file %q", path)
	}
	pos += recordHeaderLen
	if version.recordType != typeVersion {
		return nil, fmt.Errorf("expected version record (0x%04x), got 0x%04x in %q", typeVersion, version.recordType, path)
	}
	if err := skipRecordData(file, version.length); err != nil {
		return nil, fmt.Errorf("failed to read version record data from %q: %w", path, err)
	}
	pos += int64(version.length)

	index := &eraIndex{
		path:   path,
		blocks: make(map[uint64]recordRef),
	}

	for {
		header, ok, err := readRecordHeader(file)
		if err != nil {
			return nil, fmt.Errorf("read record header from %q: %w", path, err)
		}
		if !ok {
			break
		}
		pos += recordHeaderLen
		dataOffset := pos
		index.numRecs++

		switch header.recordType {
		case typeCompressedSignedBeaconBlock:
			compressed := make([]byte, int(header.length))
			if _, err := io.ReadFull(file, compressed); err != nil {
				return nil, fmt.Errorf("failed to read block record data from %q at offset %d: %w", path, dataOffset, err)
			}
			pos += int64(header.length)

			ssz, err := decompressSnappy(compressed)
			if err != nil {
				return nil, fmt.Errorf("decompress block record from %q at offset %d: %w", path, dataOffset, err)
			}
			slot, err := extractSlotFromSignedBeaconBlockSSZ(ssz)
			if err != nil {
				return nil, fmt.Errorf("extract block slot from %q at offset %d: %w", path, dataOffset, err)
			}

			index.blocks[slot] = recordRef{offset: dataOffset, length: header.length}
		case typeCompressedBeaconState, typeSlotIndex, typeEmpty:
			n, err := skipRecordDataN(file, header.length)
			pos += n
			if err != nil {
				return nil, fmt.Errorf("failed to skip record type 0x%04x from %q at offset %d: %w", header.recordType, path, dataOffset, err)
			}
		default:
			n, err := skipRecordDataN(file, header.length)
			pos += n
			if err != nil {
				return nil, fmt.Errorf("failed to skip unknown record type 0x%04x from %q at offset %d: %w", header.recordType, path, dataOffset, err)
			}
		}
	}

	return index, nil
}

func readRecordDataAt(path string, ref recordRef) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ERA file %q: %w", path, err)
	}
	defer file.Close()

	data := make([]byte, int(ref.length))
	if _, err := file.ReadAt(data, ref.offset); err != nil {
		return nil, fmt.Errorf("read compressed block from %q at offset %d: %w", path, ref.offset, err)
	}
	return data, nil
}

func decompressSnappy(data []byte) ([]byte, error) {
	reader := snappy.NewReader(bytes.NewReader(data))
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("snappy decompression failed: %w", err)
	}
	return decompressed, nil
}

func skipRecordData(r io.Reader, length uint32) error {
	_, err := skipRecordDataN(r, length)
	return err
}

func skipRecordDataN(r io.Reader, length uint32) (int64, error) {
	return io.CopyN(io.Discard, r, int64(length))
}

func findEraFile(cacheDir string, eraNumber uint64) (string, error) {
	path, ok, err := findCachedFile(cacheDir, eraFilePrefix(eraNumber))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("ERA file not found for era number %d in %q", eraNumber, cacheDir)
	}
	return path, nil
}

func findCachedFile(cacheDir, pattern string) (string, bool, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return "", false, nil
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, pattern) && strings.HasSuffix(name, ".era") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", false, nil
	}
	sort.Strings(names)
	return filepath.Join(cacheDir, names[0]), true, nil
}

func eraFilePrefix(eraNumber uint64) string {
	return fmt.Sprintf("mainnet-%05d-", eraNumber)
}

func cloneBytes(data []byte) []byte {
	cloned := make([]byte, len(data))
	copy(cloned, data)
	return cloned
}
