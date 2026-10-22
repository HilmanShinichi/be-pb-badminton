package matchmaker

import (
	"testing"
	"time"
)

// TestSchemaBlockMemo proves a provider that refused json_schema is remembered
// briefly, so later draws skip the doomed request instead of paying for it
// every time.
func TestSchemaBlockMemo(t *testing.T) {
	base, model := "https://example.test/v1", "combo-x"
	aiSchemaMu.Lock()
	delete(aiSchemaRejected, aiSchemaKey(base, model))
	aiSchemaMu.Unlock()

	if aiSchemaBlocked(base, model) {
		t.Fatal("a provider must not start out blocked")
	}
	aiMarkSchemaBlocked(base, model)
	if !aiSchemaBlocked(base, model) {
		t.Fatal("after a refusal the mode must be skipped")
	}

	// Other providers and models keep the strict mode.
	if aiSchemaBlocked(base, "other-model") {
		t.Error("the memo is per model, not per host")
	}
	if aiSchemaBlocked("https://other.test/v1", model) {
		t.Error("the memo is per host, not global")
	}

	// A trailing slash in the base URL must not create a second entry.
	aiMarkSchemaBlocked(base+"/", model)
	aiSchemaMu.Lock()
	n := len(aiSchemaRejected)
	aiSchemaMu.Unlock()
	if n != 1 {
		t.Errorf("base URL variants must share one entry, got %d", n)
	}
}

// TestSchemaBlockExpires proves the memo is not permanent: a gateway that
// gains structured-output support is retried after the TTL.
func TestSchemaBlockExpires(t *testing.T) {
	base, model := "https://example.test/v1", "combo-y"
	aiSchemaMu.Lock()
	aiSchemaRejected[aiSchemaKey(base, model)] = time.Now().Add(-aiSchemaRejectTTL - time.Minute)
	aiSchemaMu.Unlock()

	if aiSchemaBlocked(base, model) {
		t.Error("an expired refusal must be retried")
	}
	aiSchemaMu.Lock()
	_, still := aiSchemaRejected[aiSchemaKey(base, model)]
	aiSchemaMu.Unlock()
	if still {
		t.Error("the expired entry should have been dropped")
	}
}
