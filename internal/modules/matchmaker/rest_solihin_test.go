package matchmaker

import (
	"testing"
)

// gm builds a generated card whose draw time and start time are the same.
func gm(wave int, status, at string, t1, t2 []TeamPlayer) GenMatch {
	return gmS(wave, status, at, at, t1, t2)
}

// gmS builds a card with separate draw and start times. Postgres hands the
// service timestamps as text, so the tests use the same shape.
func gmS(wave int, status, created, started string, t1, t2 []TeamPlayer) GenMatch {
	m := GenMatch{Wave: wave, Status: status, CreatedAt: created, Team1: t1, Team2: t2}
	if started != "" {
		s := started
		m.StartedAt = &s
	}
	return m
}

// TestRestSetForSolihinCase replays the 18 Oct season 4 event (29 players,
// 4 courts) as it looked when the next batch was generated at 07:40.
//
// Five waves ran before the draw. Wave 5's last card was drawn 07:36:19 and
// the two cards before it were still on court, so the newest wave at draw
// time was wave 4-5, not the leftover single-card waves drawn earlier.
// Solihin was on court in wave 4 and must not be drawn again, either through
// the rest set or the live set — the caller merges both.
func TestRestSetForSolihinCase(t *testing.T) {
	matches := []GenMatch{
		// Waves 1-3: finished long before the draw.
		gm(1, "ENDED", "2026-10-18T05:44:15Z", tp("a1", "a2"), tp("a3", "a4")),
		gm(2, "ENDED", "2026-10-18T05:51:22Z", tp("d1", "d2"), tp("d3", "d4")),
		gm(3, "ENDED", "2026-10-18T05:53:51Z", tp("g1", "g2"), tp("g3", "g4")),
		// Wave 4: Solihin's game, still on court at draw time.
		gm(4, "PLAYING", "2026-10-18T07:29:40Z", tp("solihin", "agus"), tp("ilham", "rama")),
		gm(4, "PLAYING", "2026-10-18T07:30:04Z", tp("fadil", "hidayat"), tp("djovan", "ikhsan")),
		// Wave 5: two on court, one card already finished.
		gm(5, "PLAYING", "2026-10-18T07:30:39Z", tp("icha", "afan"), tp("ari", "syifa")),
		gm(5, "PLAYING", "2026-10-18T07:31:13Z", tp("p_tri", "ferry"), tp("aby", "kris")),
		gm(5, "ENDED", "2026-10-18T07:36:19Z", tp("amir", "hilman"), tp("jio", "imam")),
		// Leftover single-card waves, drawn earlier and never started.
		gm(1, "ENDED", "2026-10-18T05:58:03Z", tp("s1", "s2"), tp("s3", "s4")),
		gmS(2, "ENDED", "2026-10-18T06:35:42Z", "", tp("t1", "t2"), tp("t3", "t4")),
	}
	now := mustParse(t, "2026-10-18T07:40:00Z")

	// Four courts: the newest four cards, ordered by the moment they started.
	rest := restSetForAt(matches, nil, 4, now)
	wantSet(t, rest,
		"amir", "hilman", "jio", "imam",
		"p_tri", "ferry", "aby", "kris",
		"icha", "afan", "ari", "syifa",
		"fadil", "hidayat", "djovan", "ikhsan")

	// Solihin's card is the fifth newest, so the wave set alone misses him;
	// the live set is what catches him. Together they cover the batch.
	live := liveSetForAt(matches, now)
	if !live["solihin"] {
		t.Fatal("solihin is on court but missing from the live set")
	}
	merged := map[string]bool{}
	for id := range rest {
		merged[id] = true
	}
	for id := range live {
		merged[id] = true
	}
	for _, id := range []string{"solihin", "agus", "ilham", "rama"} {
		if !merged[id] {
			t.Fatalf("%q would be drawn again while their game runs", id)
		}
	}

	// Nobody from the leftover waves rests: those cards are old.
	for _, id := range []string{"s1", "s2", "s3", "s4", "t1", "t2", "t3", "t4"} {
		if rest[id] {
			t.Fatalf("%q rests, but their card is not in the newest wave", id)
		}
	}
}

// TestRestSetForSolihinLive: the live set alone must cover a player whose
// game is genuinely running, without help from the wave ordering.
func TestRestSetForSolihinLive(t *testing.T) {
	matches := []GenMatch{
		gm(4, "PLAYING", "2026-10-18T07:29:40Z", tp("solihin", "agus"), tp("ilham", "rama")),
		gm(5, "PLAYING", "2026-10-18T07:30:39Z", tp("icha", "afan"), tp("ari", "syifa")),
	}
	now := mustParse(t, "2026-10-18T07:40:00Z")
	wantSet(t, liveSetForAt(matches, now),
		"solihin", "agus", "ilham", "rama", "icha", "afan", "ari", "syifa")
}
