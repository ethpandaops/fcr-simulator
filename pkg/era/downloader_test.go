package era

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethpandaops/fcr-simulator/pkg/s3cache"
	"github.com/stretchr/testify/require"
)

func TestNewDownloaderCreatesEraCacheDir(t *testing.T) {
	baseDir := t.TempDir()

	downloader, err := NewDownloader("https://example.invalid/", baseDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(baseDir, "era"), downloader.CacheDir())

	info, err := os.Stat(downloader.CacheDir())
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestDownloaderPreDownloadCachesNeededEras(t *testing.T) {
	files := map[string][]byte{
		"mainnet-00001-aaaa.era": []byte("era-one"),
		"mainnet-00002-bbbb.era": []byte("era-two"),
	}

	downloads := make(map[string]int)
	downloader, err := NewDownloader("https://era.example/", t.TempDir())
	require.NoError(t, err)
	downloader.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "" {
				body := `<a href="mainnet-00001-aaaa.era">one</a>` + "\n" +
					`<a href="mainnet-00002-bbbb.era">two</a>` + "\n"
				return httpResponse(req, http.StatusOK, []byte(body)), nil
			}

			filename := path.Base(req.URL.Path)
			data, ok := files[filename]
			if !ok {
				return httpResponse(req, http.StatusNotFound, []byte("missing")), nil
			}

			downloads[filename]++
			return httpResponse(req, http.StatusOK, data), nil
		}),
	}

	require.NoError(t, downloader.PreDownload(8191, 8191))
	require.Equal(t, files["mainnet-00001-aaaa.era"], readCachedFile(t, downloader.CacheDir(), "mainnet-00001-aaaa.era"))
	require.Equal(t, files["mainnet-00002-bbbb.era"], readCachedFile(t, downloader.CacheDir(), "mainnet-00002-bbbb.era"))

	require.NoError(t, downloader.PreDownload(8191, 8191))

	require.Equal(t, 1, downloads["mainnet-00001-aaaa.era"])
	require.Equal(t, 1, downloads["mainnet-00002-bbbb.era"])
}

func TestDownloaderPreDownloadUsesS3BeforeHTTP(t *testing.T) {
	store := newFakeEraS3Store(map[string][]byte{
		"era/mainnet/mainnet-00001-aaaa.era": []byte("era-one"),
	})
	downloader, err := NewDownloaderWithS3("https://era.example/", t.TempDir(), "mainnet", store)
	require.NoError(t, err)
	downloader.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("HTTP should not be called on S3 hit: %s", req.URL.String())
			return nil, nil
		}),
	}

	require.NoError(t, downloader.PreDownload(0, 0))
	require.Equal(t, []byte("era-one"), readCachedFile(t, downloader.CacheDir(), "mainnet-00001-aaaa.era"))
	require.Equal(t, []string{"era/mainnet/mainnet-00001-"}, store.lists)
	require.Equal(t, []string{"era/mainnet/mainnet-00001-aaaa.era"}, store.downloads)
}

func TestDownloaderPreDownloadFallsBackToHTTPAndUploadsToS3(t *testing.T) {
	store := newFakeEraS3Store(nil)
	downloader, err := NewDownloaderWithS3("https://era.example/", t.TempDir(), "mainnet", store)
	require.NoError(t, err)
	downloader.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "" {
				return httpResponse(req, http.StatusOK, []byte(`<a href="mainnet-00001-aaaa.era">one</a>`)), nil
			}
			require.Equal(t, "/mainnet-00001-aaaa.era", req.URL.Path)
			return httpResponse(req, http.StatusOK, []byte("era-one")), nil
		}),
	}

	require.NoError(t, downloader.PreDownload(0, 0))
	require.Equal(t, []byte("era-one"), readCachedFile(t, downloader.CacheDir(), "mainnet-00001-aaaa.era"))
	require.Equal(t, []byte("era-one"), store.objects["era/mainnet/mainnet-00001-aaaa.era"])
	require.Equal(t, []string{"era/mainnet/mainnet-00001-"}, store.lists)
	require.Equal(t, []string{"era/mainnet/mainnet-00001-aaaa.era"}, store.uploads)
}

