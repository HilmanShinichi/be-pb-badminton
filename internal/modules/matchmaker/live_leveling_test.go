package matchmaker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
)

// TestLiveLevelingDraw reproduces the failing path exactly: the real event, the
// real leveling pool after enforceCaps, the real prompt, the real endpoints.
// It prints what each endpoint actually answered so a non-JSON reply can be
// diagnosed instead of guessed at. Set INTEGRATION_AI=1 to run.
func TestLiveLevelingDraw(t *testing.T) {
	if os.Getenv("INTEGRATION_AI") != "1" {
		t.Skip("set INTEGRATION_AI=1 to call the real provider")
	}
	config.LoadDotEnv("../../../.env")
	cfg := config.Load()
	if cfg.AIBaseURL == "" || cfg.AIAPIKey == "" {
		t.Skip("no AI credentials configured")
	}

	pool := testDBPool(t)
	s := &Service{db: pool, cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	const eventID = "d5e11c16-9d05-4651-8a8a-7bf8b4f2418c"
	gb, err := s.loadGenBase(ctx, eventID)
	if err != nil {
		t.Fatalf("loadGenBase: %v", err)
	}

	// Same filter Generate applies for leveling.
	players := enforceCaps(gb.aiPlayers, gb.playedCounts, gb.caps, gb.maxRounds)
	for i := range players {
		players[i].Played = gb.playedCounts[players[i].ID]
	}
	applyCaps(players, gb.caps, gb.maxRounds)
	t.Logf("leveling pool=%d (from %d), maxRounds=%d maxWave=%d",
		len(players), len(gb.aiPlayers), gb.maxRounds, gb.maxWave)

	user := buildUserPrompt(players, gb.history, gb.softHistory, 1, 0, nil, "", gb.pool)
	t.Logf("prompt bytes=%d", len(user))
	if err := os.WriteFile("/tmp/leveling_prompt.txt", []byte(user), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	for _, ep := range aiEndpoints(cfg) {
		t.Logf("--- %s model=%s base=%s", ep.Label, ep.Model, ep.Base)
		raw, err := callOpenAI(ctx, ep.Base, ep.Key, ep.Model, user)
		if err != nil {
			t.Logf("    CALL FAILED: %v", err)
			continue
		}
		t.Logf("    answered %d bytes", len(raw))
		snip := raw
		if len(snip) > 900 {
			snip = snip[:900]
		}
		t.Logf("    raw: %q", snip)
		out, perr := parseAIOutput(raw)
		if perr != nil {
			t.Logf("    PARSE ERROR: %v", perr)
		} else {
			n := 0
			for _, r := range out.Rounds {
				n += len(r.Matches)
			}
			t.Logf("    parsed rounds=%d matches=%d", len(out.Rounds), n)
		}
	}
}
