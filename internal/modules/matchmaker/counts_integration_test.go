package matchmaker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
)

// TestIntegrationCountsIncludeBasePlayed is the regression for the sixth game:
// loadGenBase used to count only stored match rows, ignoring base_played and
// manual adjustments, so a player on 5x looked like 4x and the draw scheduled
// them again. Read-only.
func TestIntegrationCountsIncludeBasePlayed(t *testing.T) {
	if os.Getenv("INTEGRATION_DB") != "1" {
		t.Skip("set INTEGRATION_DB=1 to run against the real database")
	}
	pool := testDBPool(t)
	s := &Service{db: pool, cfg: config.Config{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const eventID = "d5e11c16-9d05-4651-8a8a-7bf8b4f2418c"

	var base int
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(base_played, 0) FROM match_events WHERE id = $1`, eventID).Scan(&base); err != nil {
		t.Fatalf("load base_played: %v", err)
	}
	if base == 0 {
		t.Skip("event has no base_played to check")
	}

	gb, err := s.loadGenBase(ctx, eventID)
	if err != nil {
		t.Fatalf("loadGenBase: %v", err)
	}
	counts, err := s.loadCounts(ctx, eventID, gb.pool, gb.matches)
	if err != nil {
		t.Fatalf("loadCounts: %v", err)
	}
	display := map[string]int{}
	for _, c := range counts {
		display[c.PlayerID] = c.Played
	}

	mismatch := 0
	for _, p := range gb.aiPlayers {
		if gb.playedCounts[p.ID] != display[p.ID] {
			mismatch++
			t.Errorf("%s: draw pool says %dx, UI shows %dx", p.Name, gb.playedCounts[p.ID], display[p.ID])
		}
	}
	if mismatch == 0 {
		t.Logf("all %d players agree with the UI view (base_played=%d)", len(gb.aiPlayers), base)
	}
}
