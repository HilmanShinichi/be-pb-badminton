package matchmaker

import (
	"testing"
)

// mkPool builds a pool where the first n names get the given play counts.
func mkPool(names []string, counts []int) ([]poolPlayer, map[string]int) {
	pool := make([]poolPlayer, 0, len(names))
	played := map[string]int{}
	for i, n := range names {
		pool = append(pool, poolPlayer{ID: n, Name: n})
		played[n] = counts[i]
	}
	return pool, played
}

func ids(ps []poolPlayer) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// TestAllowancePickLowestLevelOnly is the case the user reported: four players
// on 3x while everyone else already has 5x. Only those four may play — the
// draw must not raise anyone's allowance just because others are ahead.
func TestAllowancePickLowestLevelOnly(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	counts := []int{3, 3, 3, 3, 5, 5, 5, 5}
	pool, played := mkPool(names, counts)

	picked, err := allowancePick(pool, played, nil, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected exactly one card (4 players), got %d: %v", len(got), got)
	}
	for _, id := range got {
		if played[id] != 3 {
			t.Errorf("player %s on %dx was drawn while 3x players were available", id, played[id])
		}
	}
}

// TestAllowancePickThreeUnderCap proves the short-fill rule: three players below
// the cap cannot make a 2v2, so exactly one capped player joins them.
func TestAllowancePickThreeUnderCap(t *testing.T) {
	names := []string{"a", "b", "c", "x", "e", "f"}
	counts := []int{3, 3, 3, 5, 5, 5}
	pool, played := mkPool(names, counts)

	picked, err := allowancePick(pool, played, nil, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected 4 players, got %d: %v", len(got), got)
	}
	under, capped := 0, 0
	for _, id := range got {
		if played[id] < 5 {
			under++
		} else {
			capped++
		}
	}
	if under != 3 || capped != 1 {
		t.Errorf("expected 3 under cap + 1 borrowed, got under=%d capped=%d (%v)", under, capped, got)
	}
}

// TestAllowancePickNoCapRaiseWhenLevelFills checks that a level with more than
// four players is trimmed to whole cards instead of pulling in the next level.
func TestAllowancePickNoCapRaiseWhenLevelFills(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	counts := []int{2, 2, 2, 2, 2, 2, 4, 4, 4, 4}
	pool, played := mkPool(names, counts)

	picked, err := allowancePick(pool, played, nil, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected one card, got %d: %v", len(got), got)
	}
	for _, id := range got {
		if played[id] != 2 {
			t.Errorf("player %s on %dx drawn while 2x players were available", id, played[id])
		}
	}
}

// TestAllowancePickAllUnderCapLevelsToCards checks that when everyone is under
// the cap the draw still respects count order and returns whole cards, trimming
// the spill-over from a level instead of leaving a partial card.
func TestAllowancePickAllUnderCapLevelsToCards(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	counts := []int{1, 1, 1, 2, 2, 2}
	pool, played := mkPool(names, counts)

	picked, err := allowancePick(pool, played, nil, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected a single whole card (4), got %d: %v", len(got), got)
	}
	// The three 1x players must all be in; the fourth slot comes from the 2x
	// level because nobody on 1x is left.
	for _, id := range []string{"a", "b", "c"} {
		found := false
		for _, g := range got {
			if g == id {
				found = true
			}
		}
		if !found {
			t.Errorf("1x player %s was left out while a 2x player took the slot: %v", id, got)
		}
	}
}

// TestAllowancePickTrimsExtraFromOneLevel covers the unavoidable split: five
// players on the same count cannot fill 1.25 cards, so one sits out and leads
// the next draw.
func TestAllowancePickTrimsExtraFromOneLevel(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	counts := []int{2, 2, 2, 2, 2}
	pool, played := mkPool(names, counts)

	picked, err := allowancePick(pool, played, nil, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected the draw trimmed to one whole card, got %d: %v", len(got), got)
	}
}

// TestAllowancePickDoesNotRaiseCap is the regression for the reported bug: a
// player already at the cap must never take a slot from someone below it just
// because the low group is not a multiple of four.
func TestAllowancePickDoesNotRaiseCap(t *testing.T) {
	// 6 on 3x + 2 on 5x. The 6 make one card plus 2 spare; no 5x player joins.
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	counts := []int{3, 3, 3, 3, 3, 3, 5, 5}
	pool, played := mkPool(names, counts)

	picked, err := allowancePick(pool, played, nil, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected one card, got %d: %v", len(got), got)
	}
	for _, id := range got {
		if played[id] >= 5 {
			t.Errorf("capped player %s was drawn while 3x players were still waiting: %v", id, got)
		}
	}
}

// TestAllowancePickPerPlayerCaps checks that an explicit personal cap below the
// event limit still marks that player as capped.
func TestAllowancePickPerPlayerCaps(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	counts := []int{3, 3, 3, 3, 2, 2}
	pool, played := mkPool(names, counts)
	caps := map[string]int{"e": 2, "f": 2}

	picked, err := allowancePick(pool, played, caps, 5)
	if err != nil {
		t.Fatalf("allowancePick: %v", err)
	}
	got := ids(picked)
	if len(got) != 4 {
		t.Fatalf("expected one card, got %d: %v", len(got), got)
	}
	for _, id := range got {
		if caps[id] > 0 && played[id] >= caps[id] {
			t.Errorf("player %s at their personal cap was drawn: %v", id, got)
		}
	}
}

// TestAllowancePickEveryoneCapped proves the draw refuses instead of silently
// raising the limit when nobody has games left.
func TestAllowancePickEveryoneCapped(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	counts := []int{5, 5, 5, 5}
	pool, played := mkPool(names, counts)

	if _, err := allowancePick(pool, played, nil, 5); err == nil {
		t.Fatal("expected an error when every player is capped")
	}
}
