package blockarchive

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientFetchBlockSSZByRootURLConstructionAndCache(t *testing.T) {
	root := testRoot(0x98)
	rootText := rootHex(root)
	cacheDir := t.TempDir()
	var indexRequests int
	var downloadRequests int
	client, err := New("http://archive.test/", "mainnet", cacheDir)
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/index":
			indexRequests++
			require.Equal(t, "mainnet", r.URL.Query().Get("network"))
			require.Equal(t, rootText, r.URL.Query().Get("block_root_prefix"))
			require.Equal(t, "1", r.URL.Query().Get("limit"))
			return response(200, `{"index":[{"network":"mainnet","slot":13598720,"block_root":"`+rootText+`","execution_block_hash":"0x00","indexed_at_ns":1}]}`), nil
		case "/mainnet/13598720/" + rootText + ".ssz":
			downloadRequests++
			return response(200, "block-ssz"), nil
		default:
			return response(404, "not found"), nil
		}
	})}

	got, err := client.FetchBlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("block-ssz"), got)
	require.Equal(t, 1, indexRequests)
	require.Equal(t, 1, downloadRequests)

	cachePath := filepath.Join(cacheDir, rootText+".ssz")
	require.Equal(t, []byte("block-ssz"), mustReadFile(t, cachePath))

	got[0] = 'X'
	again, err := client.FetchBlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("block-ssz"), again)
	require.Equal(t, 1, indexRequests, "second fetch should use cache")
	require.Equal(t, 1, downloadRequests, "second fetch should use cache")
}

func TestClientFetchBlockSSZByRootRejectsIndexRootMismatch(t *testing.T) {
	root := testRoot(0x98)
	wrongRoot := rootHex(testRoot(0x99))
	client, err := New("http://archive.test", "mainnet", t.TempDir())
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/v1/index", r.URL.Path)
		return response(200, `{"index":[{"slot":13598720,"block_root":"`+wrongRoot+`"}]}`), nil
	})}

	got, err := client.FetchBlockSSZByRoot(root)
	require.Nil(t, got)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClientFetchBlockSSZByRootDownload404(t *testing.T) {
	root := testRoot(0x98)
	rootText := rootHex(root)
	client, err := New("http://archive.test", "mainnet", t.TempDir())
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/index":
			return response(200, `{"index":[{"slot":13598720,"block_root":"`+rootText+`"}]}`), nil
		case "/mainnet/13598720/" + rootText + ".ssz":
			return response(404, "not found"), nil
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
			return nil, nil
		}
	})}

	got, err := client.FetchBlockSSZByRoot(root)
	require.Nil(t, got)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClientFetchBlockSSZByRootRetriesTransientIndexFailure(t *testing.T) {
	root := testRoot(0x98)
	rootText := rootHex(root)
	var indexRequests int
	client, err := New("http://archive.test", "mainnet", t.TempDir())
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/index":
			indexRequests++
			if indexRequests == 1 {
				return response(500, "temporary failure"), nil
			}
			return response(200, `{"index":[{"slot":13598720,"block_root":"`+rootText+`"}]}`), nil
		case "/mainnet/13598720/" + rootText + ".ssz":
			return response(200, "block-ssz"), nil
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
			return nil, nil
		}
	})}

	got, stats, err := client.FetchBlockSSZByRootWithStats(root)
	require.NoError(t, err)
	require.Equal(t, []byte("block-ssz"), got)
	require.Equal(t, 2, indexRequests)
	require.Equal(t, 1, stats.TransientFailuresRetried)
	require.True(t, stats.Downloaded)
}

func TestClientListIndexBySlotRangeURLConstruction(t *testing.T) {
	root0 := rootHex(testRoot(0x11))
	root1 := rootHex(testRoot(0x22))
	var offsets []string
	client, err := New("http://archive.test", "mainnet", t.TempDir())
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/v1/index", r.URL.Path)
		require.Equal(t, "mainnet", r.URL.Query().Get("network"))
		require.Equal(t, "13774990", r.URL.Query().Get("slot_min"))
		require.Equal(t, "13774991", r.URL.Query().Get("slot_max"))
		require.Equal(t, "2", r.URL.Query().Get("limit"))
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)
		switch offset {
		case "0":
			return response(200, `{"index":[{"slot":13774990,"block_root":"`+root0+`"},{"slot":13774991,"block_root":"`+root1+`"}]}`), nil
		case "2":
			return response(200, `{"index":[]}`), nil
		default:
			t.Fatalf("unexpected offset %q", offset)
			return nil, nil
		}
	})}

	got, stats, err := client.ListIndexBySlotRangeWithStats(13774990, 13774991, 2)
	require.NoError(t, err)
	require.Zero(t, stats.TransientFailuresRetried)
	require.Equal(t, []string{"0", "2"}, offsets)
	require.Equal(t, []IndexEntry{
		{Slot: 13774990, BlockRoot: root0},
		{Slot: 13774991, BlockRoot: root1},
	}, got)
}

