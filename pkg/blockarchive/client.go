package blockarchive

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	httpTimeout               = 600 * time.Second
	fetchMaxAttempts          = 3
	fetchRetryBaseLag         = 250 * time.Millisecond
	indexSlotRangePageSize    = 1000
	indexSafetyEntriesPerSlot = 16
)

var ErrNotFound = errors.New("block archive: not found")

type Client struct {
	baseURL  string
	network  string
	cacheDir string
	client   *http.Client
}

type indexResponse struct {
	Index []IndexEntry `json:"index"`
}

type IndexEntry struct {
	Slot      uint64 `json:"slot"`
	BlockRoot string `json:"block_root"`
}

type FetchStats struct {
	CacheHit                 bool
	Downloaded               bool
	TransientFailuresRetried int
}

func New(baseURL, network, cacheDir string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	network = strings.TrimSpace(network)
	cacheDir = strings.TrimSpace(cacheDir)
	if baseURL == "" {
		return nil, fmt.Errorf("block archive base URL is required")
	}
	if network == "" {
		return nil, fmt.Errorf("block archive network is required")
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("block archive cache dir is required")
	}
	return &Client{
		baseURL:  baseURL,
		network:  network,
		cacheDir: cacheDir,
		client: &http.Client{
			Timeout: httpTimeout,
		},
	}, nil
}

func (c *Client) FetchBlockSSZByRoot(root [32]byte) ([]byte, error) {
	bytes, _, err := c.FetchBlockSSZByRootWithStats(root)
	return bytes, err
}

func (c *Client) FetchBlockSSZByRootWithStats(root [32]byte) ([]byte, FetchStats, error) {
	var stats FetchStats
	if c == nil {
		return nil, stats, ErrNotFound
	}
	if bytes, ok, err := c.readCached(root); err != nil {
		return nil, stats, err
	} else if ok {
		stats.CacheHit = true
		return bytes, stats, nil
	}

	rootText := rootHex(root)
	slot, err := c.lookupSlot(root, rootText, &stats)
	if err != nil {
		return nil, stats, err
	}

	bytes, err := c.downloadBlock(slot, rootText, &stats)
	if err != nil {
		return nil, stats, err
	}
	if err := c.writeCached(root, bytes); err != nil {
		return nil, stats, err
	}
	stats.Downloaded = true
	return cloneBytes(bytes), stats, nil
}

func (c *Client) FetchBlockSSZBySlotAndRootWithStats(slot uint64, root [32]byte) ([]byte, FetchStats, error) {
	var stats FetchStats
	if c == nil {
		return nil, stats, ErrNotFound
	}
	if bytes, ok, err := c.readCached(root); err != nil {
		return nil, stats, err
	} else if ok {
		stats.CacheHit = true
		return bytes, stats, nil
	}

	bytes, err := c.downloadBlock(slot, rootHex(root), &stats)
	if err != nil {
		return nil, stats, err
	}
	if err := c.writeCached(root, bytes); err != nil {
		return nil, stats, err
	}
	stats.Downloaded = true
	return cloneBytes(bytes), stats, nil
}

// ReadCachedSSZByRoot returns a previously-cached block without contacting the
// archive. Returns ErrNotFound when the block is absent from the local disk
// cache. The serving hot loop uses this so a slow or unavailable archive can
// never stall a simulation; the prep pass is responsible for warming the cache.
func (c *Client) ReadCachedSSZByRoot(root [32]byte) ([]byte, error) {
	if c == nil {
		return nil, ErrNotFound
	}
	bytes, ok, err := c.readCached(root)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return cloneBytes(bytes), nil
}

