package matchmaker

import (
	"strings"
	"testing"

	"github.com/pb-kecebong/backend/internal/httpx"
)

func TestNormalizeStrategy(t *testing.T) {
	cases := map[string]string{
		"":           strategyLeveling,
		"leveling":   strategyLeveling,
		"level":      strategyLeveling,
		"LEVELING":   strategyLeveling,
		" rest ":     strategyRest,
		"istirahat":  strategyRest,
		"rest":       strategyRest,
		"random":     "",
		"harusnya-x": "",
	}
	for in, want := range cases {
		if got := normalizeStrategy(in); got != want {
			t.Errorf("normalizeStrategy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDropBench(t *testing.T) {
	players := []aiPlayer{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	bench := map[string]bool{"a": true, "c": true, "e": true}

	// Leveling keeps everyone: live players are only benched first, not
	// removed, otherwise an odd pool could never fill its courts.
	if got := dropBench(players, bench, strategyLeveling); len(got) != len(players) {
		t.Errorf("leveling dropped players: got %d, want %d", len(got), len(players))
	}

	got := dropBench(players, bench, strategyRest)
	ids := []string{}
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if len(ids) != 2 || ids[0] != "b" || ids[1] != "d" {
		t.Errorf("resting kept %v, want [b d]", ids)
	}

	// Nothing benched: the pool passes through untouched.
	if got := dropBench(players, nil, strategyRest); len(got) != len(players) {
		t.Errorf("empty bench dropped players: got %d", len(got))
	}
}

func TestRequireDrawable(t *testing.T) {
	if err := requireDrawable(4, strategyRest); err != nil {
		t.Errorf("4 players should be drawable, got %v", err)
	}
	if err := requireDrawable(8, strategyLeveling); err != nil {
		t.Errorf("8 players should be drawable, got %v", err)
	}
	err := requireDrawable(3, strategyRest)
	if err == nil {
		t.Fatal("3 players in resting mode should be refused")
	}
	// The message has to name the mode, otherwise the user cannot tell why
	// leveling works at that moment and resting does not.
	if msg := httpx.AsAppError(err).Message; !strings.Contains(msg, "istirahat") {
		t.Errorf("resting refusal should mention the mode, got %q", msg)
	}
	if err := requireDrawable(3, strategyLeveling); err == nil {
		t.Error("3 players should be refused in any mode")
	}
}
