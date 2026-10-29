package matchmaker

import (
	"context"
	"fmt"
	"math"
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
	rest := restSetFor(gb.matches, nil, gb.wavesPerRound)
	for id := range live {
		rest[id] = true
	}

	statuses := map[string]int{}
	for _, m := range gb.matches {
		statuses[m.Status]++
	}
	t.Logf("matches=%d status=%v maxWave=%d wavesPerRound=%d basePlayed=%d",
		len(gb.matches), statuses, gb.maxWave, gb.wavesPerRound, gb.basePlayed)
	t.Logf("live: %s", names(live))
	t.Logf("rest: %s", names(rest))

	// Live is time-relative, not empty-by-definition: a card started moments ago
	// must bench its players, while a forgotten PLAYING flag from days ago must
	// not. Assert the semantics rather than a fixed count, so the test survives
	// a night where somebody is actually on court.
	stale := 0
	fresh := 0
	cutoff := time.Now().Add(-liveMatchWindow)
	for _, m := range gb.matches {
		if m.Status != "PLAYING" {
			continue
		}
		if at := liveAt(m); at.IsZero() || at.Before(cutoff) {
			stale++
			continue
		}
		fresh++
	}
	t.Logf("playing cards: fresh=%d stale=%d", fresh, stale)
	if len(live) > 0 && fresh == 0 {
		t.Errorf("live set is non-empty but no PLAYING card started within the window")
	}
	if fresh > 0 && len(live) == 0 {
		t.Errorf("a PLAYING card started within the window but the live set is empty")
	}
	if len(rest) == 0 {
		t.Errorf("expected a rest set from the last wave")
	}
}

// TestIntegrationAllowanceModeRealEvent checks the per-player allowance draw on
// the real event: once a player is at their cap, the pool must be everyone
// still under it plus the fewest capped players needed to reach a 2v2.
// Read-only.
func TestIntegrationAllowanceModeRealEvent(t *testing.T) {
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

	// Precondition: this is the case the user hit — somebody is at their cap.
	if gb.maxRounds <= 0 || !gb.allowanceOpen(gb.playedCounts) {
		t.Skipf("no player is at their cap yet (maxRounds=%d)", gb.maxRounds)
	}

	sub, cards, err := s.allowancePlayers(ctx, eventID, gb)
	if err != nil {
		t.Fatalf("allowancePlayers: %v", err)
	}

	// The draw counts games including base_played and manual adjustments;
	// gb.playedCounts only holds the raw match rows, so auditing fairness with
	// it would disagree with the code under test.
	cnts, err := s.loadCounts(ctx, eventID, gb.pool, gb.matches)
	if err != nil {
		t.Fatalf("loadCounts: %v", err)
	}
	played := make(map[string]int, len(cnts))
	for _, c := range cnts {
		played[c.PlayerID] = c.Played
	}

	under, capped := 0, 0
	for _, p := range sub {
		lim := effCap(gb.caps, gb.maxRounds, p.ID)
		if lim > 0 && played[p.ID] >= lim {
			capped++
			continue
		}
		under++
	}

	t.Logf("maxWave=%d maxRounds=%d pool=%d allowancePool=%d under=%d borrowed=%d cards=%d",
		gb.maxWave, gb.maxRounds, len(gb.pool), len(sub), under, capped, cards)

	if len(sub) == 0 {
		t.Fatalf("allowance pool is empty")
	}
	if len(sub)%4 != 0 {
		t.Errorf("allowance pool is not a full 2v2 set: %d", len(sub))
	}
	if cards != len(sub)/4 {
		t.Errorf("card count %d does not match pool size %d", cards, len(sub))
	}
	// Borrowing must be minimal: only to round the remainder up to a 2v2.
	if capped >= 4 {
		t.Errorf("borrowed %d capped players, expected fewer than 4", capped)
	}
	if capped > 0 && under%4 == 0 {
		t.Errorf("borrowed %d capped players although the group was already complete", capped)
	}

	// Fairness: the card must be built from the lowest play counts first. If a
	// capped player was borrowed, everyone picked below the cap must come from
	// the bottom of the count table — no player may be left behind on fewer
	// games than somebody who was drawn.
	pickedMin, pickedMax := math.MaxInt32, -1
	for _, p := range sub {
		n := played[p.ID]
		if n < pickedMin {
			pickedMin = n
		}
		if n > pickedMax {
			pickedMax = n
		}
	}
	lowest := math.MaxInt32
	for _, p := range gb.pool {
		if n := played[p.ID]; n < lowest {
			lowest = n
		}
	}
	t.Logf("picked counts: min=%d max=%d; lowest in pool=%d", pickedMin, pickedMax, lowest)

	// Name the picks so a human can eyeball the fairness on the real event.
	pickedSet := make(map[string]bool, len(sub))
	picks := make([]string, 0, len(sub))
	for _, p := range sub {
		pickedSet[p.ID] = true
		picks = append(picks, fmt.Sprintf("%s(%dx)", p.Name, played[p.ID]))
	}
	sort.Strings(picks)
	t.Logf("picked: %s", strings.Join(picks, " "))

	// Everyone left out must have played at least as often as the players who
	// went on court; otherwise the draw skipped somebody who was behind.
	leftOut := make([]string, 0, 4)
	for _, p := range gb.pool {
		if pickedSet[p.ID] {
			continue
		}
		if n := played[p.ID]; n < pickedMax {
			leftOut = append(leftOut, fmt.Sprintf("%s(%dx)", p.Name, n))
		}
	}
	if len(leftOut) > 0 {
		t.Errorf("left out while fewer games than a picked player: %s", strings.Join(leftOut, " "))
	}

	if pickedMin != lowest {
		t.Errorf("draw skipped players on %dx: lowest picked is %d", lowest, pickedMin)
	}
	// Levels are taken whole, so at most one count value may appear below the
	// cap plus the borrowed tail: anything wider means a level was skipped.
	underCounts := map[int]bool{}
	for _, p := range sub {
		n := played[p.ID]
		if lim := effCap(gb.caps, gb.maxRounds, p.ID); lim <= 0 || n < lim {
			underCounts[n] = true
		}
	}
	if len(underCounts) > 2 {
		t.Errorf("under-cap picks span %d count levels (%v); should be one level plus at most a trailing one", len(underCounts), underCounts)
	}
}

