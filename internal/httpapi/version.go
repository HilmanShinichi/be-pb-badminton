package httpapi

import (
	"os"
	"runtime/debug"
	"time"

	"github.com/gofiber/fiber/v2"
)

// BuildTag names the fix this binary carries. Bump it in the same commit as
// the fix, then curl /version against the deployed host: a changed tag proves
// the new code is live, an unchanged one proves the deploy did not land.
//
// Keep it short, no spaces — it travels in logs and chat messages.
var BuildTag = "json-schema-fallback"

// startedAt marks process start. A fresh value proves the process restarted,
// which separates "new code deployed" from "same process still answering".
var startedAt = time.Now()

// buildVersion reports what this process is running. The commit comes from the
// VCS stamp the Go toolchain embeds at build time; it is absent when the build
// had no repository to read, hence the "unknown" fallback.
func buildVersion() fiber.Map {
	tag := BuildTag
	if v := os.Getenv("BUILD_TAG"); v != "" {
		tag = v
	}
	out := fiber.Map{
		"tag":        tag,
		"commit":     "unknown",
		"built":      "unknown",
		"started_at": startedAt.UTC().Format(time.RFC3339),
		"uptime":     time.Since(startedAt).Round(time.Second).String(),
	}
	// Render exposes the deployed commit as RENDER_GIT_COMMIT. Without this the
	// tag alone would be the only signal, and a forgotten bump would look like
	// a failed deploy forever.
	if v := os.Getenv("RENDER_GIT_COMMIT"); v != "" {
		out["commit"] = v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if out["commit"] == "unknown" {
				out["commit"] = s.Value
			}
		case "vcs.time":
			out["built"] = s.Value
		}
	}
	return out
}

// Version is the deploy check: no auth, no database, nothing that can fail.
// It answers "is the code I just pushed the code that is serving requests?"
func Version(c *fiber.Ctx) error {
	return c.JSON(buildVersion())
}
