package era

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	downloaderHTTPTimeout = 600 * time.Second
	defaultMaxRetries     = 10
	retryBackoffStepSec   = 5
	retryBackoffMaxSec    = 30
)

var downloaderRetrySleep = time.Sleep

// Downloader fetches ERA files from a remote endpoint into a cache directory.
type Downloader struct {
	baseURL  string
	cacheDir string
	client   *http.Client

	mu           sync.Mutex
	indexLoaded  bool
	eraFilenames []eraFilename
}

type eraFilename struct {
	era      uint64
	filename string
}

// NewDownloader returns a downloader writing to cacheDir/era/.
func NewDownloader(baseURL, cacheDir string) (*Downloader, error) {
	eraDir := filepath.Join(cacheDir, "era")
	if err := os.MkdirAll(eraDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create ERA cache directory %q: %w", eraDir, err)
	}

	return &Downloader{
		baseURL:  strings.TrimRight(baseURL, "/"),
		cacheDir: eraDir,
		client: &http.Client{
			Timeout: downloaderHTTPTimeout,
		},
	}, nil
}

// PreDownload fetches every era covering [startSlot, endSlot+32].
//
// Files already present in the cache are skipped. Each missing file is retried
// up to defaultMaxRetries times with capped linear backoff.
func (d *Downloader) PreDownload(startSlot, endSlot uint64) error {
	if d == nil {
		return fmt.Errorf("nil ERA downloader")
	}
	if startSlot > endSlot {
		return fmt.Errorf("start slot %d is after end slot %d", startSlot, endSlot)
	}

	startEra := EraNumberForSlot(startSlot)
	endWithLookahead := endSlot + 32
	if endSlot > math.MaxUint64-32 {
		endWithLookahead = math.MaxUint64
	}
	endEra := EraNumberForSlot(endWithLookahead)

	var needed []uint64
	for eraNumber := startEra; ; eraNumber++ {
		_, ok, err := findCachedFile(d.cacheDir, eraFilePrefix(eraNumber))
		if err != nil {
			return err
		}
		if !ok {
			needed = append(needed, eraNumber)
		}
		if eraNumber == endEra {
			break
		}
	}

	for _, eraNumber := range needed {
		if err := d.downloadEraWithRetries(eraNumber, defaultMaxRetries); err != nil {
			return err
		}
	}

	return nil
}

// CacheDir returns the actual directory ERA files are written to.
func (d *Downloader) CacheDir() string {
	if d == nil {
		return ""
	}
	return d.cacheDir
}

func (d *Downloader) fetchIndex() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.indexLoaded {
		return nil
	}

	resp, err := d.client.Get(d.baseURL)
	if err != nil {
		return fmt.Errorf("failed to fetch ERA index: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read ERA index body: %w", err)
	}

	filenames := parseEraFilenames(string(body))
	d.eraFilenames = filenames
	d.indexLoaded = true
	return nil
}

func parseEraFilenames(body string) []eraFilename {
	var filenames []eraFilename
	for _, line := range strings.Split(body, "\n") {
		start := strings.Index(line, "mainnet-")
		if start < 0 {
			continue
		}
		rest := line[start:]
		end := strings.Index(rest, ".era")
		if end < 0 {
			continue
		}

		filename := rest[:end+len(".era")]
		numStr := strings.TrimPrefix(filename, "mainnet-")
		if dash := strings.Index(numStr, "-"); dash >= 0 {
			numStr = numStr[:dash]
		}

		eraNumber, err := strconv.ParseUint(numStr, 10, 64)
		if err != nil {
			continue
		}
		filenames = append(filenames, eraFilename{era: eraNumber, filename: filename})
	}

	sort.Slice(filenames, func(i, j int) bool {
		return filenames[i].era < filenames[j].era
	})
	return filenames
}

func (d *Downloader) filenameForEra(eraNumber uint64) (string, error) {
	if err := d.fetchIndex(); err != nil {
		return "", err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, candidate := range d.eraFilenames {
		if candidate.era == eraNumber {
			return candidate.filename, nil
		}
	}
	return "", fmt.Errorf("ERA file not found for era number %d", eraNumber)
}

func (d *Downloader) downloadEraWithRetries(eraNumber uint64, maxRetries uint32) error {
	if maxRetries == 0 {
		return fmt.Errorf("max retries must be greater than zero")
	}

	filename, err := d.filenameForEra(eraNumber)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s", d.baseURL, filename)

	var lastErr error
	for attempt := uint32(1); attempt <= maxRetries; attempt++ {
		if err := d.tryDownload(url, filename); err != nil {
			lastErr = err
			if attempt < maxRetries {
				backoff := min(retryBackoffStepSec*int(attempt), retryBackoffMaxSec)
				downloaderRetrySleep(time.Duration(backoff) * time.Second)
			}
			continue
		}
		return nil
	}

	return fmt.Errorf("failed to download ERA %d after %d attempts: %w", eraNumber, maxRetries, lastErr)
}

func (d *Downloader) tryDownload(url, filename string) error {
	resp, err := d.client.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download ERA file %q: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s from %s", resp.Status, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read ERA file response body: %w", err)
	}

	cachePath := filepath.Join(d.cacheDir, filename)
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		return fmt.Errorf("failed to create cache file %q: %w", cachePath, err)
	}
	return nil
}
