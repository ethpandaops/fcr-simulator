package era

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

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
	require.Contains(t, err.Error(), "failed to download ERA 1 after 3 attempts")
	require.Equal(t, 3, attempts)
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
