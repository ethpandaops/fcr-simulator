package era

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/fcr-simulator/pkg/s3cache"
)

const (
	downloaderHTTPTimeout = 600 * time.Second
	defaultMaxRetries     = 10
	// PreDownloadLookaheadSlots is the legacy right-side slot padding used by
	// PreDownload and PreDownloadContext.
	PreDownloadLookaheadSlots uint64 = 32
	retryBackoffStepSec              = 5
	retryBackoffMaxSec               = 30
)

var downloaderRetrySleep = time.Sleep

// Downloader fetches ERA files from a remote endpoint into a cache directory.
type Downloader struct {
	baseURL  string
	cacheDir string
	client   *http.Client
	network  string
	s3Store  s3cache.Store

	mu           sync.Mutex
	indexLoaded  bool
	eraFilenames []eraFilename
}

type eraFilename struct {
	era      uint64
	filename string
}

// MirrorStats summarizes an ERA mirror run.
type MirrorStats struct {
	Scanned    int
	Skipped    int
	Downloaded int
	Uploaded   int
}

// NewDownloader returns a downloader writing to cacheDir/era/.
func NewDownloader(baseURL, cacheDir string) (*Downloader, error) {
	return NewDownloaderWithS3(baseURL, cacheDir, "", nil)
}

// NewDownloaderWithS3 returns a downloader that can use an S3-compatible cache.
func NewDownloaderWithS3(baseURL, cacheDir, network string, s3Store s3cache.Store) (*Downloader, error) {
	eraDir := filepath.Join(cacheDir, "era")
	if err := os.MkdirAll(eraDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create ERA cache directory %q: %w", eraDir, err)
	}
	network = strings.TrimSpace(network)
	if s3Store != nil && network == "" {
		return nil, fmt.Errorf("network is required when ERA S3 cache is configured")
	}

	return &Downloader{
		baseURL:  strings.TrimRight(baseURL, "/"),
		cacheDir: eraDir,
		client: &http.Client{
			Timeout: downloaderHTTPTimeout,
		},
		network: network,
		s3Store: s3Store,
	}, nil
}

// PreDownload fetches every era covering [startSlot, endSlot+32].
//
// Files already present in the cache are skipped. Each missing file is retried
// up to defaultMaxRetries times with capped linear backoff.
func (d *Downloader) PreDownload(startSlot, endSlot uint64) error {
	return d.PreDownloadContext(context.Background(), startSlot, endSlot)
}

// PreDownloadContext fetches every era covering [startSlot, endSlot+32].
func (d *Downloader) PreDownloadContext(ctx context.Context, startSlot, endSlot uint64) error {
	if d == nil {
		return fmt.Errorf("nil ERA downloader")
	}
	if startSlot > endSlot {
		return fmt.Errorf("start slot %d is after end slot %d", startSlot, endSlot)
	}
	return d.preDownloadSlotRangeContext(ctx, startSlot, saturatingAddSlot(endSlot, PreDownloadLookaheadSlots))
}

// PreDownloadSlotRangeContext fetches every era covering [startSlot, endSlot].
func (d *Downloader) PreDownloadSlotRangeContext(ctx context.Context, startSlot, endSlot uint64) error {
	return d.preDownloadSlotRangeContext(ctx, startSlot, endSlot)
}

func (d *Downloader) preDownloadSlotRangeContext(ctx context.Context, startSlot, endSlot uint64) error {
	if d == nil {
		return fmt.Errorf("nil ERA downloader")
	}
	if startSlot > endSlot {
		return fmt.Errorf("start slot %d is after end slot %d", startSlot, endSlot)
	}

	startEra := EraNumberForSlot(startSlot)
	endEra := EraNumberForSlot(endSlot)

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
		if err := d.downloadEraWithRetries(ctx, eraNumber, defaultMaxRetries); err != nil {
			return err
		}
	}

	return nil
}

