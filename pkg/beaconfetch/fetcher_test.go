package beaconfetch

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFetchStateSSZAtSlot_Cached(t *testing.T) {
	var calls atomic.Int32
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected HTTP call", http.StatusInternalServerError)
	})
	cachePath := filepath.Join(fetcher.cacheDir, "states", "state-123.ssz")
	require.NoError(t, os.WriteFile(cachePath, []byte("cached-state"), 0o644))

	data, err := fetcher.FetchStateSSZAtSlot(123)
	require.NoError(t, err)
	require.Equal(t, []byte("cached-state"), data)
	require.Zero(t, calls.Load())
}

func TestFetchStateSSZAtSlot_404ReturnsErrNotFound(t *testing.T) {
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/eth/v2/debug/beacon/states/123", r.URL.Path)
		require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
		http.NotFound(w, r)
	})

	data, err := fetcher.FetchStateSSZAtSlot(123)
	require.Nil(t, data)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFetchStateSSZAtSlot_HTTP200WritesCache(t *testing.T) {
	var calls atomic.Int32
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/eth/v2/debug/beacon/states/456", r.URL.Path)
		require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
		_, _ = w.Write([]byte("state-ssz"))
	})

	data, err := fetcher.FetchStateSSZAtSlot(456)
	require.NoError(t, err)
	require.Equal(t, []byte("state-ssz"), data)
	require.Equal(t, []byte("state-ssz"), readCacheFile(t, fetcher.cacheDir, "states", "state-456.ssz"))

	data, err = fetcher.FetchStateSSZAtSlot(456)
	require.NoError(t, err)
	require.Equal(t, []byte("state-ssz"), data)
	require.EqualValues(t, 1, calls.Load())
}

func TestFetchGenesisStateSSZ_HTTP200WritesCache(t *testing.T) {
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/eth/v2/debug/beacon/states/genesis", r.URL.Path)
		require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
		_, _ = w.Write([]byte("genesis-state"))
	})

	data, err := fetcher.FetchGenesisStateSSZ()
	require.NoError(t, err)
	require.Equal(t, []byte("genesis-state"), data)
	require.Equal(t, []byte("genesis-state"), readCacheFile(t, fetcher.cacheDir, "states", "state-genesis.ssz"))
}

func TestFetchBlockSSZByRoot_Cached(t *testing.T) {
	var calls atomic.Int32
	root := testRoot(0x11)
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected HTTP call", http.StatusInternalServerError)
	})
	cachePath := filepath.Join(fetcher.cacheDir, "blocks", fmt.Sprintf("block-%s.ssz", rootHex(root)))
	require.NoError(t, os.WriteFile(cachePath, []byte("cached-block"), 0o644))

	data, err := fetcher.FetchBlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("cached-block"), data)
	require.Zero(t, calls.Load())
}

func TestFetchBlockSSZByRoot_404ReturnsErrNotFound(t *testing.T) {
	root := testRoot(0x22)
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/eth/v2/beacon/blocks/"+rootHex(root), r.URL.Path)
		require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
		http.NotFound(w, r)
	})

	data, err := fetcher.FetchBlockSSZByRoot(root)
	require.Nil(t, data)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFetchBlockSSZByRoot_HTTP200WritesCache(t *testing.T) {
	root := testRoot(0x33)
	var calls atomic.Int32
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/eth/v2/beacon/blocks/"+rootHex(root), r.URL.Path)
		require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
		_, _ = w.Write([]byte("block-ssz"))
	})

	data, err := fetcher.FetchBlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("block-ssz"), data)
	require.Equal(t, []byte("block-ssz"), readCacheFile(t, fetcher.cacheDir, "blocks", fmt.Sprintf("block-%s.ssz", rootHex(root))))

	data, err = fetcher.FetchBlockSSZByRoot(root)
	require.NoError(t, err)
	require.Equal(t, []byte("block-ssz"), data)
	require.EqualValues(t, 1, calls.Load())
}

