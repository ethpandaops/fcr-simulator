package beaconapi

import (
	"encoding/binary"
	"fmt"
	"strings"

	eth2spec "github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
)

const (
	signedBeaconBlockMessageOffset = 100
	slotSize                       = 8
)

type blockInfo struct {
	Slot         uint64
	Root         [32]byte
	ParentRoot   [32]byte
	Attestations []attestationInfo
}

type attestationInfo struct {
	Slot            uint64
	BeaconBlockRoot [32]byte
	TargetRoot      [32]byte
}

func parseBlockInfo(ssz []byte, forkAtSlot func(uint64) string) (blockInfo, error) {
	slot, err := extractSlotFromSignedBeaconBlockSSZ(ssz)
	if err != nil {
		return blockInfo{}, err
	}

	version, err := eth2spec.DataVersionFromString(strings.ToLower(forkAtSlot(slot)))
	if err != nil {
		return blockInfo{}, fmt.Errorf("resolve data version for slot %d: %w", slot, err)
	}

	block, err := decodeSignedBeaconBlockSSZ(ssz, version)
	if err != nil {
		return blockInfo{}, err
	}

	root, err := block.Root()
	if err != nil {
		return blockInfo{}, fmt.Errorf("calculate block root for slot %d: %w", slot, err)
	}
	parentRoot, err := block.ParentRoot()
	if err != nil {
		return blockInfo{}, fmt.Errorf("read parent root for slot %d: %w", slot, err)
	}

	attestations, err := block.Attestations()
	if err != nil {
		return blockInfo{}, fmt.Errorf("read attestations for slot %d: %w", slot, err)
	}

	infos := make([]attestationInfo, 0, len(attestations))
	for _, attestation := range attestations {
		data, err := attestation.Data()
		if err != nil {
			return blockInfo{}, fmt.Errorf("read attestation data for block slot %d: %w", slot, err)
		}
		if data == nil || data.Target == nil {
			return blockInfo{}, fmt.Errorf("attestation in block slot %d has nil data or target", slot)
		}
		infos = append(infos, attestationInfo{
			Slot:            uint64(data.Slot),
			BeaconBlockRoot: [32]byte(data.BeaconBlockRoot),
			TargetRoot:      [32]byte(data.Target.Root),
		})
	}

	return blockInfo{
		Slot:         slot,
		Root:         [32]byte(root),
		ParentRoot:   [32]byte(parentRoot),
		Attestations: infos,
	}, nil
}

func decodeSignedBeaconBlockSSZ(ssz []byte, version eth2spec.DataVersion) (*eth2spec.VersionedSignedBeaconBlock, error) {
	switch version {
	case eth2spec.DataVersionPhase0:
		block := &phase0.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode phase0 signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Phase0: block}, nil
	case eth2spec.DataVersionAltair:
		block := &altair.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode altair signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Altair: block}, nil
	case eth2spec.DataVersionBellatrix:
		block := &bellatrix.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode bellatrix signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Bellatrix: block}, nil
	case eth2spec.DataVersionCapella:
		block := &capella.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode capella signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Capella: block}, nil
	case eth2spec.DataVersionDeneb:
		block := &deneb.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode deneb signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Deneb: block}, nil
	case eth2spec.DataVersionElectra:
		block := &electra.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode electra signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Electra: block}, nil
	case eth2spec.DataVersionFulu:
		block := &electra.SignedBeaconBlock{}
		if err := block.UnmarshalSSZ(ssz); err != nil {
			return nil, fmt.Errorf("decode fulu signed beacon block SSZ: %w", err)
		}
		return &eth2spec.VersionedSignedBeaconBlock{Version: version, Fulu: block}, nil
	default:
		return nil, fmt.Errorf("unsupported signed beacon block data version %s", version)
	}
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
