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
	"strings"
	"time"
)

const httpTimeout = 600 * time.Second

var ErrNotFound = errors.New("block archive: not found")

type Client struct {
	baseURL  string
	network  string
	cacheDir string
	client   *http.Client
}

type indexResponse struct {
	Index []indexEntry `json:"index"`
}

type indexEntry struct {
	Slot      uint64 `json:"slot"`
	BlockRoot string `json:"block_root"`
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
	if c == nil {
		return nil, ErrNotFound
	}
	if bytes, ok, err := c.readCached(root); err != nil {
		return nil, err
	} else if ok {
		return bytes, nil
	}

	rootText := rootHex(root)
	slot, err := c.lookupSlot(root, rootText)
	if err != nil {
		return nil, err
	}

	bytes, err := c.downloadBlock(slot, rootText)
	if err != nil {
		return nil, err
	}
	if err := c.writeCached(root, bytes); err != nil {
		return nil, err
	}
	return cloneBytes(bytes), nil
}

func (c *Client) lookupSlot(root [32]byte, rootText string) (uint64, error) {
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
		return 0, fmt.Errorf("fetch block archive index: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read block archive index body: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
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

func (c *Client) downloadBlock(slot uint64, rootText string) ([]byte, error) {
	blockURL := fmt.Sprintf(
		"%s/%s/%d/%s.ssz",
		c.baseURL,
		url.PathEscape(c.network),
		slot,
		rootText,
	)
	resp, err := c.client.Get(blockURL)
	if err != nil {
		return nil, fmt.Errorf("download block archive block: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read block archive block body: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
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

func rootsEqual(text string, root [32]byte) bool {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "0x"), "0X")
	if len(text) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return false
	}
	return string(decoded) == string(root[:])
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
