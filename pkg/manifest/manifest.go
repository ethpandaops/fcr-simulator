package manifest

import (
	"crypto/sha256"
	"encoding/hex"
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

type RunManifest struct {
	SchemaVersion       int            `json:"schema_version"`
	FCRSimulatorVersion string         `json:"fcr_simulator_version"`
	RanAt               string         `json:"ran_at"`
	Config              Config         `json:"config"`
	EngineManifest      EngineManifest `json:"engine_manifest"`
	Inputs              Inputs         `json:"inputs"`
	Outputs             Outputs        `json:"outputs"`
}

type Config struct {
	Engine                string `json:"engine"`
	Network               string `json:"network"`
	StartEpoch            uint64 `json:"start_epoch"`
	EndEpoch              uint64 `json:"end_epoch"`
	WarmupEpochs          uint64 `json:"warmup_epochs"`
	Parallel              int    `json:"parallel"`
	AttestationSourceMode string `json:"attestation_source_mode"`
	LookaheadCap          uint64 `json:"lookahead_cap"`
	ByzantineThreshold    uint64 `json:"byzantine_threshold"`
	BeaconNodeURL         string `json:"beacon_node_url"`
	EraURL                string `json:"era_url"`
}

type EngineManifest struct {
	EngineName    string   `json:"engine_name"`
	EngineVersion string   `json:"engine_version"`
	EngineCommit  string   `json:"engine_commit"`
	BuildFlags    []string `json:"build_flags"`
	FCRSpecCommit string   `json:"fcr_spec_commit"`
}

type Inputs struct {
	EraFiles         []EraFile         `json:"era_files"`
	CheckpointStates []CheckpointState `json:"checkpoint_states"`
}

type EraFile struct {
	Era    uint64 `json:"era"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type CheckpointState struct {
	Worker int    `json:"worker"`
	Slot   uint64 `json:"slot"`
	SHA256 string `json:"sha256"`
}

type Outputs struct {
	ResultsJSONLSHA256 string `json:"results_jsonl_sha256"`
	ResultsCSVSHA256   string `json:"results_csv_sha256"`
	TotalSlots         uint64 `json:"total_slots"`
	FastConfirmedCount uint64 `json:"fast_confirmed_count"`
}

// Write serializes m as pretty-printed JSON to path.
func Write(path string, m RunManifest) error {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = schema.SchemaVersion
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest directory for %q: %w", path, err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-manifest-*")
	if err != nil {
		return fmt.Errorf("create temp manifest for %q: %w", path, err)
	}
	tempPath := tempFile.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	encoder := json.NewEncoder(tempFile)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(m); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("encode manifest %q: %w", path, err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync manifest temp file %q: %w", tempPath, err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close manifest temp file %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace manifest %q: %w", path, err)
	}

	keepTemp = true
	return nil
}

// SHA256Bytes returns the lowercase hex SHA-256 digest of data.
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256File returns the lowercase hex SHA-256 digest of path.
func SHA256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q for hashing: %w", path, err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// CollectEraFiles scans eraCacheDir for mainnet ERA files and returns their
// hashes. baseURL is used to fill the manifest URL field.
func CollectEraFiles(eraCacheDir, baseURL string) ([]EraFile, error) {
	entries, err := os.ReadDir(eraCacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read ERA cache directory %q: %w", eraCacheDir, err)
	}

	baseURL = strings.TrimRight(baseURL, "/")
	files := make([]EraFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".era") {
			continue
		}

		eraNumber, ok := parseEraNumber(entry.Name())
		if !ok {
			continue
		}

		path := filepath.Join(eraCacheDir, entry.Name())
		sha, err := SHA256File(path)
		if err != nil {
			return nil, err
		}

		url := entry.Name()
		if baseURL != "" {
			url = baseURL + "/" + entry.Name()
		}

		files = append(files, EraFile{
			Era:    eraNumber,
			URL:    url,
			SHA256: sha,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Era < files[j].Era
	})
	return files, nil
}

func parseEraNumber(filename string) (uint64, bool) {
	const prefix = "mainnet-"
	if !strings.HasPrefix(filename, prefix) {
		return 0, false
	}

	rest := strings.TrimPrefix(filename, prefix)
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}

	era, err := strconv.ParseUint(rest[:end], 10, 64)
	return era, err == nil
}
