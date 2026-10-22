package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestVersionTagFromEnv proves a deploy can be renamed without a rebuild.
func TestVersionTagFromEnv(t *testing.T) {
	t.Setenv("BUILD_TAG", "totally-new-fix")
	app := fiber.New()
	app.Get("/version", Version)

	res, err := app.Test(httptest.NewRequest("GET", "/version", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["tag"] != "totally-new-fix" {
		t.Errorf("tag = %v, want the env override", body["tag"])
	}
	for _, key := range []string{"commit", "built", "started_at", "uptime"} {
		if _, ok := body[key]; !ok {
			t.Errorf("response is missing %q", key)
		}
	}
}

// TestVersionDefaultsToBuildTag proves the compiled-in tag is what ships when
// no environment override exists, so bumping it in the commit is enough.
func TestVersionDefaultsToBuildTag(t *testing.T) {
	t.Setenv("BUILD_TAG", "")
	app := fiber.New()
	app.Get("/version", Version)

	res, err := app.Test(httptest.NewRequest("GET", "/version", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["tag"] != BuildTag {
		t.Errorf("tag = %v, want the compiled-in %q", body["tag"], BuildTag)
	}
}

// TestVersionNoDatabaseNeeded guards the point of the endpoint: it must answer
// even when nothing else works, because a failing deploy check is useless.
func TestVersionNoDatabaseNeeded(t *testing.T) {
	t.Setenv("BUILD_TAG", "")
	os.Unsetenv("RENDER_GIT_COMMIT")
	app := fiber.New()
	app.Get("/version", Version)

	res, err := app.Test(httptest.NewRequest("GET", "/version", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("version must not depend on the database, got %d", res.StatusCode)
	}
}