func TestDownloaderMirrorToS3SkipsExistingAndUploadsMissing(t *testing.T) {
	store := newFakeEraS3Store(map[string][]byte{
		"era/mainnet/mainnet-00001-aaaa.era": []byte("already-present"),
	})
	downloader, err := NewDownloaderWithS3("https://era.example/", t.TempDir(), "mainnet", store)
	require.NoError(t, err)
	downloader.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "" {
				body := `<a href="mainnet-00001-aaaa.era">one</a>` + "\n" +
					`<a href="mainnet-00002-bbbb.era">two</a>` + "\n"
				return httpResponse(req, http.StatusOK, []byte(body)), nil
			}
			require.Equal(t, "/mainnet-00002-bbbb.era", req.URL.Path)
			return httpResponse(req, http.StatusOK, []byte("era-two")), nil
		}),
	}

	stats, err := downloader.MirrorToS3(context.Background(), 1, 2, 1)
	require.NoError(t, err)
	require.Equal(t, MirrorStats{Scanned: 2, Skipped: 1, Downloaded: 1, Uploaded: 1}, stats)
	require.Equal(t, []byte("already-present"), store.objects["era/mainnet/mainnet-00001-aaaa.era"])
	require.Equal(t, []byte("era-two"), store.objects["era/mainnet/mainnet-00002-bbbb.era"])
	require.Equal(t, []string{"era/mainnet/mainnet-00001-aaaa.era", "era/mainnet/mainnet-00002-bbbb.era"}, store.exists)
	require.Equal(t, []string{"era/mainnet/mainnet-00002-bbbb.era"}, store.uploads)
	require.NoFileExists(t, filepath.Join(downloader.CacheDir(), "mainnet-00002-bbbb.era"))
}

func TestDownloaderMissingEraInIndex(t *testing.T) {
	downloader, err := NewDownloader("https://era.example", t.TempDir())
	require.NoError(t, err)
	downloader.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpResponse(req, http.StatusOK, []byte(`<a href="mainnet-00001-aaaa.era">one</a>`)), nil
		}),
	}

	err = downloader.PreDownload(8192, 8192)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ERA file not found for era number 2")
}

func TestDownloaderRetriesFailedDownloads(t *testing.T) {
	oldSleep := downloaderRetrySleep
	downloaderRetrySleep = func(time.Duration) {}
	t.Cleanup(func() {
		downloaderRetrySleep = oldSleep
	})

	attempts := 0
	downloader, err := NewDownloader("https://era.example", t.TempDir())
	require.NoError(t, err)
	downloader.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "" {
				return httpResponse(req, http.StatusOK, []byte(`<a href="mainnet-00001-aaaa.era">one</a>`)), nil
			}

			attempts++
			return httpResponse(req, http.StatusInternalServerError, []byte("nope")), nil
		}),
	}

	err = downloader.PreDownload(0, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("failed to download ERA 1 after %d attempts", defaultMaxRetries))
	require.Equal(t, defaultMaxRetries, attempts)
}

func TestDownloaderInvalidRangeAndNilCacheDir(t *testing.T) {
	var nilDownloader *Downloader
	require.Equal(t, "", nilDownloader.CacheDir())

	downloader, err := NewDownloader("https://era.example", t.TempDir())
	require.NoError(t, err)

	require.Error(t, downloader.PreDownload(2, 1))
	require.Error(t, nilDownloader.PreDownload(0, 0))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpResponse(req *http.Request, statusCode int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
}

func readCachedFile(t *testing.T, dir, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return data
}

type fakeEraS3Store struct {
	mu        sync.Mutex
	objects   map[string][]byte
	lists     []string
	downloads []string
	uploads   []string
	exists    []string
}

func newFakeEraS3Store(objects map[string][]byte) *fakeEraS3Store {
	cloned := make(map[string][]byte)
	for key, value := range objects {
		cloned[key] = append([]byte(nil), value...)
	}
	return &fakeEraS3Store{objects: cloned}
}

func (s *fakeEraS3Store) Download(_ context.Context, _ string) ([]byte, bool, error) {
	panic("unexpected Download call")
}

func (s *fakeEraS3Store) DownloadFile(_ context.Context, key, target string) (bool, error) {
	s.mu.Lock()
	s.downloads = append(s.downloads, key)
	data, ok := s.objects[key]
	data = append([]byte(nil), data...)
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, os.WriteFile(target, data, 0o644)
}

func (s *fakeEraS3Store) Upload(_ context.Context, _ string, _ []byte) error {
	panic("unexpected Upload call")
}

func (s *fakeEraS3Store) UploadFile(_ context.Context, key, source string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, key)
	s.objects[key] = append([]byte(nil), data...)
	return nil
}

func (s *fakeEraS3Store) ObjectExists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exists = append(s.exists, key)
	_, ok := s.objects[key]
	return ok, nil
}

func (s *fakeEraS3Store) ListObjects(_ context.Context, prefix string) ([]s3cache.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists = append(s.lists, prefix)
	var objects []s3cache.Object
	for key, data := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, s3cache.Object{Key: key, Size: int64(len(data))})
		}
	}
	return objects, nil
}
