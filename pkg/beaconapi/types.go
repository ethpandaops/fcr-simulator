package beaconapi

import "errors"

// ErrNotFound is returned when a block or state is not present.
var ErrNotFound = errors.New("not found")

// Backend is the data source for the HTTP server.
//
// Implementations keep the HTTP layer pure and testable: production code wires
// era, beaconfetch, and attplan together, while tests can return canned data.
type Backend interface {
	// BlockSSZBySlot returns the SSZ-encoded block at the given slot.
	// Returns (nil, ErrNotFound) if the slot is missed or out of range.
	BlockSSZBySlot(slot uint64) ([]byte, error)

	// BlockSSZByRoot returns the SSZ-encoded block by its hash_tree_root.
	// Only checkpoint blocks pre-fetched by the orchestrator are addressable
	// by root. Returns (nil, ErrNotFound) otherwise.
	BlockSSZByRoot(root [32]byte) ([]byte, error)

	// StateSSZBySlot returns the SSZ-encoded state at the given slot.
	// Only worker checkpoint states are cached. Returns (nil, ErrNotFound)
	// for any other slot.
	StateSSZBySlot(slot uint64) ([]byte, error)

	// GenesisStateSSZ returns the SSZ-encoded genesis state.
	GenesisStateSSZ() ([]byte, error)

	// GenesisInfo returns the JSON-encodable genesis info.
	GenesisInfo() (GenesisInfo, error)

	// ConsensusVersionAtSlot returns the fork name for the slot, lowercase.
	ConsensusVersionAtSlot(slot uint64) string

	// BuildPlan returns the attestation source plan for sim slots [from, to).
	BuildPlan(from, to uint64) ([]PlanEntry, error)
}

type GenesisInfo struct {
	GenesisTime           uint64 `json:"genesis_time,string"`
	GenesisValidatorsRoot string `json:"genesis_validators_root"`
	GenesisForkVersion    string `json:"genesis_fork_version"`
}

type PlanEntry struct {
	SimSlot            uint64                  `json:"sim_slot"`
	EvalSlot           uint64                  `json:"eval_slot"`
	ImportBlocks       []PlanBlockImport       `json:"import_blocks"`
	AttestationSources []PlanAttestationSource `json:"attestation_sources"`
	// SourceBlockSlot is the legacy representative source, kept for output
	// compatibility while engines migrate to AttestationSources.
	SourceBlockSlot *uint64 `json:"source_block_slot"`
}

type PlanBlockImport struct {
	Slot      uint64 `json:"slot"`
	Root      string `json:"root"`
	Canonical bool   `json:"canonical"`
}

type PlanAttestationSource struct {
	Slot uint64 `json:"slot"`
	// MaxAttestationSlot is set for greedy-lookahead. Nil means the engine
	// should preserve the old mode behavior and inject the source block as-is.
	MaxAttestationSlot *uint64 `json:"max_attestation_slot"`
}
