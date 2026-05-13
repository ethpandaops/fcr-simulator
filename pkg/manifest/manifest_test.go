package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethpandaops/fcr-simulator/pkg/schema"
)

func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.manifest.json")

	want := RunManifest{
		SchemaVersion:       schema.SchemaVersion,
		FCRSimulatorVersion: "deadbeef",
		RanAt:               "2026-05-13T00:00:00Z",
		Config: Config{
			Engine:                "lighthouse",
			Network:               "mainnet",
			StartEpoch:            1,
			EndEpoch:              2,
			WarmupEpochs:          10,
			Parallel:              4,
			AttestationSourceMode: "next-non-missed",
			LookaheadCap:          4,
			ByzantineThreshold:    25,
			BeaconNodeURL:         "http://beacon",
			EraURL:                "https://era",
		},
		EngineManifest: EngineManifest{
			EngineName:    "lighthouse",
			EngineVersion: "5.4.0",
			EngineCommit:  "abc123",
			BuildFlags:    []string{"fake_crypto"},
			FCRSpecCommit: "spec123",
		},
		Inputs: Inputs{
			EraFiles: []EraFile{
				{Era: 1, URL: "https://era/mainnet-00001.era", SHA256: "era-sha"},
			},
			CheckpointStates: []CheckpointState{
				{Worker: 0, Slot: 32, SHA256: "state-sha"},
			},
		},
		Outputs: Outputs{
			ResultsJSONLSHA256: "jsonl-sha",
			ResultsCSVSHA256:   "csv-sha",
			TotalSlots:         32,
			FastConfirmedCount: 30,
		},
	}

	if err := Write(path, want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got RunManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestWriteDefaultsSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := Write(path, RunManifest{}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var got RunManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.SchemaVersion != schema.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, schema.SchemaVersion)
	}
}

func TestHashHelpersAndCollectEraFiles(t *testing.T) {
	dir := t.TempDir()
	era0 := filepath.Join(dir, "mainnet-00000-a.era")
	era2 := filepath.Join(dir, "mainnet-00002-b.era")
	if err := os.WriteFile(era2, []byte("two"), 0o644); err != nil {
		t.Fatalf("write era2: %v", err)
	}
	if err := os.WriteFile(era0, []byte("zero"), 0o644); err != nil {
		t.Fatalf("write era0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write ignore: %v", err)
	}

	files, err := CollectEraFiles(dir, "https://era.example")
	if err != nil {
		t.Fatalf("CollectEraFiles() error = %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Era != 0 || files[1].Era != 2 {
		t.Fatalf("files not sorted by era: %#v", files)
	}
	if files[0].URL != "https://era.example/mainnet-00000-a.era" {
		t.Fatalf("URL = %q", files[0].URL)
	}
	if files[0].SHA256 != SHA256Bytes([]byte("zero")) || files[1].SHA256 != SHA256Bytes([]byte("two")) {
		t.Fatalf("unexpected hashes: %#v", files)
	}
}

func TestCollectEraFilesMissingDirectory(t *testing.T) {
	files, err := CollectEraFiles(filepath.Join(t.TempDir(), "missing"), "https://era.example")
	if err != nil {
		t.Fatalf("CollectEraFiles() error = %v", err)
	}
	if files != nil {
		t.Fatalf("files = %#v, want nil", files)
	}
}

func TestErrorBranches(t *testing.T) {
	dir := t.TempDir()
	parentFile := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write parent file: %v", err)
	}

	if err := Write(filepath.Join(parentFile, "manifest.json"), RunManifest{}); err == nil {
		t.Fatal("Write() error = nil, want invalid parent error")
	}
	if _, err := SHA256File(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("SHA256File() error = nil, want missing file error")
	}
	if _, err := CollectEraFiles(parentFile, ""); err == nil {
		t.Fatal("CollectEraFiles() error = nil, want readdir error")
	}
}

func TestParseEraNumber(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		wantEra  uint64
		wantOK   bool
	}{
		{name: "valid", filename: "mainnet-00123-deadbeef.era", wantEra: 123, wantOK: true},
		{name: "missing prefix", filename: "gnosis-00123.era", wantOK: false},
		{name: "missing digits", filename: "mainnet-.era", wantOK: false},
		{name: "overflow", filename: "mainnet-184467440737095516160.era", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotEra, gotOK := parseEraNumber(tc.filename)
			if gotOK != tc.wantOK {
				t.Fatalf("parseEraNumber(%q) ok = %v, want %v", tc.filename, gotOK, tc.wantOK)
			}
			if gotOK && gotEra != tc.wantEra {
				t.Fatalf("parseEraNumber(%q) era = %d, want %d", tc.filename, gotEra, tc.wantEra)
			}
		})
	}
}
