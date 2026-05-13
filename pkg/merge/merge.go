package merge

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ethpandaops/fcr-simulator/pkg/schema"
)

const maxJSONLRecordBytes = 16 * 1024 * 1024

var requiredEngineFields = []string{
	"slot",
	"epoch",
	"has_block",
	"block_root",
	"head_root",
	"confirmed_root",
	"confirmed_slot",
	"confirmation_delay_slots",
	"fast_confirmed",
	"strict_one_slot_confirmed",
	"finalized_epoch",
	"justified_epoch",
	"source_block_slot",
	"num_attestations_injected",
	"is_epoch_boundary",
	"is_missed_slot",
	"fcr_eval_duration_us",
}

var nonNullableEngineFields = map[string]struct{}{
	"slot":                      {},
	"epoch":                     {},
	"has_block":                 {},
	"head_root":                 {},
	"confirmed_root":            {},
	"confirmed_slot":            {},
	"confirmation_delay_slots":  {},
	"fast_confirmed":            {},
	"strict_one_slot_confirmed": {},
	"finalized_epoch":           {},
	"justified_epoch":           {},
	"num_attestations_injected": {},
	"is_epoch_boundary":         {},
	"is_missed_slot":            {},
	"fcr_eval_duration_us":      {},
}

var orchestratorAddedFields = []string{
	"schema_version",
	"engine_name",
	"engine_version",
	"engine_commit",
	"attestation_source_mode",
	"lookahead_cap",
}

var csvColumns = []string{
	"schema_version",
	"engine_name",
	"engine_version",
	"engine_commit",
	"slot",
	"epoch",
	"has_block",
	"block_root",
	"head_root",
	"confirmed_root",
	"confirmed_slot",
	"confirmation_delay_slots",
	"fast_confirmed",
	"strict_one_slot_confirmed",
	"finalized_epoch",
	"justified_epoch",
	"source_block_slot",
	"num_attestations_injected",
	"is_epoch_boundary",
	"is_missed_slot",
	"fcr_eval_duration_us",
	"attestation_source_mode",
	"lookahead_cap",
}

// Stats summarizes the merged output.
type Stats struct {
	TotalSlots         uint64
	FastConfirmedCount uint64
	FirstSlot          uint64
	LastSlot           uint64
}

// MergeAndWrite reads JSONL files from workerPaths, validates each record,
// enriches with orchestrator-added fields from meta, sorts by slot, and writes
// final JSONL and CSV outputs. Empty output paths are skipped.
//
// If expectedSlots is non-empty, MergeAndWrite returns an error unless every
// slot in that set is present exactly once across the worker outputs. This
// protects against engine workers silently exiting 0 with truncated output.
func MergeAndWrite(workerPaths []string, jsonlOut, csvOut string, meta schema.OrchestratorMetadata, expectedSlots []uint64) (Stats, error) {
	if jsonlOut == "" && csvOut == "" {
		return Stats{}, fmt.Errorf("at least one output path is required")
	}

	records, err := readWorkerRecords(workerPaths, meta)
	if err != nil {
		return Stats{}, err
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Slot < records[j].Slot
	})

	if len(expectedSlots) > 0 {
		seen := make(map[uint64]struct{}, len(records))
		for _, r := range records {
			seen[r.Slot] = struct{}{}
		}
		var missing []uint64
		for _, s := range expectedSlots {
			if _, ok := seen[s]; !ok {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			limit := len(missing)
			if limit > 10 {
				limit = 10
			}
			return Stats{}, fmt.Errorf("worker JSONL is missing %d expected slot(s) (first: %v); refusing to write incomplete output", len(missing), missing[:limit])
		}
	}

	stats := computeStats(records)

	if jsonlOut != "" {
		if err := writeJSONL(jsonlOut, records); err != nil {
			return Stats{}, err
		}
	}
	if csvOut != "" {
		if err := writeCSV(csvOut, records); err != nil {
			return Stats{}, err
		}
	}

	return stats, nil
}

func readWorkerRecords(workerPaths []string, meta schema.OrchestratorMetadata) ([]schema.Record, error) {
	var records []schema.Record
	seenSlots := make(map[uint64]string)

	for _, path := range workerPaths {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open worker JSONL %q: %w", path, err)
		}

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), maxJSONLRecordBytes)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			record, err := parseEngineRecord([]byte(line), path, lineNumber, meta)
			if err != nil {
				_ = file.Close()
				return nil, err
			}

			if previous, ok := seenSlots[record.Slot]; ok {
				_ = file.Close()
				return nil, fmt.Errorf("%s:%d: duplicate slot %d already seen in %s", path, lineNumber, record.Slot, previous)
			}
			seenSlots[record.Slot] = fmt.Sprintf("%s:%d", path, lineNumber)
			records = append(records, record)
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("read worker JSONL %q: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close worker JSONL %q: %w", path, err)
		}
	}

	return records, nil
}

