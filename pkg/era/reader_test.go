package era

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/snappy"
	"github.com/stretchr/testify/require"
)

type fakeBlock struct {
	slot uint64
}

func TestEraNumberForSlot(t *testing.T) {
	require.Equal(t, uint64(1), EraNumberForSlot(0))
	require.Equal(t, uint64(1), EraNumberForSlot(8191))
	require.Equal(t, uint64(2), EraNumberForSlot(8192))
	require.Equal(t, uint64(13), EraNumberForSlot(100000))
}

func TestRecordHeaderParsing(t *testing.T) {
	var header [recordHeaderLen]byte
	binary.LittleEndian.PutUint16(header[0:2], typeCompressedSignedBeaconBlock)
	binary.LittleEndian.PutUint32(header[2:6], 0x01020304)
	header[6] = 0xaa
	header[7] = 0xbb

	parsed, err := parseRecordHeader(header[:])
	require.NoError(t, err)
	require.Equal(t, typeCompressedSignedBeaconBlock, parsed.recordType)
	require.Equal(t, uint32(0x01020304), parsed.length)
}

func TestRecordHeaderParsingTooShort(t *testing.T) {
	_, err := parseRecordHeader([]byte{0x01, 0x00})
	require.Error(t, err)
}

func TestSnappyDecompress(t *testing.T) {
	// This is a valid Snappy framed stream from github.com/golang/snappy's
	// framing tests: stream identifier + one uncompressed data chunk for "abcd".
	compressed := []byte{
		0xff, 0x06, 0x00, 0x00, 's', 'N', 'a', 'P', 'p', 'Y',
		0x01, 0x08, 0x00, 0x00,
		0x68, 0x10, 0xe6, 0xb6,
		'a', 'b', 'c', 'd',
	}

	got, err := decompressSnappy(compressed)
	require.NoError(t, err)
	require.Equal(t, []byte("abcd"), got)
}

func TestSlotExtractionFromSSZ(t *testing.T) {
	ssz := encodeFakeSignedBeaconBlock(123456789)

	slot, err := extractSlotFromSignedBeaconBlockSSZ(ssz)
	require.NoError(t, err)
	require.Equal(t, uint64(123456789), slot)
}

func TestSlotExtractionFromSSZErrors(t *testing.T) {
	_, err := extractSlotFromSignedBeaconBlockSSZ(make([]byte, signedBeaconBlockMessageOffset+slotSize-1))
	require.Error(t, err)

	ssz := encodeFakeSignedBeaconBlock(1)
	binary.LittleEndian.PutUint32(ssz[0:4], 96)
	_, err = extractSlotFromSignedBeaconBlockSSZ(ssz)
	require.Error(t, err)
}

func TestNewReaderValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	_, err := New(path)
	require.Error(t, err)
}

func TestReaderMissedSlot(t *testing.T) {
	cacheDir := t.TempDir()
	writeFakeEraFile(t, cacheDir, 1, []fakeBlock{{slot: 0}, {slot: 2}})

	reader, err := New(cacheDir)
	require.NoError(t, err)

	data, present, err := reader.RawBlockSSZ(1)
	require.NoError(t, err)
	require.False(t, present)
	require.Nil(t, data)
}

func TestReaderMissingEraFile(t *testing.T) {
	reader, err := New(t.TempDir())
	require.NoError(t, err)

	data, present, err := reader.RawBlockSSZ(0)
	require.Error(t, err)
	require.False(t, present)
	require.Nil(t, data)
}

func TestReaderRawBlockSSZ(t *testing.T) {
	cacheDir := t.TempDir()
	writeFakeEraFile(t, cacheDir, 1, []fakeBlock{{slot: 7}})

	reader, err := New(cacheDir)
	require.NoError(t, err)

	got, present, err := reader.RawBlockSSZ(7)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, encodeFakeSignedBeaconBlock(7), got)

	got[100] = 0xff
	again, present, err := reader.RawBlockSSZ(7)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, encodeFakeSignedBeaconBlock(7), again)
}

