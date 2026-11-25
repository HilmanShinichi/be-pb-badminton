package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
)

type Storage interface {
	Upload(ctx context.Context, key string, data []byte, contentType string) (publicURL string, err error)
	Delete(ctx context.Context, keyOrURL string) error
}

type S3Storage struct {
	endpoint   string
	region     string
	bucket     string
	accessKey  string
	secretKey  string
	publicURL  string
	uploadDir  string
	httpClient *http.Client
}

func New(cfg config.Config) *S3Storage {
	return &S3Storage{
		endpoint:   strings.TrimRight(cfg.S3Endpoint, "/"),
		region:     cfg.S3Region,
		bucket:     cfg.S3Bucket,
		accessKey:  cfg.S3AccessKey,
		secretKey:  cfg.S3SecretKey,
		publicURL:  strings.TrimRight(cfg.S3PublicURL, "/"),
		uploadDir:  cfg.UploadDir,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// IsS3Configured returns true when remote S3 credentials are provided.
func (s *S3Storage) IsS3Configured() bool {
	return s.endpoint != "" && s.accessKey != "" && s.secretKey != "" && s.bucket != ""
}

// Upload uploads the file to S3 if configured, or saves to local disk as fallback.
func (s *S3Storage) Upload(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	if !s.IsS3Configured() {
		return s.uploadLocal(key, data)
	}
	return s.uploadS3(ctx, key, data, contentType)
}

func (s *S3Storage) uploadLocal(key string, data []byte) (string, error) {
	dir := filepath.Join(s.uploadDir, s.bucket)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create local upload dir: %w", err)
	}
	// Sanitize key filename
	cleanName := filepath.Base(key)
	dest := filepath.Join(dir, cleanName)
	if err := os.WriteFile(dest, data, 0644); err != nil {
		return "", fmt.Errorf("save local upload: %w", err)
	}
	return fmt.Sprintf("/uploads/%s/%s", s.bucket, cleanName), nil
}

func (s *S3Storage) uploadS3(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	rawURL := fmt.Sprintf("%s/%s/%s", s.endpoint, s.bucket, strings.TrimLeft(key, "/"))
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid s3 url: %w", err)
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	hash := sha256.Sum256(data)
	payloadHash := hex.EncodeToString(hash[:])

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, parsedURL.String(), bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create s3 request: %w", err)
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Host", parsedURL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical headers in alphabetical order
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		contentType, parsedURL.Host, payloadHash, amzDate)
	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"

	canonicalURI := parsedURL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		http.MethodPut,
		canonicalURI,
		"", // query string
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	reqHash := sha256.Sum256([]byte(canonicalRequest))
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hex.EncodeToString(reqHash[:]),
	}, "\n")

	signingKey := getSignatureKey(s.secretKey, dateStamp, s.region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("s3 put object request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("s3 upload failed (status %d): %s", resp.StatusCode, string(body))
	}

	return s.getPublicURL(key), nil
}

// Delete removes an object from storage (S3 or local disk).
// keyOrURL can be either a relative storage key (e.g. "proofs/.../img.jpg") or a full public URL.
func (s *S3Storage) Delete(ctx context.Context, keyOrURL string) error {
	key := s.resolveKey(keyOrURL)
	if key == "" {
		return nil
	}

	if s.IsS3Configured() {
		return s.deleteS3(ctx, key)
	}
	return s.deleteLocal(ctx, key)
}

func (s *S3Storage) resolveKey(keyOrURL string) string {
	raw := strings.TrimSpace(keyOrURL)
	if raw == "" {
		return ""
	}

	// If it's a URL, parse path
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err == nil {
			path := strings.Trim(u.Path, "/")
			if s.publicURL != "" {
				if pubU, err := url.Parse(s.publicURL); err == nil && pubU.Path != "" {
					prefix := strings.Trim(pubU.Path, "/")
					if strings.HasPrefix(path, prefix) {
						path = strings.TrimPrefix(path, prefix)
						path = strings.Trim(path, "/")
					}
				}
			}
			parts := strings.Split(path, "/")
			for i, p := range parts {
				if (p == "public" || p == "authenticated") && i+2 < len(parts) && parts[i+1] == s.bucket {
					return strings.Join(parts[i+2:], "/")
				}
			}
			if len(parts) > 1 && parts[0] == s.bucket {
				return strings.Join(parts[1:], "/")
			}
			if idx := strings.Index(path, "proofs/"); idx != -1 {
				return path[idx:]
			}
			raw = path
		}
	}

	raw = strings.TrimPrefix(raw, "/uploads/")
	raw = strings.TrimPrefix(raw, "uploads/")
	if s.bucket != "" {
		raw = strings.TrimPrefix(raw, s.bucket+"/")
	}

	return strings.Trim(raw, "/")
}

func (s *S3Storage) deleteLocal(ctx context.Context, key string) error {
	candidates := []string{
		filepath.Join(s.uploadDir, s.bucket, filepath.FromSlash(key)),
		filepath.Join(s.uploadDir, s.bucket, filepath.Base(key)),
		filepath.Join(s.uploadDir, filepath.FromSlash(key)),
	}

	for _, targetPath := range candidates {
		if err := os.Remove(targetPath); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete local file: %w", err)
		}
	}
	return nil
}

func (s *S3Storage) deleteS3(ctx context.Context, key string) error {
	reqURL := fmt.Sprintf("%s/%s/%s", s.endpoint, s.bucket, key)
	u, err := url.Parse(reqURL)
	if err != nil {
		return fmt.Errorf("invalid s3 delete URL: %w", err)
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	canonicalURI := u.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", u.Host, emptyPayloadSHA256, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalReq := fmt.Sprintf("DELETE\n%s\n\n%s\n%s\n%s", canonicalURI, canonicalHeaders, signedHeaders, emptyPayloadSHA256)

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.region)
	reqHash := sha256.Sum256([]byte(canonicalReq))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%x", amzDate, credentialScope, reqHash)

	signingKey := getSignatureKey(s.secretKey, dateStamp, s.region, "s3")
	signature := fmt.Sprintf("%x", hmacSHA256(signingKey, []byte(stringToSign)))
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, credentialScope, signedHeaders, signature)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create s3 delete request: %w", err)
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyPayloadSHA256)
	req.Header.Set("Authorization", authHeader)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s3 delete failed (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

func (s *S3Storage) getPublicURL(key string) string {
	if s.publicURL != "" {
		return fmt.Sprintf("%s/%s", s.publicURL, strings.TrimLeft(key, "/"))
	}
	// Check if endpoint is Supabase S3
	if strings.Contains(s.endpoint, "supabase.co") {
		// Supabase S3 endpoint: https://<ref>.supabase.co/storage/v1/s3
		// Supabase Public Object URL: https://<ref>.supabase.co/storage/v1/object/public/<bucket>/<key>
		base := strings.TrimSuffix(s.endpoint, "/s3")
		return fmt.Sprintf("%s/object/public/%s/%s", base, s.bucket, strings.TrimLeft(key, "/"))
	}
	// Default S3 URL
	return fmt.Sprintf("%s/%s/%s", s.endpoint, s.bucket, strings.TrimLeft(key, "/"))
}

func hmacSHA256(key []byte, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func getSignatureKey(key, dateStamp, regionName, serviceName string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+key), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(regionName))
	kService := hmacSHA256(kRegion, []byte(serviceName))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}
