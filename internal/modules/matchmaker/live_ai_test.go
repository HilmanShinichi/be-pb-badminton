package matchmaker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
)

func sp(s string) *string { return &s }

// TestLiveAIFallbackDraw calls the real configured provider with the real
// prompt path and checks a usable draw comes back. It proves the json_schema
// fallback works end to end, not just that the error classifier says so.
// Requires AI_* in .env; set INTEGRATION_AI=1 to run.
func TestLiveAIFallbackDraw(t *testing.T) {
	if os.Getenv("INTEGRATION_AI") != "1" {
		t.Skip("set INTEGRATION_AI=1 to call the real provider")
	}
	// Tests run inside the package dir, so point LoadDotEnv at the repo root.
	config.LoadDotEnv("../../../.env")
	cfg := config.Load()
	if cfg.AIBaseURL == "" || cfg.AIAPIKey == "" {
		t.Skip("no AI credentials configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// A tiny but real draw request, same code path as production.
	pool := []aiPlayer{
		{Index: 0, Name: "Hari", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 1, Played: 2},
		{Index: 1, Name: "Ivan", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 2, Played: 3},
		{Index: 2, Name: "Eres", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 3, Played: 3},
		{Index: 3, Name: "Hidayat", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 4, Played: 3},
		{Index: 4, Name: "Feri", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 5, Played: 3},
		{Index: 5, Name: "Ferry", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 6, Played: 4},
		{Index: 6, Name: "Riki", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 7, Played: 4},
		{Index: 7, Name: "Imam Ali", Grade: sp("B1"), Gender: sp("L"), Rank: 1, Arrival: 8, Played: 4},
	}
	user := buildUserPrompt(pool, nil, nil, 1, 1, nil, "", nil)
	t.Logf("prompt is %d bytes", len(user))

	raw, err := callOpenAI(ctx, cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel, user)
	if err != nil {
		t.Fatalf("AI call failed: %v", err)
	}
	out, err := parseAIOutput(raw)
	if err != nil {
		t.Fatalf("returned answer is not parseable JSON: %v\nraw: %.300s", err, raw)
	}
	cards := 0
	for _, r := range out.Rounds {
		cards += len(r.Matches)
	}
	if cards == 0 {
		t.Fatalf("no card in the answer\nraw: %.300s", raw)
	}
	t.Logf("got %d card(s) after the fallback chain", cards)
	for _, r := range out.Rounds {
		for _, m := range r.Matches {
			t.Logf("   %v vs %v", m.Team1, m.Team2)
		}
	}
}
