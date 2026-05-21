package s3cache

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeEndpoint(t *testing.T) {
	endpoint, secure, err := normalizeEndpoint("http://rook-ceph-rgw-ceph-objectstore.rook-ceph.svc:8080")
	require.NoError(t, err)
	require.Equal(t, "rook-ceph-rgw-ceph-objectstore.rook-ceph.svc:8080", endpoint)
	require.False(t, secure)

	endpoint, secure, err = normalizeEndpoint("s3.example.com")
	require.NoError(t, err)
	require.Equal(t, "s3.example.com", endpoint)
	require.False(t, secure)

	endpoint, secure, err = normalizeEndpoint("https://s3.example.com")
	require.NoError(t, err)
	require.Equal(t, "s3.example.com", endpoint)
	require.True(t, secure)
}

func TestNormalizeEndpointRejectsPath(t *testing.T) {
	_, _, err := normalizeEndpoint("http://s3.example.com/cache")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not include a path")
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{
		Endpoint:  "http://s3.example.com",
		Bucket:    "fcr-simulator",
		PathStyle: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "AWS_ACCESS_KEY_ID")
}
