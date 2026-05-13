package merge

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethpandaops/fcr-simulator/pkg/schema"
)

func TestMergeAndWriteEnrichesSortsAndWritesOutputs(t *testing.T) {
	dir := t.TempDir()
	worker0 := filepath.Join(dir, "worker-0.jsonl")
	worker1 := filepath.Join(dir, "worker-1.jsonl")
	jsonlOut := filepath.Join(dir, "results.jsonl")
	csvOut := filepath.Join(dir, "results.csv")

	writeFile(t, worker0, validEngineRecordJSON(2, true)+"\n")
	writeFile(t, worker1, validEngineRecordJSON(0, true)+"\n"+validEngineRecordJSON(1, false)+"\n")

	meta := schema.OrchestratorMetadata{
		EngineName:            "lighthouse",
		EngineVersion:         "5.4.0",
		EngineCommit:          "abc123",
		AttestationSourceMode: "next-non-missed",
		LookaheadCap:          4,
	}

	stats, err := MergeAndWrite([]string{worker0, worker1}, jsonlOut, csvOut, meta, nil)
	if err != nil {
		t.Fatalf("MergeAndWrite() error = %v", err)
	}
	if stats.TotalSlots != 3 || stats.FastConfirmedCount != 2 || stats.FirstSlot != 0 || stats.LastSlot != 2 {
		t.Fatalf("unexpected stats: %#v", stats)
	}

	records := readJSONLRecords(t, jsonlOut)
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	for i, record := range records {
		if record.Slot != uint64(i) {
			t.Fatalf("record %d slot = %d, want %d", i, record.Slot, i)
		}
		if record.SchemaVersion != schema.SchemaVersion {
			t.Fatalf("record schema_version = %d, want %d", record.SchemaVersion, schema.SchemaVersion)
		}
		if record.EngineName != meta.EngineName || record.EngineVersion != meta.EngineVersion || record.EngineCommit != meta.EngineCommit {
			t.Fatalf("record metadata not enriched: %#v", record)
		}
		if record.AttestationSourceMode != meta.AttestationSourceMode || record.LookaheadCap != meta.LookaheadCap {
			t.Fatalf("record mode metadata not enriched: %#v", record)
		}
	}

	csvFile, err := os.Open(csvOut)
	if err != nil {
		t.Fatalf("open CSV: %v", err)
	}
	defer csvFile.Close()

	scanner := bufio.NewScanner(csvFile)
	if !scanner.Scan() {
		t.Fatal("CSV is empty")
	}
	if got := scanner.Text(); got != schema.CSVSchemaMarker {
		t.Fatalf("CSV marker = %q, want %q", got, schema.CSVSchemaMarker)
	}

	rest := strings.Builder{}
	for scanner.Scan() {
		rest.WriteString(scanner.Text())
		rest.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan CSV: %v", err)
	}

	rows, err := csv.NewReader(strings.NewReader(rest.String())).ReadAll()
	if err != nil {
		t.Fatalf("decode CSV: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("CSV rows = %d, want 4", len(rows))
	}
	if rows[0][0] != "schema_version" || rows[0][len(rows[0])-1] != "lookahead_cap" {
		t.Fatalf("unexpected CSV header: %#v", rows[0])
	}
	if rows[1][4] != "0" || rows[2][4] != "1" || rows[3][4] != "2" {
		t.Fatalf("CSV rows not sorted by slot: %#v", rows[1:])
	}
}

func TestMergeAndWriteRejectsMalformedRecords(t *testing.T) {
	dir := t.TempDir()
	meta := schema.OrchestratorMetadata{EngineName: "lighthouse", AttestationSourceMode: "next-non-missed", LookaheadCap: 4}

	tests := []struct {
		name    string
		record  string
		wantErr string
	}{
		{
			name:    "missing required field",
			record:  `{"epoch":0}`,
			wantErr: `missing required engine field "slot"`,
		},
		{
			name:    "orchestrator field present",
			record:  strings.TrimSuffix(validEngineRecordJSON(0, true), "}") + `,"schema_version":3}`,
			wantErr: `must not include orchestrator-added field "schema_version"`,
		},
		{
			name:    "null non nullable",
			record:  strings.Replace(validEngineRecordJSON(0, true), `"slot":0`, `"slot":null`, 1),
			wantErr: `field "slot" must not be null`,
		},
		{
			name:    "bad consistency",
			record:  strings.Replace(validEngineRecordJSON(0, true), `"is_epoch_boundary":true`, `"is_epoch_boundary":false`, 1),
			wantErr: "is_epoch_boundary does not match",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			worker := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".jsonl")
			writeFile(t, worker, tc.record+"\n")
			_, err := MergeAndWrite([]string{worker}, filepath.Join(dir, tc.name+".jsonl"), "", meta, nil)
			if err == nil {
				t.Fatal("MergeAndWrite() error = nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestMergeAndWriteRejectsDuplicateSlots(t *testing.T) {
	dir := t.TempDir()
	worker0 := filepath.Join(dir, "worker-0.jsonl")
	worker1 := filepath.Join(dir, "worker-1.jsonl")
	writeFile(t, worker0, validEngineRecordJSON(0, true)+"\n")
	writeFile(t, worker1, validEngineRecordJSON(0, true)+"\n")

	_, err := MergeAndWrite(
		[]string{worker0, worker1},
		filepath.Join(dir, "results.jsonl"),
		"",
		schema.OrchestratorMetadata{EngineName: "lighthouse", AttestationSourceMode: "next-non-missed", LookaheadCap: 4},
		nil,
	)
	if err == nil {
		t.Fatal("MergeAndWrite() error = nil, want duplicate slot error")
	}
	if !strings.Contains(err.Error(), "duplicate slot 0") {
		t.Fatalf("error = %q, want duplicate slot", err.Error())
	}
}

func TestMergeAndWriteEmptyInputs(t *testing.T) {
	dir := t.TempDir()
	jsonlOut := filepath.Join(dir, "empty.jsonl")
	csvOut := filepath.Join(dir, "empty.csv")

	stats, err := MergeAndWrite(
		nil,
		jsonlOut,
		csvOut,
		schema.OrchestratorMetadata{EngineName: "lighthouse", AttestationSourceMode: "next-non-missed", LookaheadCap: 4},
		nil,
	)
	if err != nil {
		t.Fatalf("MergeAndWrite() error = %v", err)
	}
	if stats != (Stats{}) {
		t.Fatalf("stats = %#v, want zero", stats)
	}

	if records := readJSONLRecords(t, jsonlOut); len(records) != 0 {
		t.Fatalf("JSONL records = %d, want 0", len(records))
	}
	data, err := os.ReadFile(csvOut)
	if err != nil {
		t.Fatalf("ReadFile(CSV) error = %v", err)
	}
	if !strings.HasPrefix(string(data), schema.CSVSchemaMarker+"\n") {
		t.Fatalf("CSV missing schema marker: %q", string(data))
	}
}

func TestMergeAndWriteOutputAndInputErrors(t *testing.T) {
	dir := t.TempDir()
	meta := schema.OrchestratorMetadata{EngineName: "lighthouse", AttestationSourceMode: "next-non-missed", LookaheadCap: 4}

	if _, err := MergeAndWrite(nil, "", "", meta, nil); err == nil {
		t.Fatal("MergeAndWrite() with no outputs error = nil")
	}

	if _, err := MergeAndWrite([]string{filepath.Join(dir, "missing.jsonl")}, filepath.Join(dir, "out.jsonl"), "", meta, nil); err == nil {
		t.Fatal("MergeAndWrite() with missing worker error = nil")
	}

	worker := filepath.Join(dir, "worker.jsonl")
	writeFile(t, worker, validEngineRecordJSON(0, true)+"\n")
	parentFile := filepath.Join(dir, "not-a-dir")
	writeFile(t, parentFile, "x")
	if _, err := MergeAndWrite([]string{worker}, filepath.Join(parentFile, "out.jsonl"), "", meta, nil); err == nil {
		t.Fatal("MergeAndWrite() with invalid output parent error = nil")
	}
}

func validEngineRecordJSON(slot uint64, hasBlock bool) string {
	blockRoot := "null"
	if hasBlock {
		blockRoot = `"0x010203"`
	}

	source := slot + 1
	record := map[string]any{
		"slot":                      slot,
		"epoch":                     slot / 32,
		"has_block":                 hasBlock,
		"block_root":                json.RawMessage(blockRoot),
		"head_root":                 "0xhead",
		"confirmed_root":            "0xconfirmed",
		"confirmed_slot":            slot,
		"confirmation_delay_slots":  uint64(0),
		"fast_confirmed":            hasBlock,
		"strict_one_slot_confirmed": hasBlock,
		"finalized_epoch":           uint64(0),
		"justified_epoch":           uint64(0),
		"source_block_slot":         source,
		"num_attestations_injected": uint64(10),
		"is_epoch_boundary":         slot%32 == 0,
		"is_missed_slot":            !hasBlock,
		"fcr_eval_duration_us":      uint64(42),
	}

	data, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSONLRecords(t *testing.T, path string) []schema.Record {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open JSONL: %v", err)
	}
	defer file.Close()

	var records []schema.Record
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record schema.Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode JSONL: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan JSONL: %v", err)
	}
	return records
}
