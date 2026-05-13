package schema

// Record matches SCHEMA_V3.md's full set of fields.
type Record struct {
	SchemaVersion           int     `json:"schema_version"`
	EngineName              string  `json:"engine_name"`
	EngineVersion           string  `json:"engine_version"`
	EngineCommit            string  `json:"engine_commit"`
	Slot                    uint64  `json:"slot"`
	Epoch                   uint64  `json:"epoch"`
	HasBlock                bool    `json:"has_block"`
	BlockRoot               *string `json:"block_root"`
	HeadRoot                string  `json:"head_root"`
	ConfirmedRoot           string  `json:"confirmed_root"`
	ConfirmedSlot           uint64  `json:"confirmed_slot"`
	ConfirmationDelaySlots  uint64  `json:"confirmation_delay_slots"`
	FastConfirmed           bool    `json:"fast_confirmed"`
	StrictOneSlotConfirmed  bool    `json:"strict_one_slot_confirmed"`
	FinalizedEpoch          uint64  `json:"finalized_epoch"`
	JustifiedEpoch          uint64  `json:"justified_epoch"`
	SourceBlockSlot         *uint64 `json:"source_block_slot"`
	NumAttestationsInjected uint64  `json:"num_attestations_injected"`
	IsEpochBoundary         bool    `json:"is_epoch_boundary"`
	IsMissedSlot            bool    `json:"is_missed_slot"`
	FcrEvalDurationUs       uint64  `json:"fcr_eval_duration_us"`
	AttestationSourceMode   string  `json:"attestation_source_mode"`
	LookaheadCap            uint64  `json:"lookahead_cap"`
}

type OrchestratorMetadata struct {
	EngineName            string
	EngineVersion         string
	EngineCommit          string
	AttestationSourceMode string
	LookaheadCap          uint64
}

const CSVSchemaMarker = "# fcr-simulator-csv-schema-version:3"
const SchemaVersion = 3