// TestIntegrationStrategyRestRealEvent checks resting mode on the real event:
// the draw pool must contain nobody who is still on court or played the last
// wave, and it must be strictly smaller than the leveling pool (which keeps
// them and only benches them first). Read-only.
func TestIntegrationStrategyRestRealEvent(t *testing.T) {
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

	rest := restSetFor(gb.matches, nil, gb.wavesPerRound)
	live := liveSetFor(gb.matches)
	bench := map[string]bool{}
	for id := range rest {
		bench[id] = true
	}
	for id := range live {
		bench[id] = true
	}

	leveling := gb.aiPlayers
	resting := dropBench(gb.aiPlayers, bench, strategyRest)

	t.Logf("pool=%d bench=%d (rest=%d live=%d) leveling=%d resting=%d",
		len(gb.aiPlayers), len(bench), len(rest), len(live), len(leveling), len(resting))

	if len(bench) == 0 {
		t.Skip("no live or last-wave players right now — nothing to bench")
	}
	for _, p := range resting {
		if bench[p.ID] {
			t.Errorf("%s is on the bench (live or last wave) but entered the resting draw", p.Name)
		}
	}
	// Leveling must keep them, otherwise the two modes would be identical and
	// the toggle would be pointless.
	if len(leveling) != len(gb.aiPlayers) {
		t.Errorf("leveling dropped players: %d of %d", len(leveling), len(gb.aiPlayers))
	}
	if len(resting) >= len(leveling) {
		t.Errorf("resting pool (%d) should be smaller than leveling (%d)", len(resting), len(leveling))
	}
}

// TestIntegrationSoftScopeRealPeriod checks the cross-week no-repeat guard on
// the real period: every matchup of the three most recent earlier events enters
// softSeen, while the prompt list stays capped. Read-only.
func TestIntegrationSoftScopeRealPeriod(t *testing.T) {
	if os.Getenv("INTEGRATION_DB") != "1" {
		t.Skip("set INTEGRATION_DB=1 to run against the real database")
	}

	pool := testDBPool(t)
	s := &Service{db: pool, cfg: config.Config{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const eventID = "d5e11c16-9d05-4651-8a8a-7bf8b4f2418c"
	periodID, ok := s.eventPeriodID(ctx, eventID)
	if !ok {
		t.Fatalf("event %s has no period link", eventID)
	}

	softSeen, softHistory := s.periodRecentSoft(ctx, periodID, eventID)

	// Count what a narrower guard would have produced: only the final wave
	// of each event (the round column is gone, so the wave is the closest
	// leftover grouping).
	var oldStyle int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM generated_matches m
		JOIN match_events e ON e.id = m.event_id
		JOIN mabar_sessions ms ON ms.id = e.source_session_id
		WHERE ms.period_id = $1
		  AND m.event_id <> $2
		  AND m.wave = (SELECT MAX(m2.wave) FROM generated_matches m2 WHERE m2.event_id = m.event_id)`,
		periodID, eventID).Scan(&oldStyle); err != nil {
		t.Fatalf("count old scope: %v", err)
	}

	var total int
	if err := pool.QueryRow(ctx, `
		WITH recent AS (
			SELECT e.id
			FROM match_events e
			JOIN mabar_sessions ms ON ms.id = e.source_session_id
			WHERE ms.period_id = $1 AND e.id <> $2
			ORDER BY e.created_at DESC
			LIMIT $3
		)
		SELECT count(*) FROM generated_matches m JOIN recent r ON r.id = m.event_id`,
		periodID, eventID, periodRecentSoftEvents).Scan(&total); err != nil {
		t.Fatalf("count new scope: %v", err)
	}

	t.Logf("period=%s softKeys=%d promptRows=%d oldScope=%d newScopeRows=%d",
		periodID, len(softSeen), len(softHistory), oldStyle, total)

	if len(softSeen) == 0 {
		t.Errorf("expected a non-empty soft set")
	}
	// The new scope must cover strictly more cards than the old last-wave view.
	if len(softSeen) <= oldStyle {
		t.Errorf("soft scope did not widen: %d keys vs old %d", len(softSeen), oldStyle)
	}
	if len(softHistory) > softHistoryPromptLimit {
		t.Errorf("prompt history exceeds cap: %d > %d", len(softHistory), softHistoryPromptLimit)
	}
	// The cap trims the prompt list only; the validator keeps every key.
	if total > softHistoryPromptLimit && len(softHistory) != softHistoryPromptLimit {
		t.Errorf("expected the prompt list capped at %d, got %d", softHistoryPromptLimit, len(softHistory))
	}
}
