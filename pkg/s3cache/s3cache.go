package s3cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store is the small object-store surface used by the simulator cache layers.
type Store interface {
	Download(context.Context, string) ([]byte, bool, error)
	DownloadFile(context.Context, string, string) (bool, error)
	Upload(context.Context, string, []byte) error
	UploadFile(context.Context, string, string) error
	ObjectExists(context.Context, string) (bool, error)
	ListObjects(context.Context, string) ([]Object, error)
}

// Config configures an S3-compatible object store.
type Config struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool
}

// Object is a minimal S3 object listing entry.
type Object struct {
	Key  string
	Size int64
}

// Client is an S3-compatible cache client backed by minio-go.
type Client struct {
	client *minio.Client
	bucket string
}

// New creates an S3-compatible cache client.
func New(cfg Config) (*Client, error) {
	endpoint, secure, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	bucket := strings.TrimSpace(cfg.Bucket)
	if bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	if strings.TrimSpace(cfg.AccessKeyID) == "" {
		return nil, fmt.Errorf("AWS_ACCESS_KEY_ID is required when S3 cache is configured")
	}
	if strings.TrimSpace(cfg.SecretAccessKey) == "" {
		return nil, fmt.Errorf("AWS_SECRET_ACCESS_KEY is required when S3 cache is configured")
	}

	options := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: secure,
	}
	if cfg.PathStyle {
		options.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(endpoint, options)
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	return &Client{client: client, bucket: bucket}, nil
}

// Download reads an object into memory. ok is false when the object is absent.
func (c *Client) Download(ctx context.Context, key string) ([]byte, bool, error) {
	if ok, err := c.ObjectExists(ctx, key); err != nil || !ok {
		return nil, ok, err
	}

	object, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("get S3 object %q: %w", key, err)
	}
	defer object.Close()

	data, err := io.ReadAll(object)
	if err != nil {
		return nil, false, fmt.Errorf("read S3 object %q: %w", key, err)
	}
	return data, true, nil
}

// DownloadFile downloads an object to path atomically. ok is false when absent.
func (c *Client) DownloadFile(ctx context.Context, key, path string) (bool, error) {
	if ok, err := c.ObjectExists(ctx, key); err != nil || !ok {
		return ok, err
	}

	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, fmt.Errorf("create cache directory %q: %w", parent, err)
	}

	tempFile, err := os.CreateTemp(parent, ".s3-*")
	if err != nil {
		return false, fmt.Errorf("create temp file in %q: %w", parent, err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return false, fmt.Errorf("close temp file %q: %w", tempPath, err)
	}

	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := c.client.FGetObject(ctx, c.bucket, key, tempPath, minio.GetObjectOptions{}); err != nil {
		if isObjectNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("download S3 object %q: %w", key, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return false, fmt.Errorf("move S3 cache file into place %q: %w", path, err)
	}
	keepTemp = true
	return true, nil
}

// Upload writes data to an object.
func (c *Client) Upload(ctx context.Context, key string, data []byte) error {
	_, err := c.client.PutObject(ctx, c.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("upload S3 object %q: %w", key, err)
	}
	return nil
}

// UploadFile writes a local file to an object.
func (c *Client) UploadFile(ctx context.Context, key, path string) error {
	_, err := c.client.FPutObject(ctx, c.bucket, key, path, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("upload file %q to S3 object %q: %w", path, key, err)
	}
	return nil
}

// ObjectExists checks whether an object exists.
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.client.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isObjectNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat S3 object %q: %w", key, err)
}

// ListObjects lists objects under prefix.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	for object := range c.client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if object.Err != nil {
			return nil, fmt.Errorf("list S3 objects with prefix %q: %w", prefix, object.Err)
		}
		objects = append(objects, Object{Key: object.Key, Size: object.Size})
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})
	return objects, nil
}

func normalizeEndpoint(raw string) (endpoint string, secure bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, fmt.Errorf("S3 endpoint is required")
	}

	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("S3 endpoint is not a valid URL: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		secure = false
	case "https":
		secure = true
	default:
		return "", false, fmt.Errorf("S3 endpoint must use http or https")
	}
	if parsed.Host == "" {
		return "", false, fmt.Errorf("S3 endpoint must include a host")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", false, fmt.Errorf("S3 endpoint must not include a path")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false, fmt.Errorf("S3 endpoint must not include query or fragment")
	}

	return parsed.Host, secure, nil
}

func isObjectNotFound(err error) bool {
	var response minio.ErrorResponse
	if errors.As(err, &response) {
		return response.Code == "NoSuchKey" ||
			response.Code == "NotFound" ||
			(response.StatusCode == http.StatusNotFound && response.Code == "")
	}
	return false
}