func TestClientListIndexBySlotRangePaginatesCappedResponses(t *testing.T) {
	const (
		totalEntries = 1025
		serverCap    = 500
		slotMin      = 13774000
	)
	allEntries := make([]IndexEntry, 0, totalEntries)
	for i := 0; i < totalEntries; i++ {
		allEntries = append(allEntries, IndexEntry{
			Slot:      slotMin + uint64(i),
			BlockRoot: testRootText(i),
		})
	}

	var offsets []int
	client, err := New("http://archive.test", "mainnet", t.TempDir())
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/v1/index", r.URL.Path)
		require.Equal(t, "mainnet", r.URL.Query().Get("network"))
		require.Equal(t, strconv.FormatUint(slotMin, 10), r.URL.Query().Get("slot_min"))
		require.Equal(t, strconv.FormatUint(slotMin+totalEntries-1, 10), r.URL.Query().Get("slot_max"))
		require.Equal(t, "1000", r.URL.Query().Get("limit"))
		require.NotEmpty(t, r.URL.Query().Get("offset"))

		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		require.NoError(t, err)
		offsets = append(offsets, offset)
		if offset >= len(allEntries) {
			return response(200, `{"index":[]}`), nil
		}
		end := offset + serverCap
		if end > len(allEntries) {
			end = len(allEntries)
		}
		return response(200, indexResponseBody(t, allEntries[offset:end])), nil
	})}

	got, stats, err := client.ListIndexBySlotRangeWithStats(slotMin, slotMin+totalEntries-1, 5000)
	require.NoError(t, err)
	require.Zero(t, stats.TransientFailuresRetried)
	require.Equal(t, []int{0, 500, 1000, 1025}, offsets)
	require.Equal(t, allEntries, got)
}

func TestClientFetchBlockSSZBySlotAndRootSkipsIndexLookup(t *testing.T) {
	root := testRoot(0x46)
	rootText := rootHex(root)
	var requests []string
	client, err := New("http://archive.test", "mainnet", t.TempDir())
	require.NoError(t, err)
	client.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/mainnet/123/" + rootText + ".ssz":
			return response(200, "slot-root-block"), nil
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
			return nil, nil
		}
	})}

	got, stats, err := client.FetchBlockSSZBySlotAndRootWithStats(123, root)
	require.NoError(t, err)
	require.Equal(t, []byte("slot-root-block"), got)
	require.True(t, stats.Downloaded)
	require.Equal(t, []string{"/mainnet/123/" + rootText + ".ssz"}, requests)
}

func TestNewRejectsMissingConfig(t *testing.T) {
	for i, tc := range []struct {
		baseURL  string
		network  string
		cacheDir string
	}{
		{"", "mainnet", t.TempDir()},
		{"http://example.test", "", t.TempDir()},
		{"http://example.test", "mainnet", ""},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			client, err := New(tc.baseURL, tc.network, tc.cacheDir)
			require.Nil(t, client)
			require.Error(t, err)
		})
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func testRoot(fill byte) [32]byte {
	var root [32]byte
	for i := range root {
		root[i] = fill
	}
	return root
}

func testRootText(i int) string {
	var root [32]byte
	root[30] = byte(i >> 8)
	root[31] = byte(i)
	return rootHex(root)
}

func indexResponseBody(t *testing.T, entries []IndexEntry) string {
	t.Helper()
	body, err := json.Marshal(indexResponse{Index: entries})
	require.NoError(t, err)
	return string(body)
}

func TestRootsEqual(t *testing.T) {
	root := testRoot(0xab)
	require.True(t, rootsEqual(rootHex(root), root))
	require.True(t, rootsEqual("0X"+rootHex(root)[2:], root))
	require.False(t, rootsEqual(rootHex(testRoot(0xac)), root))
	require.False(t, rootsEqual("0x1234", root))
	require.False(t, rootsEqual("0x"+string(make([]byte, 64)), root))

	parsed, err := ParseRoot(rootHex(root))
	require.NoError(t, err)
	require.Equal(t, root, parsed)
}

func TestReadCachedMiss(t *testing.T) {
	client, err := New("http://example.test", "mainnet", t.TempDir())
	require.NoError(t, err)

	got, ok, err := client.readCached(testRoot(0x01))
	require.Nil(t, got)
	require.False(t, ok)
	require.NoError(t, err)
	require.False(t, errors.Is(err, ErrNotFound))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func response(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