func TestBlockExists(t *testing.T) {
	cacheDir := t.TempDir()
	writeFakeEraFile(t, cacheDir, 1, []fakeBlock{{slot: 0}, {slot: 2}})

	reader, err := New(cacheDir)
	require.NoError(t, err)

	exists, err := reader.BlockExists(2)
	require.NoError(t, err)
	require.True(t, exists)

	exists, err = reader.BlockExists(1)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestPreloadEras(t *testing.T) {
	cacheDir := t.TempDir()
	writeFakeEraFile(t, cacheDir, 1, []fakeBlock{{slot: 0}})
	writeFakeEraFile(t, cacheDir, 2, []fakeBlock{{slot: SlotsPerEra}})

	reader, err := New(cacheDir)
	require.NoError(t, err)

	require.NoError(t, reader.PreloadEras(1, 2))
	require.Len(t, reader.eras, 2)
}

func TestReaderLRUEviction(t *testing.T) {
	cacheDir := t.TempDir()
	blocks := make([]fakeBlock, 0, rawBlockCacheSize+1)
	for slot := uint64(0); slot < rawBlockCacheSize+1; slot++ {
		blocks = append(blocks, fakeBlock{slot: slot})
	}
	writeFakeEraFile(t, cacheDir, 1, blocks)

	reader, err := New(cacheDir)
	require.NoError(t, err)

	for slot := uint64(0); slot < rawBlockCacheSize+1; slot++ {
		_, present, err := reader.RawBlockSSZ(slot)
		require.NoError(t, err)
		require.True(t, present)
	}

	require.False(t, reader.cache.Contains(0))
	require.True(t, reader.cache.Contains(rawBlockCacheSize))
}

func TestReaderIntegrationFixture(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("testdata", "*.era"))
	require.NoError(t, err)
	if len(matches) == 0 {
		t.Skip("TODO: add a small real ERA fixture under pkg/era/testdata")
	}

	reader, err := New("testdata")
	require.NoError(t, err)
	require.NoError(t, reader.PreloadEras(1, 1))
}

func makeFakeEra(t *testing.T, blocks []fakeBlock) []byte {
	t.Helper()

	var buf bytes.Buffer
	writeRecord(t, &buf, typeVersion, nil)
	writeRecord(t, &buf, typeCompressedBeaconState, []byte("state"))
	writeRecord(t, &buf, typeSlotIndex, []byte("index"))
	writeRecord(t, &buf, typeEmpty, nil)

	for _, block := range blocks {
		ssz := encodeFakeSignedBeaconBlock(block.slot)
		writeRecord(t, &buf, typeCompressedSignedBeaconBlock, snappyCompress(t, ssz))
	}

	writeRecord(t, &buf, 0x9999, []byte("unknown"))
	return buf.Bytes()
}

func writeFakeEraFile(t *testing.T, dir string, eraNumber uint64, blocks []fakeBlock) string {
	t.Helper()

	path := filepath.Join(dir, fmt.Sprintf("mainnet-%05d-deadbeef.era", eraNumber))
	require.NoError(t, os.WriteFile(path, makeFakeEra(t, blocks), 0o644))
	return path
}

func writeRecord(t *testing.T, buf *bytes.Buffer, recordType uint16, data []byte) {
	t.Helper()

	var header [recordHeaderLen]byte
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

func encodeFakeSignedBeaconBlock(slot uint64) []byte {
	ssz := make([]byte, signedBeaconBlockMessageOffset+slotSize)
	binary.LittleEndian.PutUint32(ssz[0:4], signedBeaconBlockMessageOffset)
	binary.LittleEndian.PutUint64(ssz[signedBeaconBlockMessageOffset:signedBeaconBlockMessageOffset+slotSize], slot)
	return ssz
}
