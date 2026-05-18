package beaconfetch

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	httpTimeout              = 600 * time.Second
	slotsPerEpoch            = uint64(32)
	checkpointFallbackEpochs = uint64(4)
)

// ErrNotFound is returned when the beacon node returns 404 (slot missed,
// state pruned, etc.).
var ErrNotFound = errors.New("not found")

// Fetcher pulls SSZ blobs from a real beacon node and caches them on disk.
type Fetcher struct {
	beaconNodeURL string
	cacheDir      string
	client        *http.Client
}

// New returns a Fetcher that reads and writes SSZ cache files under cacheDir.
func New(beaconNodeURL, cacheDir string) (*Fetcher, error) {
	beaconNodeURL = strings.TrimRight(strings.TrimSpace(beaconNodeURL), "/")
	if beaconNodeURL == "" {
		return nil, fmt.Errorf("beacon node URL is required")
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}

	if err := os.MkdirAll(filepath.Join(cacheDir, "states"), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create states cache directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "blocks"), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create blocks cache directory: %w", err)
	}

	return &Fetcher{
		beaconNodeURL: beaconNodeURL,
		cacheDir:      cacheDir,
		client: &http.Client{
			Timeout: httpTimeout,
		},
	}, nil
}

// FetchStateSSZAtSlot fetches /eth/v2/debug/beacon/states/{slot}.
// Returns the raw SSZ bytes. Caches to cacheDir/states/state-{slot}.ssz.
// Returns (nil, ErrNotFound) on 404.
func (f *Fetcher) FetchStateSSZAtSlot(slot uint64) ([]byte, error) {
	cachePath := filepath.Join(f.cacheDir, "states", fmt.Sprintf("state-%d.ssz", slot))
	url := fmt.Sprintf("%s/eth/v2/debug/beacon/states/%d", f.beaconNodeURL, slot)

	return f.cachedOrFetch(cachePath, url, acceptSSZ)
}

// FetchGenesisStateSSZ fetches /eth/v2/debug/beacon/states/genesis.
// Caches to cacheDir/states/state-genesis.ssz.
func (f *Fetcher) FetchGenesisStateSSZ() ([]byte, error) {
	cachePath := filepath.Join(f.cacheDir, "states", "state-genesis.ssz")
	url := fmt.Sprintf("%s/eth/v2/debug/beacon/states/genesis", f.beaconNodeURL)

	return f.cachedOrFetch(cachePath, url, acceptSSZ)
}

// FetchBlockSSZByRoot fetches /eth/v2/beacon/blocks/{0x...}.
// Caches to cacheDir/blocks/block-{root_hex}.ssz.
func (f *Fetcher) FetchBlockSSZByRoot(root [32]byte) ([]byte, error) {
	rootText := rootHex(root)
	cachePath := filepath.Join(f.cacheDir, "blocks", fmt.Sprintf("block-%s.ssz", rootText))
	url := fmt.Sprintf("%s/eth/v2/beacon/blocks/%s", f.beaconNodeURL, rootText)

	return f.cachedOrFetch(cachePath, url, acceptSSZ)
}

// FetchCheckpointAtWarmupSlot fetches the checkpoint state at the warmup slot.
//
// Lighthouse's weak_subjectivity_state bootstrap requires the state to be on an
// epoch boundary and will per-slot advance it to the next boundary if not.
// If the boundary slot was missed on mainnet, Lighthouse advances the state's
// slot past the latest block, sets split_slot above the block's slot, and then
// fails when reading head state (block.slot < split_slot puts the block in
// cold-DB territory).
//
// To stay safely epoch-aligned, this fallback shifts back by a full epoch (32
// slots) per attempt, not by single slots. The caller is expected to pass a
// warmup slot that is already epoch-aligned.
func (f *Fetcher) FetchCheckpointAtWarmupSlot(warmupSlot uint64) (actualSlot uint64, stateSSZ []byte, err error) {
	for epochOffset := uint64(0); epochOffset <= checkpointFallbackEpochs; epochOffset++ {
		step := epochOffset * slotsPerEpoch
		if step > warmupSlot {
			break
		}

		slot := warmupSlot - step
		_, err := f.CheckpointBlockRootAtSlot(slot)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, nil, fmt.Errorf("failed to fetch checkpoint block root at slot %d: %w", slot, err)
		}

		stateSSZ, err := f.FetchStateSSZAtSlot(slot)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to fetch checkpoint state at slot %d: %w", slot, err)
		}

		return slot, stateSSZ, nil
	}

	return 0, nil, fmt.Errorf("no non-missed epoch-aligned checkpoint slot found at or before warmup slot %d within %d epochs: %w", warmupSlot, checkpointFallbackEpochs, ErrNotFound)
}

// CheckpointBlockRootAtSlot returns data.root from /eth/v1/beacon/headers/{slot}.
func (f *Fetcher) CheckpointBlockRootAtSlot(slot uint64) ([32]byte, error) {
	url := fmt.Sprintf("%s/eth/v1/beacon/headers/%d", f.beaconNodeURL, slot)

	body, err := f.fetch(url, acceptJSON)
	if err != nil {
		return [32]byte{}, err
	}

	var response struct {
		Data struct {
			Root string `json:"root"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return [32]byte{}, fmt.Errorf("failed to decode beacon header response: %w", err)
	}

	root, err := parseRoot(response.Data.Root)
	if err != nil {
		return [32]byte{}, fmt.Errorf("failed to parse beacon header root: %w", err)
	}

	return root, nil
}

const (
	acceptSSZ  = "application/octet-stream"
	acceptJSON = "application/json"
)

func (f *Fetcher) cachedOrFetch(cachePath, url, accept string) ([]byte, error) {
	data, err := os.ReadFile(cachePath)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to read cache file %q: %w", cachePath, err)
	}

	data, err = f.fetch(url, accept)
	if err != nil {
		return nil, err
	}

	if err := writeFileAtomic(cachePath, data); err != nil {
		return nil, fmt.Errorf("failed to write cache file %q: %w", cachePath, err)
	}

	return data, nil
}

func (f *Fetcher) fetch(url, accept string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request for %s: %w", url, err)
	}
	req.Header.Set("Accept", accept)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s from %s", resp.Status, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from %s: %w", url, err)
	}

	return body, nil
}

func writeFileAtomic(path string, data []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	tempFile, err := os.CreateTemp(parent, ".tmp-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}

	keepTemp = true
	return nil
}

func rootHex(root [32]byte) string {
	return "0x" + hex.EncodeToString(root[:])
}

func parseRoot(value string) ([32]byte, error) {
	if value == "" {
		return [32]byte{}, fmt.Errorf("empty root")
	}

	value = strings.TrimPrefix(value, "0x")
	value = strings.TrimPrefix(value, "0X")
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return [32]byte{}, err
	}
	if len(decoded) != 32 {
		return [32]byte{}, fmt.Errorf("expected 32 bytes, got %d", len(decoded))
	}

	var root [32]byte
	copy(root[:], decoded)
	return root, nil
}