func saturatingAddSlot(value, delta uint64) uint64 {
	if delta > math.MaxUint64-value {
		return math.MaxUint64
	}
	return value + delta
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

func (d *Downloader) downloadEraWithRetries(ctx context.Context, eraNumber uint64, maxRetries uint32) error {
	if maxRetries == 0 {
		return fmt.Errorf("max retries must be greater than zero")
	}

	if ok, err := d.tryDownloadEraFromS3(ctx, eraNumber); err != nil {
		return err
	} else if ok {
		return nil
	}

	return d.downloadEraFromHTTPWithRetries(ctx, eraNumber, maxRetries, d.s3Store != nil)
}

func (d *Downloader) downloadEraFromHTTPWithRetries(ctx context.Context, eraNumber uint64, maxRetries uint32, uploadToS3 bool) error {
	filename, err := d.filenameForEra(eraNumber)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s", d.baseURL, filename)

	var lastErr error
	for attempt := uint32(1); attempt <= maxRetries; attempt++ {
		if err := d.tryDownloadHTTP(ctx, url, filename, uploadToS3); err != nil {
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

func (d *Downloader) tryDownloadHTTP(ctx context.Context, url, filename string, uploadToS3 bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to build ERA file request %q: %w", url, err)
	}

	resp, err := d.client.Do(req)
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
	if uploadToS3 {
		key := eraObjectKey(d.network, filename)
		if err := d.s3Store.UploadFile(ctx, key, cachePath); err != nil {
			return err
		}
	}
	return nil
}

func (d *Downloader) tryDownloadEraFromS3(ctx context.Context, eraNumber uint64) (bool, error) {
	if d.s3Store == nil {
		return false, nil
	}

	filename, ok, err := d.s3FilenameForEra(ctx, eraNumber)
	if err != nil || !ok {
		return false, err
	}

	key := eraObjectKey(d.network, filename)
	cachePath := filepath.Join(d.cacheDir, filename)
	ok, err = d.s3Store.DownloadFile(ctx, key, cachePath)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (d *Downloader) s3FilenameForEra(ctx context.Context, eraNumber uint64) (string, bool, error) {
	objects, err := d.s3Store.ListObjects(ctx, eraObjectPrefix(d.network, eraNumber))
	if err != nil {
		return "", false, err
	}

	for _, object := range objects {
		filename := path.Base(object.Key)
		if strings.HasPrefix(filename, eraFilePrefix(eraNumber)) && strings.HasSuffix(filename, ".era") {
			return filename, true, nil
		}
	}
	return "", false, nil
}

// MirrorToS3 mirrors every era in [startEra, endEra] from the public archive to S3.
func (d *Downloader) MirrorToS3(ctx context.Context, startEra, endEra uint64, parallel int) (MirrorStats, error) {
	var stats MirrorStats
	if d == nil {
		return stats, fmt.Errorf("nil ERA downloader")
	}
	if d.s3Store == nil {
		return stats, fmt.Errorf("ERA S3 cache is not configured")
	}
	if startEra > endEra {
		return stats, fmt.Errorf("start era %d is after end era %d", startEra, endEra)
	}
	if parallel <= 0 {
		return stats, fmt.Errorf("parallel must be greater than zero")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan uint64)
	var (
		statsMu  sync.Mutex
		errOnce  sync.Once
		firstErr error
		wg       sync.WaitGroup
	)

	increment := func(update func(*MirrorStats)) {
		statsMu.Lock()
		defer statsMu.Unlock()
		update(&stats)
	}
	setErr := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case eraNumber, ok := <-jobs:
				if !ok {
					return
				}
				if err := d.mirrorEraToS3(ctx, eraNumber, increment); err != nil {
					setErr(err)
					return
				}
			}
		}
	}

	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go worker()
	}

	for eraNumber := startEra; ; eraNumber++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			statsMu.Lock()
			defer statsMu.Unlock()
			if firstErr != nil {
				return stats, firstErr
			}
			return stats, ctx.Err()
		case jobs <- eraNumber:
		}
		if eraNumber == endEra {
			break
		}
	}
	close(jobs)
	wg.Wait()

	statsMu.Lock()
	defer statsMu.Unlock()
	return stats, firstErr
}

func (d *Downloader) mirrorEraToS3(ctx context.Context, eraNumber uint64, increment func(func(*MirrorStats))) error {
	increment(func(stats *MirrorStats) {
		stats.Scanned++
	})

	filename, err := d.filenameForEra(eraNumber)
	if err != nil {
		return err
	}
	key := eraObjectKey(d.network, filename)
	exists, err := d.s3Store.ObjectExists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		increment(func(stats *MirrorStats) {
			stats.Skipped++
		})
		return nil
	}

	cachePath := filepath.Join(d.cacheDir, filename)
	if _, err := os.Stat(cachePath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat ERA cache file %q: %w", cachePath, err)
		}
		url := fmt.Sprintf("%s/%s", d.baseURL, filename)
		if err := d.tryDownloadHTTP(ctx, url, filename, false); err != nil {
			return err
		}
		increment(func(stats *MirrorStats) {
			stats.Downloaded++
		})
	}
	if err := d.s3Store.UploadFile(ctx, key, cachePath); err != nil {
		return err
	}
	increment(func(stats *MirrorStats) {
		stats.Uploaded++
	})
	if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove mirrored ERA cache file %q: %w", cachePath, err)
	}
	return nil
}

func eraObjectPrefix(network string, eraNumber uint64) string {
	return fmt.Sprintf("era/%s/%s", network, eraFilePrefix(eraNumber))
}

func eraObjectKey(network, filename string) string {
	return fmt.Sprintf("era/%s/%s", network, filename)
}
