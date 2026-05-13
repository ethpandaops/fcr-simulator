package era

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	typeVersion                     uint16 = 0x3265
	typeCompressedSignedBeaconBlock uint16 = 0x0001
	typeCompressedBeaconState       uint16 = 0x0002
	typeSlotIndex                   uint16 = 0x3269
	typeEmpty                       uint16 = 0x0000

	recordHeaderLen = 8

	signedBeaconBlockMessageOffset = 100
	slotSize                       = 8
)

// SlotsPerEra is the number of beacon slots covered by one ERA file.
const SlotsPerEra uint64 = 8192

type recordHeader struct {
	recordType uint16
	length     uint32
}

func parseRecordHeader(header []byte) (recordHeader, error) {
	if len(header) < recordHeaderLen {
		return recordHeader{}, fmt.Errorf("record header too short: got %d bytes, want %d", len(header), recordHeaderLen)
	}

	return recordHeader{
		recordType: binary.LittleEndian.Uint16(header[0:2]),
		length:     binary.LittleEndian.Uint32(header[2:6]),
	}, nil
}

func readRecordHeader(r io.Reader) (recordHeader, bool, error) {
	var header [recordHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return recordHeader{}, false, nil
		}
		return recordHeader{}, false, err
	}

	parsed, err := parseRecordHeader(header[:])
	if err != nil {
		return recordHeader{}, false, err
	}
	return parsed, true, nil
}

// EraNumberForSlot returns the era file number containing a slot.
//
// ERA 0 is genesis-only. Era N (N>=1) contains slots
// [(N-1)*8192, N*8192-1].
func EraNumberForSlot(slot uint64) uint64 {
	return slot/SlotsPerEra + 1
}

func extractSlotFromSignedBeaconBlockSSZ(ssz []byte) (uint64, error) {
	if len(ssz) < signedBeaconBlockMessageOffset+slotSize {
		return 0, fmt.Errorf("SignedBeaconBlock SSZ too short: got %d bytes, need at least %d", len(ssz), signedBeaconBlockMessageOffset+slotSize)
	}

	messageOffset := binary.LittleEndian.Uint32(ssz[0:4])
	if messageOffset != signedBeaconBlockMessageOffset {
		return 0, fmt.Errorf("unexpected SignedBeaconBlock message offset: got %d, want %d", messageOffset, signedBeaconBlockMessageOffset)
	}

	return binary.LittleEndian.Uint64(ssz[signedBeaconBlockMessageOffset : signedBeaconBlockMessageOffset+slotSize]), nil
}