func parseEngineRecord(line []byte, path string, lineNumber int, meta schema.OrchestratorMetadata) (schema.Record, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return schema.Record{}, fmt.Errorf("%s:%d: decode JSON record: %w", path, lineNumber, err)
	}

	for _, field := range requiredEngineFields {
		value, ok := raw[field]
		if !ok {
			return schema.Record{}, fmt.Errorf("%s:%d: missing required engine field %q", path, lineNumber, field)
		}
		if _, nonNullable := nonNullableEngineFields[field]; nonNullable && isJSONNull(value) {
			return schema.Record{}, fmt.Errorf("%s:%d: field %q must not be null", path, lineNumber, field)
		}
	}

	for _, field := range orchestratorAddedFields {
		if _, ok := raw[field]; ok {
			return schema.Record{}, fmt.Errorf("%s:%d: engine record must not include orchestrator-added field %q", path, lineNumber, field)
		}
	}

	var record schema.Record
	if err := json.Unmarshal(line, &record); err != nil {
		return schema.Record{}, fmt.Errorf("%s:%d: decode engine record fields: %w", path, lineNumber, err)
	}

	if record.Epoch != record.Slot/32 {
		return schema.Record{}, fmt.Errorf("%s:%d: epoch %d does not match slot %d", path, lineNumber, record.Epoch, record.Slot)
	}
	if record.IsEpochBoundary != (record.Slot%32 == 0) {
		return schema.Record{}, fmt.Errorf("%s:%d: is_epoch_boundary does not match slot %d", path, lineNumber, record.Slot)
	}
	if record.IsMissedSlot != !record.HasBlock {
		return schema.Record{}, fmt.Errorf("%s:%d: is_missed_slot does not match has_block", path, lineNumber)
	}
	if record.HasBlock && record.BlockRoot == nil {
		return schema.Record{}, fmt.Errorf("%s:%d: block_root must be set when has_block=true", path, lineNumber)
	}
	if !record.HasBlock && record.BlockRoot != nil {
		return schema.Record{}, fmt.Errorf("%s:%d: block_root must be null when has_block=false", path, lineNumber)
	}

	record.SchemaVersion = schema.SchemaVersion
	record.EngineName = meta.EngineName
	record.EngineVersion = meta.EngineVersion
	record.EngineCommit = meta.EngineCommit
	record.AttestationSourceMode = meta.AttestationSourceMode
	record.LookaheadCap = meta.LookaheadCap

	return record, nil
}

func isJSONNull(value json.RawMessage) bool {
	return strings.EqualFold(strings.TrimSpace(string(value)), "null")
}

func computeStats(records []schema.Record) Stats {
	stats := Stats{TotalSlots: uint64(len(records))}
	if len(records) == 0 {
		return stats
	}

	stats.FirstSlot = records[0].Slot
	stats.LastSlot = records[len(records)-1].Slot
	for _, record := range records {
		if record.FastConfirmed {
			stats.FastConfirmedCount++
		}
	}
	return stats
}

func writeJSONL(path string, records []schema.Record) error {
	return writeAtomically(path, func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		for _, record := range records {
			if err := encoder.Encode(record); err != nil {
				return fmt.Errorf("encode JSONL record for slot %d: %w", record.Slot, err)
			}
		}
		return nil
	})
}

func writeCSV(path string, records []schema.Record) error {
	return writeAtomically(path, func(w io.Writer) error {
		if _, err := io.WriteString(w, schema.CSVSchemaMarker+"\n"); err != nil {
			return err
		}

		cw := csv.NewWriter(w)
		if err := cw.Write(csvColumns); err != nil {
			return err
		}
		for _, record := range records {
			if err := cw.Write(recordCSVFields(record)); err != nil {
				return err
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
		return nil
	})
}

func recordCSVFields(record schema.Record) []string {
	blockRoot := ""
	if record.BlockRoot != nil {
		blockRoot = *record.BlockRoot
	}

	sourceBlockSlot := ""
	if record.SourceBlockSlot != nil {
		sourceBlockSlot = strconv.FormatUint(*record.SourceBlockSlot, 10)
	}

	return []string{
		strconv.Itoa(record.SchemaVersion),
		record.EngineName,
		record.EngineVersion,
		record.EngineCommit,
		strconv.FormatUint(record.Slot, 10),
		strconv.FormatUint(record.Epoch, 10),
		strconv.FormatBool(record.HasBlock),
		blockRoot,
		record.HeadRoot,
		record.ConfirmedRoot,
		strconv.FormatUint(record.ConfirmedSlot, 10),
		strconv.FormatUint(record.ConfirmationDelaySlots, 10),
		strconv.FormatBool(record.FastConfirmed),
		strconv.FormatBool(record.StrictOneSlotConfirmed),
		strconv.FormatUint(record.FinalizedEpoch, 10),
		strconv.FormatUint(record.JustifiedEpoch, 10),
		sourceBlockSlot,
		strconv.FormatUint(record.NumAttestationsInjected, 10),
		strconv.FormatBool(record.IsEpochBoundary),
		strconv.FormatBool(record.IsMissedSlot),
		strconv.FormatUint(record.FcrEvalDurationUs, 10),
		record.AttestationSourceMode,
		strconv.FormatUint(record.LookaheadCap, 10),
	}
}

func writeAtomically(path string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory for %q: %w", path, err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %q: %w", path, err)
	}
	tempPath := tempFile.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := write(tempFile); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync %q: %w", tempPath, err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}

	keepTemp = true
	return nil
}
