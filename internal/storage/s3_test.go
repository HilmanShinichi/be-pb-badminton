package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalUpload(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "storage_test_*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	cfg := config.Config{
		UploadDir: tempDir,
		S3Bucket:  "payment-proofs",
	}
	s := New(cfg)
	assert.False(t, s.IsS3Configured())

	data := []byte("fake-image-bytes")
	url, err := s.Upload(context.Background(), "proof-1.jpg", data, "image/jpeg")
	require.NoError(t, err)
	assert.Equal(t, "/uploads/payment-proofs/proof-1.jpg", url)

	saved, err := os.ReadFile(filepath.Join(tempDir, "payment-proofs", "proof-1.jpg"))
	require.NoError(t, err)
	assert.Equal(t, data, saved)
}

func TestS3UploadWithMockServer(t *testing.T) {
	var receivedAuth string
	var receivedContentType string
	var receivedData []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/payment-proofs/test-proof.jpg", r.URL.Path)
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")
		receivedData, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := config.Config{
		S3Endpoint:  server.URL,
		S3Region:    "ap-southeast-1",
		S3Bucket:    "payment-proofs",
		S3AccessKey: "test-access-key",
		S3SecretKey: "test-secret-key",
	}
	s := New(cfg)
	assert.True(t, s.IsS3Configured())

	data := []byte("image-content-for-s3")
	url, err := s.Upload(context.Background(), "test-proof.jpg", data, "image/jpeg")
	require.NoError(t, err)
	assert.Equal(t, server.URL+"/payment-proofs/test-proof.jpg", url)
	assert.Equal(t, "image/jpeg", receivedContentType)
	assert.Equal(t, data, receivedData)
	assert.True(t, strings.HasPrefix(receivedAuth, "AWS4-HMAC-SHA256 Credential=test-access-key/"))
	assert.Contains(t, receivedAuth, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date")
	assert.Contains(t, receivedAuth, "Signature=")
}
