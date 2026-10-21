package matchmaker

import (
	"testing"
	"time"
)

// tp builds a team from player ids; ids double as display names in failures.
func tp(ids ...string) []TeamPlayer {
	t := make([]TeamPlayer, 0, len(ids))
	for _, id := range ids {
		t = append(t, TeamPlayer{PlayerID: id, Name: id})
	}
	return t
}

func wantSet(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	wantM := map[string]bool{}
	for _, w := range want {
		wantM[w] = true
	}
	if len(got) != len(wantM) {
		t.Fatalf("set size = %d (%v), want %d (%v)", len(got), keys(got), len(wantM), want)
	}
	for id := range wantM {
		if !got[id] {
			t.Fatalf("missing %q in %v", id, keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLiveSetForScansEveryRound: a PLAYING card on an old round still benches
// its players. Round numbers cannot gate this, because a manual card added
// late lands in an older round while its match is on.
func TestLiveSetForScansEveryRound(t *testing.T) {
	matches := []GenMatch{
		{Round: 4, Wave: 1, Status: "ENDED", CreatedAt: "2026-10-18T06:00:00Z", Team1: tp("a", "b"), Team2: tp("c", "d")},
		{Round: 1, Wave: 4, Status: "PLAYING", CreatedAt: "2026-10-18T07:29:40Z", Team1: tp("solihin", "agus"), Team2: tp("ilham", "rama")},
		{Round: 4, Wave: 1, Status: "UPCOMING", CreatedAt: "2026-10-18T08:00:00Z", Team1: tp("e", "f"), Team2: tp("g", "h")},
	}
	live := liveSetForAt(matches, mustParse(t, "2026-10-18T08:00:00Z"))
	wantSet(t, live, "solihin", "agus", "ilham", "rama")
}

// TestSetsEmpty: no cards and round 1 produce an empty set instead of panicking.
func TestSetsEmpty(t *testing.T) {
	if got := liveSetForAt(nil, time.Now()); len(got) != 0 {
		t.Fatalf("live = %v, want empty", got)
	}
	if got := restSetForAt(nil, nil, 1, 4, time.Now()); len(got) != 0 {
		t.Fatalf("rest = %v, want empty", got)
	}
	if got := restSetForAt(nil, nil, 4, 4, time.Now()); len(got) != 0 {
		t.Fatalf("rest with no cards = %v, want empty", got)
	}
}

// TestLiveSetDropsAbandonedCard: a PLAYING flag older than the window is a
// forgotten tap, not a running game, so it must not bench anyone forever.
func TestLiveSetDropsAbandonedCard(t *testing.T) {
	matches := []GenMatch{
		{Round: 1, Wave: 4, Status: "PLAYING", CreatedAt: "2026-10-18T07:29:40Z", Team1: tp("solihin", "agus"), Team2: tp("ilham", "rama")},
	}
	now := mustParse(t, "2026-10-21T09:33:00Z")
	if got := liveSetForAt(matches, now); len(got) != 0 {
		t.Fatalf("live = %v, want empty (card is 3 days old)", got)
	}
	// Same card, hours after it started: still live.
	now = mustParse(t, "2026-10-18T08:00:00Z")
	wantSet(t, liveSetForAt(matches, now), "solihin", "agus", "ilham", "rama")
}

// TestRestSetIsLastWave: one wave is courtCount cards, picked by draw order.
func TestRestSetIsLastWave(t *testing.T) {
	matches := []GenMatch{
		{Round: 1, Wave: 3, Status: "ENDED", CreatedAt: "2026-10-18T07:00:00Z", Team1: tp("a", "b"), Team2: tp("c", "d")},
		{Round: 1, Wave: 4, Status: "ENDED", CreatedAt: "2026-10-18T07:10:00Z", Team1: tp("e", "f"), Team2: tp("g", "h")},
		{Round: 2, Wave: 1, Status: "ENDED", CreatedAt: "2026-10-18T07:20:00Z", Team1: tp("i", "j"), Team2: tp("k", "l")},
		{Round: 2, Wave: 2, Status: "ENDED", CreatedAt: "2026-10-18T07:30:00Z", Team1: tp("m", "n"), Team2: tp("o", "p")},
	}
	now := mustParse(t, "2026-10-18T07:40:00Z")
	// courtCount 2 -> the two newest cards only.
	wantSet(t, restSetForAt(matches, nil, 4, 2, now), "i", "j", "k", "l", "m", "n", "o", "p")
	// courtCount 4 -> all four cards.
	wantSet(t, restSetForAt(matches, nil, 4, 4, now),
		"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p")
}

// TestRestSetStaleWaveIsIdle: when every card is older than the window the
// evening ended long ago, so nobody is owed a break.
func TestRestSetStaleWaveIsIdle(t *testing.T) {
	matches := []GenMatch{
		{Round: 1, Wave: 4, Status: "ENDED", CreatedAt: "2026-10-18T07:30:00Z", Team1: tp("a", "b"), Team2: tp("c", "d")},
	}
	now := mustParse(t, "2026-10-21T09:33:00Z")
	if got := restSetForAt(matches, nil, 4, 4, now); len(got) != 0 {
		t.Fatalf("rest = %v, want empty (wave is 3 days old)", got)
	}
	// Within the window the same wave still rests.
	now = mustParse(t, "2026-10-18T08:00:00Z")
	wantSet(t, restSetForAt(matches, nil, 4, 4, now), "a", "b", "c", "d")
}

// TestRestSetFromPendings: rounds drawn earlier in the same call are the
// newest wave, so their players rest instead of playing back to back.
func TestRestSetFromPendings(t *testing.T) {
	pend := []pending{
		{round: 1, wave: 1, team1: tp("a", "b"), team2: tp("c", "d")},
		{round: 1, wave: 2, team1: tp("e", "f"), team2: tp("g", "h")},
	}
	got := restSetForAt(nil, pend, 2, 4, time.Now())
	wantSet(t, got, "e", "f", "g", "h")
}

// TestRestSetIgnoresTargetRound: cards already in the round being generated
// are not history, they are the round itself.
func TestRestSetIgnoresTargetRound(t *testing.T) {
	matches := []GenMatch{
		{Round: 4, Wave: 1, Status: "UPCOMING", CreatedAt: "2026-10-18T07:30:00Z", Team1: tp("a", "b"), Team2: tp("c", "d")},
		{Round: 3, Wave: 2, Status: "ENDED", CreatedAt: "2026-10-18T07:20:00Z", Team1: tp("e", "f"), Team2: tp("g", "h")},
	}
	now := mustParse(t, "2026-10-18T07:40:00Z")
	wantSet(t, restSetForAt(matches, nil, 4, 4, now), "e", "f", "g", "h")
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}
