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

func TestResolveKey(t *testing.T) {
	s := &S3Storage{
		bucket:    "payment-proofs",
		endpoint:  "https://xyz.supabase.co/storage/v1/s3",
		publicURL: "",
	}

	// 1. Raw key
	assert.Equal(t, "proofs/p1/file.jpg", s.resolveKey("proofs/p1/file.jpg"))

	// 2. Raw key with leading slash
	assert.Equal(t, "proofs/p1/file.jpg", s.resolveKey("/proofs/p1/file.jpg"))

	// 3. Supabase public URL
	supabaseURL := "https://xyz.supabase.co/storage/v1/object/public/payment-proofs/proofs/p1/file.jpg"
	assert.Equal(t, "proofs/p1/file.jpg", s.resolveKey(supabaseURL))

	// 4. Standard S3 URL
	s3URL := "https://s3.ap-southeast-1.amazonaws.com/payment-proofs/proofs/p1/file.jpg"
	assert.Equal(t, "proofs/p1/file.jpg", s.resolveKey(s3URL))

	// 5. Local URL
	localURL := "/uploads/payment-proofs/proofs/p1/file.jpg"
	assert.Equal(t, "proofs/p1/file.jpg", s.resolveKey(localURL))

	// 6. Empty string
	assert.Equal(t, "", s.resolveKey(""))
}

func TestLocalDelete(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "storage_delete_test_*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	cfg := config.Config{
		UploadDir: tempDir,
		S3Bucket:  "payment-proofs",
	}
	s := New(cfg)

	data := []byte("temporary-proof-bytes")
	url, err := s.Upload(context.Background(), "delete-me.jpg", data, "image/jpeg")
	require.NoError(t, err)

	filePath := filepath.Join(tempDir, "payment-proofs", "delete-me.jpg")
	assert.FileExists(t, filePath)

	// Delete using the generated URL
	err = s.Delete(context.Background(), url)
	require.NoError(t, err)
	assert.NoFileExists(t, filePath)

	// Deleting again (already gone) should not fail
	err = s.Delete(context.Background(), url)
	require.NoError(t, err)
}

func TestS3DeleteWithMockServer(t *testing.T) {
	var receivedAuth string
	var deleteCount int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/payment-proofs/proofs/p1/test-delete.jpg", r.URL.Path)
		assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", r.Header.Get("x-amz-content-sha256"))
		receivedAuth = r.Header.Get("Authorization")
		deleteCount++
		w.WriteHeader(http.StatusNoContent)
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

	// Test deleting using raw key
	err := s.Delete(context.Background(), "proofs/p1/test-delete.jpg")
	require.NoError(t, err)
	assert.Equal(t, 1, deleteCount)
	assert.True(t, strings.HasPrefix(receivedAuth, "AWS4-HMAC-SHA256 Credential=test-access-key/"))
	assert.Contains(t, receivedAuth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date")

	// Test deleting using full Supabase-style URL
	fullURL := "https://supabase-ref.supabase.co/storage/v1/object/public/payment-proofs/proofs/p1/test-delete.jpg"
	err = s.Delete(context.Background(), fullURL)
	require.NoError(t, err)
	assert.Equal(t, 2, deleteCount)
}
