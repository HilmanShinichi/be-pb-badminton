package matchmaker

import (
	"fmt"
	"testing"

	"github.com/pb-kecebong/backend/internal/httpx"
)

// TestIsSchemaRejection covers the error bodies that must fall through to the
// next response_format mode, and the ones that must not.
func TestIsSchemaRejection(t *testing.T) {
	reject := []string{
		// The real production error: agentrouter echoes the payload back.
		`AI provider HTTP 400: {"error":{"message":"[400]: {\"model\":\"deepseek-v4.1-flash-free\"}","type":"invalid_request_error","code":"bad_request"}}`,
		`AI provider HTTP 400: {"error":{"message":"response_format is not supported"}}`,
		`AI provider HTTP 400: json_validate_failed`,
		`AI provider HTTP 400: unknown field json_schema`,
		`AI provider HTTP 422: {"error":{"message":"[422]: {\"model\":\"x\"}"}}`,
	}
	for _, body := range reject {
		err := httpx.BadRequest("AI_PROVIDER_ERROR", body)
		if !isSchemaRejection(err) {
			t.Errorf("should fall through to the next mode: %s", body)
		}
	}

	// A real 400 names its problem; retrying with a different response_format
	// cannot fix it and would waste a call before the same failure.
	keep := []string{
		`AI provider HTTP 400: {"error":{"message":"model not found"}}`,
		`AI provider HTTP 400: {"error":{"message":"Incorrect API key provided"}}`,
		`AI provider HTTP 400: {"error":{"message":"This model's maximum context length is 8192 tokens"}}`,
		`AI provider HTTP 401: unauthorized`,
		`AI provider HTTP 500: internal error`,
	}
	for _, body := range keep {
		err := httpx.BadRequest("AI_PROVIDER_ERROR", body)
		if isSchemaRejection(err) {
			t.Errorf("should fail without retrying another mode: %s", body)
		}
	}
}

// TestIsSchemaRejectionPlainError makes sure a non-HTTP error message is not
// mistaken for a schema rejection by accident.
func TestIsSchemaRejectionPlainError(t *testing.T) {
	if isSchemaRejection(fmt.Errorf("dial tcp: connection refused")) {
		t.Error("a transport error is not a schema rejection")
	}
}
