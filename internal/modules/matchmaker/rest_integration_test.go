package matchmaker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/config"
)

// TestIntegrationRestSetRealEvent loads the live 18 Oct season 4 event from
// the real database and checks that the fix behaves on production data: the
// players still running a game are live, and the players who finished the
// last wave are the rest set. Read-only — loadGenBase only issues SELECTs.
//
// Run with:
//
//	INTEGRATION_DB=1 go test -run TestIntegrationRestSetRealEvent ./internal/modules/matchmaker/
func TestIntegrationRestSetRealEvent(t *testing.T) {
	if os.Getenv("INTEGRATION_DB") == "" {
		t.Skip("set INTEGRATION_DB=1 to run against the real database")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	svc := NewService(pool, config.Config{})
	const eventID = "d5e11c16-9d05-4651-8a8a-7bf8b4f2418c"
	gb, err := svc.loadGenBase(ctx, eventID)
	if err != nil {
		t.Fatalf("loadGenBase: %v", err)
	}
	if len(gb.matches) == 0 {
		t.Fatal("event has no generated matches")
	}
	t.Logf("event %s: %d matches, maxRound=%d, wavesPerRound=%d, players=%d",
		eventID, len(gb.matches), gb.maxRound, gb.wavesPerRound, len(gb.pool))

	nameOf := map[string]string{}
	for _, p := range gb.pool {
		nameOf[p.ID] = p.Name
	}

	live := liveSetFor(gb.matches)
	rest := restSetFor(gb.matches, nil, gb.maxRound+1, gb.wavesPerRound)
	for id := range live {
		rest[id] = true
	}
	t.Logf("live: %s", names(live, nameOf))
	t.Logf("rest: %s", names(rest, nameOf))

	// The 18 Oct evening is over, so both sets may legitimately be empty:
	// abandoned PLAYING flags and a last wave older than the window no
	// longer hold anyone back. What must never happen is a live player
	// escaping the rest set while their game is genuinely running.
	for id := range live {
		if !rest[id] {
			t.Fatalf("live player %q missing from rest set", nameOr(nameOf, id))
		}
	}
}

// names renders ids as display names when known, so failures read as
// "Solihin" instead of a UUID.
func names(set map[string]bool, nameOf map[string]string) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, nameOr(nameOf, id))
	}
	return out
}

func nameOr(nameOf map[string]string, id string) string {
	if n, ok := nameOf[id]; ok && n != "" {
		return n
	}
	return id
}