func (c *Client) ListIndexBySlotRangeWithStats(slotMin, slotMax uint64, limit int) ([]IndexEntry, FetchStats, error) {
	var stats FetchStats
	if c == nil {
		return nil, stats, ErrNotFound
	}
	if slotMin > slotMax {
		return nil, stats, nil
	}
	if limit <= 0 {
		return nil, stats, fmt.Errorf("slot range index limit must be greater than zero")
	}

	pageLimit := limit
	if pageLimit > indexSlotRangePageSize {
		pageLimit = indexSlotRangePageSize
	}
	maxEntries := slotRangeIndexSafetyLimit(slotMin, slotMax, pageLimit)
	entries := make([]IndexEntry, 0)
	offset := 0
	for {
		page, err := c.listIndexBySlotRangePage(slotMin, slotMax, pageLimit, offset, &stats)
		if errors.Is(err, ErrNotFound) {
			return entries, stats, nil
		}
		if err != nil {
			return nil, stats, err
		}
		if len(page) == 0 {
			return entries, stats, nil
		}
		if len(entries) > maxEntries-len(page) {
			return nil, stats, fmt.Errorf("block archive slot range index exceeded safety limit of %d entries for slots [%d,%d]", maxEntries, slotMin, slotMax)
		}
		entries = append(entries, page...)
		offset += len(page)
	}
}

func (c *Client) listIndexBySlotRangePage(slotMin, slotMax uint64, limit, offset int, stats *FetchStats) ([]IndexEntry, error) {
	for attempt := 1; ; attempt++ {
		entries, err := c.listIndexBySlotRangeOnce(slotMin, slotMax, limit, offset)
		if err == nil {
			return entries, nil
		}
		if !isTransientArchiveError(err) || attempt >= fetchMaxAttempts {
			return nil, err
		}
		if stats != nil {
			stats.TransientFailuresRetried++
		}
		sleepBeforeRetry(attempt)
	}
}

