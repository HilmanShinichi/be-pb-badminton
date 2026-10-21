package matchmaker

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/config"
)

// defaultEnvPath is where the backend keeps its database settings. The test
// reads it from disk rather than taking a DSN as a flag, so no credential
// ever travels through a command line. ENVPATH overrides the location.
const defaultEnvPath = "/home/hilmans/pb-kecebong/be-pb-badminton/.env"

// testDBPool builds a pool from that .env. It skips instead of failing when
// the file or the database is missing, so a normal `go test ./...` stays
// green on a machine without access to the event database.
func testDBPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	path := os.Getenv("ENVPATH")
	if path == "" {
		path = defaultEnvPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no .env at %s: %v", path, err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	dsn := "postgres://" + kv["DB_USER"] + ":" + kv["DB_PASSWORD"] +
		"@" + kv["DB_HOST"] + ":" + kv["DB_PORT"] + "/" + kv["DB_NAME"] +
		"?sslmode=" + kv["DB_SSLMODE"]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestIntegrationRestSetRealEvent loads the real event behind a live session
// and reports what the code would bench. Read-only: loadGenBase runs SELECTs
// only. Skipped unless INTEGRATION_DB=1 is set, and skipped again when the
// database is not reachable.
func TestIntegrationRestSetRealEvent(t *testing.T) {
	if os.Getenv("INTEGRATION_DB") != "1" {
		t.Skip("set INTEGRATION_DB=1 to run against the real database")
	}

	pool := testDBPool(t)
	s := &Service{db: pool, cfg: config.Config{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const eventID = "d5e11c16-9d05-4651-8a8a-7bf8b4f2418c"
	gb, err := s.loadGenBase(ctx, eventID)
	if err != nil {
		t.Fatalf("loadGenBase: %v", err)
	}

	nameOf := map[string]string{}
	for _, p := range gb.pool {
		nameOf[p.ID] = p.Name
	}
	names := func(set map[string]bool) string {
		out := []string{}
		for id := range set {
			if n, ok := nameOf[id]; ok {
				out = append(out, n)
			} else {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return "[" + strings.Join(out, " ") + "]"
	}

	live := liveSetFor(gb.matches)
	rest := restSetFor(gb.matches, nil, gb.maxRound+1, gb.wavesPerRound)
	for id := range live {
		rest[id] = true
	}

	statuses := map[string]int{}
	for _, m := range gb.matches {
		statuses[m.Status]++
	}
	t.Logf("matches=%d status=%v maxRound=%d wavesPerRound=%d basePlayed=%d",
		len(gb.matches), statuses, gb.maxRound, gb.wavesPerRound, gb.basePlayed)
	t.Logf("live: %s", names(live))
	t.Logf("rest: %s", names(rest))

	// The four cards left on PLAYING from the 18 Oct evening are closed now,
	// so nothing may be held live. If a stale flag creeps back in, this
	// catches it against the real rows.
	if len(live) != 0 {
		t.Errorf("expected no live players once the stale cards are closed, got %s",
			names(live))
	}
	if len(rest) == 0 {
		t.Errorf("expected a rest set from the last wave")
	}
}
