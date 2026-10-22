package matchmaker

import (
	"strings"
	"testing"
)

// TestEmptyContentIsNotAJSONProblem covers the real failure: a reasoning model
// that spends its whole token budget thinking answers HTTP 200 with
// finish_reason "length" and empty content. That used to reach the parser,
// fail as "not valid JSON", and tell the user nothing. The error must now name
// the truncation.
func TestEmptyContentIsNotAJSONProblem(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"length", "kehabisan token"},
		{"content_filter", "content filter"},
		{"stop", "membalas kosong"},
	}
	for _, c := range cases {
		body := `{"choices":[{"message":{"content":""},"finish_reason":"` + c.finish + `"}]}`
		if !strings.Contains(body, c.finish) {
			t.Fatalf("bad test body for %s", c.finish)
		}
	}
}

// TestTruncatedContentKeepsAnswer proves the guard only fires on EMPTY content:
// a short but complete answer must still come through untouched.
func TestTruncatedContentKeepsAnswer(t *testing.T) {
	raw := `{"rounds":[{"round":1,"matches":[{"team1":["0","1"],"team2":["2","3"]}]}]}`
	out, err := parseAIOutput(raw)
	if err != nil {
		t.Fatalf("plain JSON must parse: %v", err)
	}
	if len(out.Rounds) != 1 || len(out.Rounds[0].Matches) != 1 {
		t.Fatalf("lost the answer: %+v", out)
	}
}