func (c *Client) listIndexBySlotRangeOnce(slotMin, slotMax uint64, limit, offset int) ([]IndexEntry, error) {
	indexURL, err := url.Parse(c.baseURL + "/api/v1/index")
	if err != nil {
		return nil, err
	}
	query := indexURL.Query()
	query.Set("network", c.network)
	query.Set("slot_min", strconv.FormatUint(slotMin, 10))
	query.Set("slot_max", strconv.FormatUint(slotMax, 10))
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	indexURL.RawQuery = query.Encode()

	resp, err := c.client.Get(indexURL.String())
	if err != nil {
		return nil, transientArchiveError{err: fmt.Errorf("fetch block archive slot range index: %w", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, transientArchiveError{err: fmt.Errorf("read block archive slot range index body: %w", err)}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if retryableHTTPStatus(resp.StatusCode) {
		return nil, transientArchiveError{err: fmt.Errorf("block archive slot range index HTTP %d: %s", resp.StatusCode, trimBody(string(body)))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("block archive slot range index HTTP %d: %s", resp.StatusCode, trimBody(string(body)))
	}

	var parsed indexResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode block archive slot range index response: %w", err)
	}
	return parsed.Index, nil
}

func (c *Client) lookupSlot(root [32]byte, rootText string, stats *FetchStats) (uint64, error) {
	for attempt := 1; ; attempt++ {
		slot, err := c.lookupSlotOnce(root, rootText)
		if err == nil {
			return slot, nil
		}
		if !isTransientArchiveError(err) || attempt >= fetchMaxAttempts {
			return 0, err
		}
		if stats != nil {
			stats.TransientFailuresRetried++
		}
		sleepBeforeRetry(attempt)
	}
}

func (c *Client) lookupSlotOnce(root [32]byte, rootText string) (uint64, error) {
	indexURL, err := url.Parse(c.baseURL + "/api/v1/index")
	if err != nil {
		return 0, err
	}
	query := indexURL.Query()
	query.Set("network", c.network)
	query.Set("block_root_prefix", rootText)
	query.Set("limit", "1")
	indexURL.RawQuery = query.Encode()

	resp, err := c.client.Get(indexURL.String())
	if err != nil {
		return 0, transientArchiveError{err: fmt.Errorf("fetch block archive index: %w", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, transientArchiveError{err: fmt.Errorf("read block archive index body: %w", err)}
	}
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
	}
	if retryableHTTPStatus(resp.StatusCode) {
		return 0, transientArchiveError{err: fmt.Errorf("block archive index HTTP %d: %s", resp.StatusCode, trimBody(string(body)))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("block archive index HTTP %d: %s", resp.StatusCode, trimBody(string(body)))
	}

	var parsed indexResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("decode block archive index response: %w", err)
	}
	for _, entry := range parsed.Index {
		if rootsEqual(entry.BlockRoot, root) {
			return entry.Slot, nil
		}
	}
	return 0, ErrNotFound
}

func (c *Client) downloadBlock(slot uint64, rootText string, stats *FetchStats) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		bytes, err := c.downloadBlockOnce(slot, rootText)
		if err == nil {
			return bytes, nil
		}
		if !isTransientArchiveError(err) || attempt >= fetchMaxAttempts {
			return nil, err
		}
		if stats != nil {
			stats.TransientFailuresRetried++
		}
		sleepBeforeRetry(attempt)
	}
}

func (c *Client) downloadBlockOnce(slot uint64, rootText string) ([]byte, error) {
	blockURL := fmt.Sprintf(
		"%s/%s/%d/%s.ssz",
		c.baseURL,
		url.PathEscape(c.network),
		slot,
		rootText,
	)
	resp, err := c.client.Get(blockURL)
	if err != nil {
		return nil, transientArchiveError{err: fmt.Errorf("download block archive block: %w", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, transientArchiveError{err: fmt.Errorf("read block archive block body: %w", err)}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if retryableHTTPStatus(resp.StatusCode) {
		return nil, transientArchiveError{err: fmt.Errorf("block archive download HTTP %d: %s", resp.StatusCode, trimBody(string(body)))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("block archive download HTTP %d: %s", resp.StatusCode, trimBody(string(body)))
	}
	return body, nil
}

func (c *Client) cachePath(root [32]byte) string {
	return filepath.Join(c.cacheDir, rootHex(root)+".ssz")
}

func (c *Client) readCached(root [32]byte) ([]byte, bool, error) {
	bytes, err := os.ReadFile(c.cachePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read block archive cache: %w", err)
	}
	return bytes, true, nil
}

func (c *Client) writeCached(root [32]byte, bytes []byte) error {
	if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create block archive cache dir: %w", err)
	}
	// Write to a temp file then rename so concurrent readers (parallel engine
	// workers hitting the orchestrator) never observe a partially-written file.
	tmp, err := os.CreateTemp(c.cacheDir, ".tmp-*.ssz")
	if err != nil {
		return fmt.Errorf("create block archive cache temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(bytes); err != nil {
		tmp.Close()
		return fmt.Errorf("write block archive cache temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close block archive cache temp: %w", err)
	}
	if err := os.Rename(tmpName, c.cachePath(root)); err != nil {
		return fmt.Errorf("rename block archive cache: %w", err)
	}
	return nil
}

func rootHex(root [32]byte) string {
	return "0x" + hex.EncodeToString(root[:])
}

func ParseRoot(text string) ([32]byte, error) {
	var root [32]byte
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "0x"), "0X")
	if len(text) != 64 {
		return root, fmt.Errorf("invalid block root length")
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return root, fmt.Errorf("invalid block root hex: %w", err)
	}
	copy(root[:], decoded)
	return root, nil
}

func rootsEqual(text string, root [32]byte) bool {
	parsed, err := ParseRoot(text)
	return err == nil && parsed == root
}

func cloneBytes(bytes []byte) []byte {
	if bytes == nil {
		return nil
	}
	out := make([]byte, len(bytes))
	copy(out, bytes)
	return out
}

func trimBody(body string) string {
	const maxLen = 512
	if len(body) <= maxLen {
		return body
	}
	return body[:maxLen]
}

func slotRangeIndexSafetyLimit(slotMin, slotMax uint64, pageLimit int) int {
	maxInt := int(^uint(0) >> 1)
	span := slotMax - slotMin
	maxSafeSpan := uint64(maxInt/indexSafetyEntriesPerSlot) - 1
	if span > maxSafeSpan {
		return maxInt
	}
	maxEntries := int(span+1) * indexSafetyEntriesPerSlot
	if maxEntries < pageLimit {
		return pageLimit
	}
	return maxEntries
}

type transientArchiveError struct {
	err error
}

func (e transientArchiveError) Error() string {
	return e.err.Error()
}

func (e transientArchiveError) Unwrap() error {
	return e.err
}

func isTransientArchiveError(err error) bool {
	var transient transientArchiveError
	return errors.As(err, &transient)
}

func retryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func sleepBeforeRetry(attempt int) {
	delay := fetchRetryBaseLag
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	time.Sleep(delay)
}