func TestCheckpointBlockRootAtSlot_JSONParsing(t *testing.T) {
	root := testRoot(0x44)
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/eth/v1/beacon/headers/789", r.URL.Path)
		require.Equal(t, acceptJSON, r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"root":%q,"canonical":true,"header":{"message":{},"signature":"0x00"}}}`, rootHex(root))
	})

	got, err := fetcher.CheckpointBlockRootAtSlot(789)
	require.NoError(t, err)
	require.Equal(t, root, got)
}

func TestFetchCheckpointAtWarmupSlot_StraightHit(t *testing.T) {
	root := testRoot(0x55)
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/eth/v1/beacon/headers/100":
			require.Equal(t, acceptJSON, r.Header.Get("Accept"))
			_, _ = fmt.Fprintf(w, `{"data":{"root":%q}}`, rootHex(root))
		case "/eth/v2/debug/beacon/states/100":
			require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
			_, _ = w.Write([]byte("state-100"))
		default:
			http.NotFound(w, r)
		}
	})

	actualSlot, stateSSZ, err := fetcher.FetchCheckpointAtWarmupSlot(100)
	require.NoError(t, err)
	require.Equal(t, uint64(100), actualSlot)
	require.Equal(t, []byte("state-100"), stateSSZ)
}

func TestFetchCheckpointAtWarmupSlot_MissedSlotFallback(t *testing.T) {
	root := testRoot(0x66)
	var headerCalls atomic.Int32
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/eth/v1/beacon/headers/"):
			headerCalls.Add(1)
			slot := pathSlot(t, r.URL.Path)
			require.Equal(t, acceptJSON, r.Header.Get("Accept"))
			if slot == 97 {
				_, _ = fmt.Fprintf(w, `{"data":{"root":%q}}`, rootHex(root))
				return
			}
			http.NotFound(w, r)
		case r.URL.Path == "/eth/v2/debug/beacon/states/97":
			require.Equal(t, acceptSSZ, r.Header.Get("Accept"))
			_, _ = w.Write([]byte("state-97"))
		default:
			http.NotFound(w, r)
		}
	})

	actualSlot, stateSSZ, err := fetcher.FetchCheckpointAtWarmupSlot(100)
	require.NoError(t, err)
	require.Equal(t, uint64(97), actualSlot)
	require.Equal(t, []byte("state-97"), stateSSZ)
	require.EqualValues(t, 4, headerCalls.Load())
}

func TestFetchCheckpointAtWarmupSlot_ExhaustedFallback(t *testing.T) {
	var headerCalls atomic.Int32
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/eth/v1/beacon/headers/") {
			headerCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		t.Fatalf("unexpected path %s", r.URL.Path)
	})

	actualSlot, stateSSZ, err := fetcher.FetchCheckpointAtWarmupSlot(100)
	require.Zero(t, actualSlot)
	require.Nil(t, stateSSZ)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotFound), "expected error to wrap ErrNotFound: %v", err)
	require.EqualValues(t, 33, headerCalls.Load())
}

func TestFetchCheckpointAtWarmupSlot_StateNotFoundAfterHeaderHit(t *testing.T) {
	root := testRoot(0x77)
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/eth/v1/beacon/headers/100":
			_, _ = fmt.Fprintf(w, `{"data":{"root":%q}}`, rootHex(root))
		case "/eth/v2/debug/beacon/states/100":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	_, _, err := fetcher.FetchCheckpointAtWarmupSlot(100)
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "checkpoint state at slot 100")
}

func TestParseRootRejectsMalformedValues(t *testing.T) {
	_, err := parseRoot("")
	require.Error(t, err)

	_, err = parseRoot("0x1234")
	require.Error(t, err)

	_, err = parseRoot("0x" + strings.Repeat("zz", 32))
	require.Error(t, err)
}

func TestRootHexUses0xPrefix(t *testing.T) {
	root := testRoot(0xaa)
	require.Equal(t, "0x"+strings.Repeat(hex.EncodeToString([]byte{0xaa}), 32), rootHex(root))
}

func TestNewValidationAndCacheDirErrors(t *testing.T) {
	_, err := New("", t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "beacon node URL")

	_, err = New("http://beacon.example", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cache directory")

	cacheFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(cacheFile, []byte("file"), 0o644))
	_, err = New("http://beacon.example", cacheFile)
	require.Error(t, err)
	require.Contains(t, err.Error(), "states cache directory")
}

func TestFetchHTTP500DoesNotWriteCache(t *testing.T) {
	fetcher := newTestFetcher(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/eth/v2/debug/beacon/states/12", r.URL.Path)
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	data, err := fetcher.FetchStateSSZAtSlot(12)
	require.Nil(t, data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 500")
	_, statErr := os.Stat(filepath.Join(fetcher.cacheDir, "states", "state-12.ssz"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCheckpointBlockRootAtSlot_InvalidResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name: "HTTP 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			want: "HTTP 500",
		},
		{
			name: "invalid JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("{"))
			},
			want: "failed to decode beacon header response",
		},
		{
			name: "missing root",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":{}}`))
			},
			want: "failed to parse beacon header root",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := newTestFetcher(t, t.TempDir(), test.handler)
			_, err := fetcher.CheckpointBlockRootAtSlot(1)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.want)
		})
	}
}

func TestWriteFileAtomicParentError(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "parent-file")
	require.NoError(t, os.WriteFile(parentFile, []byte("file"), 0o644))

	err := writeFileAtomic(filepath.Join(parentFile, "child.ssz"), []byte("data"))
	require.Error(t, err)
}

func newTestFetcher(t *testing.T, cacheDir string, handler http.HandlerFunc) *Fetcher {
	t.Helper()

	fetcher := mustNewFetcher(t, "http://beacon.example", cacheDir)
	fetcher.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			response := recorder.Result()
			response.Request = req
			return response, nil
		}),
	}
	return fetcher
}

func mustNewFetcher(t *testing.T, beaconNodeURL, cacheDir string) *Fetcher {
	t.Helper()

	fetcher, err := New(beaconNodeURL, cacheDir)
	require.NoError(t, err)
	return fetcher
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testRoot(fill byte) [32]byte {
	var root [32]byte
	for i := range root {
		root[i] = fill
	}
	return root
}

func readCacheFile(t *testing.T, cacheDir string, elems ...string) []byte {
	t.Helper()

	pathElems := append([]string{cacheDir}, elems...)
	data, err := os.ReadFile(filepath.Join(pathElems...))
	require.NoError(t, err)
	return data
}

func pathSlot(t *testing.T, requestPath string) uint64 {
	t.Helper()

	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	require.NotEmpty(t, parts)
	slot, err := strconv.ParseUint(parts[len(parts)-1], 10, 64)
	require.NoError(t, err)
	return slot
}
