package matchmaker

import (
	"testing"
)

// TestEnforceCapsDropsAtCapPlayers is the regression for the round 5 draw the
// user got: four players already on 5x were scheduled while Hari (2x) and Ivan
// (3x) sat out. A capped player must never reach the model while enough
// under-cap players exist to fill a card.
func TestEnforceCapsDropsAtCapPlayers(t *testing.T) {
	players := []aiPlayer{
		{ID: "hari", Name: "Hari"},
		{ID: "ivan", Name: "Ivan"},
		{ID: "feri", Name: "Feri"},
		{ID: "ferry", Name: "Ferry"},
		{ID: "riki", Name: "Riki"},
		{ID: "imam", Name: "Imam"},
		{ID: "agus", Name: "Agus"},
		{ID: "hilman", Name: "Hilman"},
		{ID: "ikhsan", Name: "Ikhsan"},
		{ID: "solihin", Name: "Solihin"},
	}
	played := map[string]int{
		"hari": 2, "ivan": 3, "feri": 3, "ferry": 4, "riki": 4,
		"imam": 4, "agus": 5, "hilman": 5, "ikhsan": 5, "solihin": 5,
	}

	got := enforceCaps(players, played, nil, 5)
	kept := map[string]bool{}
	for _, p := range got {
		kept[p.ID] = true
		if played[p.ID] >= 5 {
			t.Errorf("capped player %s (5x) was kept in the draw pool", p.Name)
		}
	}
	for _, want := range []string{"hari", "ivan", "feri", "ferry", "riki", "imam"} {
		if !kept[want] {
			t.Errorf("under-cap player %s was dropped", want)
		}
	}
}

// TestEnforceCapsBorrowsWhenShort proves the hard cap still yields a legal
// card when fewer than four players have games left: the pool is returned
// untouched so the caller can borrow.
func TestEnforceCapsBorrowsWhenShort(t *testing.T) {
	players := []aiPlayer{
		{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"},
		{ID: "d", Name: "D"}, {ID: "e", Name: "E"}, {ID: "f", Name: "F"},
	}
	played := map[string]int{"a": 2, "b": 3, "c": 5, "d": 5, "e": 5, "f": 5}

	got := enforceCaps(players, played, nil, 5)
	if len(got) != len(players) {
		t.Fatalf("expected the pool untouched when only %d players are under cap, got %d",
			2, len(got))
	}
}

// TestEnforceCapsPersonalCap checks an explicit personal cap below the event
// limit counts as capped too.
func TestEnforceCapsPersonalCap(t *testing.T) {
	players := []aiPlayer{
		{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"}, {ID: "d", Name: "D"},
		{ID: "e", Name: "E"}, {ID: "f", Name: "F"},
	}
	played := map[string]int{"a": 1, "b": 1, "c": 1, "d": 1, "e": 2, "f": 2}
	caps := map[string]int{"e": 2, "f": 2}

	got := enforceCaps(players, played, caps, 5)
	for _, p := range got {
		if p.ID == "e" || p.ID == "f" {
			t.Errorf("%s hit their personal cap of 2 but stayed in the pool", p.Name)
		}
	}
	if len(got) != 4 {
		t.Fatalf("expected the four players with games left, got %d", len(got))
	}
}

// TestEnforceCapsAllCapped proves nobody is silently scheduled when every
// player has used their allowance.
func TestEnforceCapsAllCapped(t *testing.T) {
	players := []aiPlayer{
		{ID: "a", Name: "A"}, {ID: "b", Name: "B"},
		{ID: "c", Name: "C"}, {ID: "d", Name: "D"},
	}
	played := map[string]int{"a": 5, "b": 5, "c": 5, "d": 5}

	got := enforceCaps(players, played, nil, 5)
	if len(got) != len(players) {
		t.Fatalf("expected the pool untouched when everyone is capped, got %d", len(got))
	}
}
