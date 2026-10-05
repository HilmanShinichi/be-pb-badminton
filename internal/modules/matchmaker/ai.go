package matchmaker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/pb-kecebong/backend/internal/httpx"
)

// aiPlayer is one pool member offered to the model. Rank is numeric strength:
// lower is stronger (A1=1 … C3=9, ungraded=99). Arrival is check-in order:
// 1 arrived first. Gender L = male, P = female.
type aiPlayer struct {
	Index   int     `json:"i"`
	ID      string  `json:"-"`
	Name    string  `json:"name"`
	Grade   *string `json:"grade"`
	Gender  *string `json:"gender"`
	Rank    int     `json:"rank"`
	Arrival int     `json:"arrival"`
	// Played counts matches already played or scheduled (ENDED, PLAYING,
	// UPCOMING): the load-balancing signal, lowest plays first.
	Played int `json:"played"`
}

type aiHistoryMatch struct {
	Round int      `json:"round"`
	Team1 []string `json:"team1"`
	Team2 []string `json:"team2"`
}

type aiMatchup struct {
	Team1 []string `json:"team1"`
	Team2 []string `json:"team2"`
}

type aiRound struct {
	Round   int         `json:"round"`
	Matches []aiMatchup `json:"matches"`
}

type aiOutput struct {
	Rounds []aiRound `json:"rounds"`
}

func gradeRank(g *string) int {
	if g == nil {
		return 99
	}
	switch strings.ToUpper(strings.TrimSpace(*g)) {
	case "A1":
		return 1
	case "A2":
		return 2
	case "A3":
		return 3
	case "B1":
		return 4
	case "B2":
		return 5
	case "B3":
		return 6
	case "C1":
		return 7
	case "C2":
		return 8
	case "C3":
		return 9
	}
	return 99
}

const aiSystemPrompt = `You organize balanced badminton doubles (2v2) matchups. ` +
	`Each player has a numeric rank where LOWER is STRONGER (1=A1 strongest … 9=C3 weakest, 99=ungraded). ` +
	`Rules: every match is exactly 2v2; keep the sum of ranks on both sides as close as possible; ` +
	`cover EVERY player every round: with N players make exactly floor(N/4) matches ` +
	`(20 players = 5 matches, 13 players = 3 matches); leave out at most N mod 4 players, ` +
	`preferring the latest arrivals to sit out this round, ` +
	`and rotate who sits out across rounds so no one sits out twice before everyone sat out once); ` +
	`MIXED DOUBLES is mandatory for female players: any side containing a gender-P player must pair ` +
	`her with exactly one gender-L player (P+P or P+unknown is forbidden; L may pair with L or P); ` +
	`vary partnerships across rounds; within one round a player appears at most once; ` +
	`prioritize early arrivals (low arrival number) for earlier rounds and fuller schedules, ` +
	`but keep it fair: across all generated rounds no player may sit out more than one round extra ` +
	`compared to anyone else; ` +
	`LOAD FIRST: each player has a played count (matches already played or scheduled). ` +
	`Fill every round from the LOWEST played count upward: players who played less must play more. ` +
	`When N mod 4 players must sit out, sit out the HIGHEST played count first ` +
	`(ties: latest arrival sits out); early arrival only breaks remaining ties; ` +
	`NEVER repeat an exact matchup from history (same four players with the same sides). ` +
	`Players are numbered by "i": always reference players by their index number as a string ` +
	`(e.g. team1 ["0","3"]), never by name or id. ` +
	`OUTPUT CONTRACT (mandatory): your entire response must be exactly one JSON object, ` +
	`no markdown tables, no bullet lists, no headings, no explanations, no other text: ` +
	`{"rounds": [{"round": N, "matches": [{"team1": ["<idx>", "<idx>"], "team2": ["<idx>", "<idx>"]}]}]}.`

// Free-tier AI providers (Groq 8k tokens/min on gpt-oss) rate-limit by a few
// thousand tokens a minute, so calls are spaced out and identical prompts are
// served from a short-lived cache instead of hitting the provider again.
const (
	aiMinCallGap = 8 * time.Second
	aiCacheTTL   = 10 * time.Minute
	aiMaxTokens  = 1500
)

var (
	aiCallMu   sync.Mutex
	aiLastCall time.Time
	aiCacheMu  sync.Mutex
	aiCache    = map[string]aiCacheEntry{}
)

type aiCacheEntry struct {
	raw string
	at  time.Time
}

func aiCacheKey(provider, model, user string) string {
	sum := sha256.Sum256([]byte(provider + "|" + model + "|" + user))
	return hex.EncodeToString(sum[:])
}

func aiCached(key string) (string, bool) {
	aiCacheMu.Lock()
	defer aiCacheMu.Unlock()
	e, ok := aiCache[key]
	if !ok || time.Since(e.at) > aiCacheTTL {
		if ok {
			delete(aiCache, key)
		}
		return "", false
	}
	return e.raw, true
}

func aiStore(key, raw string) {
	aiCacheMu.Lock()
	defer aiCacheMu.Unlock()
	if len(aiCache) >= 24 {
		oldestKey := ""
		var oldest time.Time
		for k, e := range aiCache {
			if oldestKey == "" || e.at.Before(oldest) {
				oldestKey, oldest = k, e.at
			}
		}
		delete(aiCache, oldestKey)
	}
	aiCache[key] = aiCacheEntry{raw: raw, at: time.Now()}
}

// aiThrottle blocks until aiMinCallGap has passed since the previous
// provider call, keeping requests inside the per-minute token budget.
func aiThrottle(ctx context.Context) error {
	aiCallMu.Lock()
	wait := aiMinCallGap - time.Since(aiLastCall)
	if wait < 0 {
		wait = 0
	}
	aiLastCall = time.Now().Add(wait)
	aiCallMu.Unlock()
	if wait == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// aiEndpoint is one LLM target in the fallback chain. Label is the stable
// human-facing name ("AI 1", "AI 2", "AI 3") shown in the loading UI.
type aiEndpoint struct {
	Label    string
	Provider string
	Model    string
	Base     string
	Key      string
}

// aiEndpoints builds the fallback chain from env: primary AI_* is AI 1,
// AI_*_2 is AI 2, AI_*_3 is AI 3. Entries without an API key are skipped so
// a partially configured fallback never produces an auth error.
func aiEndpoints(cfg config.Config) []aiEndpoint {
	out := []aiEndpoint{}
	add := func(label, provider, model, base, key string) {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			provider = "openai"
		}
		model = strings.TrimSpace(model)
		base = strings.TrimSpace(base)
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		switch provider {
		case "openai":
			if model == "" {
				model = "gpt-4o-mini"
			}
			if base == "" {
				base = "https://api.openai.com/v1"
			}
		case "claude":
			if model == "" {
				model = "claude-3-5-haiku-20241022"
			}
			if base == "" {
				base = "https://api.anthropic.com"
			}
		default:
			// Unknown provider: keep the entry so the caller can report a
			// clear config error instead of silently skipping it.
		}
		out = append(out, aiEndpoint{Label: label, Provider: provider, Model: model, Base: base, Key: key})
	}
	add("AI 1", cfg.AIProvider, cfg.AIModel, cfg.AIBaseURL, cfg.AIAPIKey)
	add("AI 2", cfg.AIProvider2, cfg.AIModel2, cfg.AIBaseURL2, cfg.AIAPIKey2)
	add("AI 3", cfg.AIProvider3, cfg.AIModel3, cfg.AIBaseURL3, cfg.AIAPIKey3)
	return out
}

func isProviderError(err error) bool {
	if err == nil {
		return false
	}
	return httpx.AsAppError(err).Code == "AI_PROVIDER_ERROR"
}

// buildUserPrompt renders the exact user message sent to the model: player
// lines keyed by index (no UUIDs on the wire), history as one line per past
// matchup, plus the soft cross-week guard on attempt 0. rest names players
// coming straight off the previous round's last wave: bench them first.
func buildUserPrompt(players []aiPlayer, history []aiHistoryMatch, softHistory []aiHistoryMatch, rounds, attempt int, rest []string) string {
	var pb strings.Builder
	for _, p := range players {
		grade := "-"
		if p.Grade != nil && *p.Grade != "" {
			grade = *p.Grade
		}
		gender := "-"
		if p.Gender != nil && *p.Gender != "" {
			gender = *p.Gender
		}
		fmt.Fprintf(&pb, "%d|%s|%s|%s|rank%d|arr%d|played%d\n", p.Index, p.Name, grade, gender, p.Rank, p.Arrival, p.Played)
	}
	var hb strings.Builder
	for _, h := range history {
		if len(h.Team1) == 2 && len(h.Team2) == 2 {
			fmt.Fprintf(&hb, "R%d: %s+%s vs %s+%s\n", h.Round,
				h.Team1[0], h.Team1[1], h.Team2[0], h.Team2[1])
		}
	}
	user := fmt.Sprintf("Generate %d round(s) of 2v2 doubles from these players (index|name|grade|gender|rank|arrival|played, use every index at most once per round):\n%sHistory to never repeat (R=round):\n%s",
		rounds, pb.String(), hb.String())
	if attempt == 0 && len(softHistory) > 0 {
		var sb strings.Builder
		for _, h := range softHistory {
			if len(h.Team1) == 2 && len(h.Team2) == 2 {
				fmt.Fprintf(&sb, "R%d: %s+%s vs %s+%s\n", h.Round,
					h.Team1[0], h.Team1[1], h.Team2[0], h.Team2[1])
			}
		}
		if sb.Len() > 0 {
			user += "Last week's final round(s) in the same period (avoid repeating these if possible, repeat only if no other valid 2v2 exists):\n" + sb.String()
		}
	}
	if attempt > 0 {
		user += fmt.Sprintf("\nThis is retry %d: use different partnerships and pairings than the obvious balanced split.", attempt)
	}
	if len(rest) > 0 {
		user += "\nRest these players first (they played the last wave of the previous round, or are still playing live right now): " + strings.Join(rest, ", ") +
			". Bench them before anyone else with equal play counts, and never schedule them in the first wave."
	}
	return user
}

// resolveIdxRefs maps player index references ("0", "3") back to real
// player ids. Shared by the API chain and the manual paste flow.
func resolveIdxRefs(players []aiPlayer) func([]string) ([]string, error) {
	byIndex := map[int]string{}
	for _, p := range players {
		byIndex[p.Index] = p.ID
	}
	return func(refs []string) ([]string, error) {
		ids := make([]string, 0, len(refs))
		for _, ref := range refs {
			n, err := strconv.Atoi(strings.TrimSpace(ref))
			if err != nil {
				return nil, fmt.Errorf("bad player reference %q", ref)
			}
			id, ok := byIndex[n]
			if !ok {
				return nil, fmt.Errorf("unknown player index %q", ref)
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
}

// generateMatchups asks the configured providers in order (AI 1, then
// AI 2, then AI 3 from env) for balanced 2v2 rounds. The first endpoint
// that returns usable JSON wins; provider errors (rate-limit, auth,
// network) and unusable answers fall through to the next endpoint. Returns
// the structured matchups, the endpoint that produced them, and short
// skip notes for every endpoint that failed (so the UI can show WHY the
// draw fell back), or an AppError the handler can write directly.
// softHistory holds last-round matchups from other events in the same period:
// the model should avoid repeating them if possible, but may repeat when
// forced (attempt > 0 drops the soft section entirely).
func generateMatchups(ctx context.Context, cfg config.Config, players []aiPlayer, history []aiHistoryMatch, softHistory []aiHistoryMatch, rounds, attempt int, rest []string) ([]aiRound, aiEndpoint, []string, error) {
	endpoints := aiEndpoints(cfg)
	if len(endpoints) == 0 {
		return nil, aiEndpoint{}, nil, httpx.Unprocessable("AI generation is not configured. Set AI_API_KEY (plus AI_PROVIDER, AI_MODEL, AI_BASE_URL) to enable it.")
	}
	for _, ep := range endpoints {
		if ep.Provider != "openai" && ep.Provider != "claude" {
			return nil, aiEndpoint{}, nil, httpx.Unprocessable("Unknown AI_PROVIDER '" + ep.Provider + "' on " + ep.Label + ". Use openai or claude.")
		}
	}

	// Compact prompt: players as short lines keyed by index (no UUIDs on the
	// wire), history as one line per past matchup. Keeps token usage low.
	user := buildUserPrompt(players, history, softHistory, rounds, attempt, rest)

	// Resolve index references back to real player ids.
	resolve := resolveIdxRefs(players)

	// Try endpoints in order. Provider errors fall through to the next AI;
	// unusable JSON also tries the next AI before giving up, so one bad
	// model response does not burn the whole generate. Every skip is noted
	// so the caller can tell the UI why the primary AI was not used.
	var lastErr error
	skips := []string{}
	skip := func(ep aiEndpoint, reason string) {
		if len(reason) > 160 {
			reason = reason[:160] + "…"
		}
		skips = append(skips, ep.Label+": "+reason)
	}
	for _, ep := range endpoints {
		cacheKey := aiCacheKey(ep.Provider, ep.Model, user)
		var raw string
		var err error
		if cached, ok := aiCached(cacheKey); ok {
			raw = cached
		} else if ep.Provider == "claude" {
			raw, err = callClaude(ctx, ep.Base, ep.Key, ep.Model, user)
		} else {
			raw, err = callOpenAI(ctx, ep.Base, ep.Key, ep.Model, user)
		}
		if err != nil {
			// Non-provider errors (e.g. cancelled context) stop the chain.
			if !isProviderError(err) {
				return nil, aiEndpoint{}, skips, err
			}
			skip(ep, httpx.AsAppError(err).Message)
			lastErr = err
			continue
		}
		out, err := parseAIOutput(raw)
		if err != nil {
			// Lenient path for chatty models (kecebong answers with a
			// markdown table): extract name pairs and validate them exactly
			// like JSON answers below.
			if md, mderr := parseMarkdownDraw(raw, players); mderr == nil && len(md) > 0 {
				if cfg.AIDebug {
					log.Printf("[ai-debug] %s model=%s markdown table accepted (%d matches)", ep.Label, ep.Model, len(md))
				}
				out = aiOutput{Rounds: []aiRound{{Round: 1, Matches: md}}}
			} else if ep.Provider == "openai" {
				// Repair: show the model its rejected answer and demand JSON
				// only, once, before falling through to the next endpoint.
				repaired := false
				if fixed, rerr := callOpenAIRepair(ctx, ep.Base, ep.Key, ep.Model, user, raw); rerr == nil {
					if out2, perr := parseAIOutput(fixed); perr == nil {
						out, err, repaired = out2, nil, true
					} else if md2, mderr := parseMarkdownDraw(fixed, players); mderr == nil && len(md2) > 0 {
						out, err, repaired = aiOutput{Rounds: []aiRound{{Round: 1, Matches: md2}}}, nil, true
					}
				}
				if cfg.AIDebug {
					log.Printf("[ai-debug] %s model=%s repair ok=%v", ep.Label, ep.Model, repaired)
				}
				if !repaired {
					if cfg.AIDebug {
						snip := raw
						if len(snip) > 600 {
							snip = snip[:600]
						}
						log.Printf("[ai-debug] %s model=%s REJECTED len=%d raw=%.600s", ep.Label, ep.Model, len(raw), snip)
					}
					skip(ep, "balasan bukan JSON (sudah diminta ulang 1x)")
					lastErr = httpx.Unprocessable("AI returned unusable matchups (" + err.Error() + "). Try generating again.")
					continue
				}
			} else {
				if cfg.AIDebug {
					snip := raw
					if len(snip) > 600 {
						snip = snip[:600]
					}
					log.Printf("[ai-debug] %s model=%s REJECTED len=%d raw=%.600s", ep.Label, ep.Model, len(raw), snip)
				}
				skip(ep, "balasan bukan JSON valid")
				lastErr = httpx.Unprocessable("AI returned unusable matchups (" + err.Error() + "). Try generating again.")
				continue
			}
		}
		if len(out.Rounds) == 0 {
			skip(ep, "tidak ada ronde di balasan")
			lastErr = httpx.Unprocessable("AI returned no rounds. Try generating again.")
			continue
		}
		badRef := ""
		for ri := range out.Rounds {
			for mi := range out.Rounds[ri].Matches {
				t1, err := resolve(out.Rounds[ri].Matches[mi].Team1)
				if err != nil {
					badRef = err.Error()
					break
				}
				t2, err := resolve(out.Rounds[ri].Matches[mi].Team2)
				if err != nil {
					badRef = err.Error()
					break
				}
				out.Rounds[ri].Matches[mi].Team1 = t1
				out.Rounds[ri].Matches[mi].Team2 = t2
			}
			if badRef != "" {
				break
			}
		}
		if badRef != "" {
			skip(ep, "referensi pemain salah ("+badRef+")")
			lastErr = httpx.Unprocessable("AI returned unusable matchups (" + badRef + "). Try generating again.")
			continue
		}
		aiStore(cacheKey, raw)
		return out.Rounds, ep, skips, nil
	}
	if lastErr != nil {
		return nil, aiEndpoint{}, skips, lastErr
	}
	return nil, aiEndpoint{}, skips, httpx.Unprocessable("AI returned no rounds. Try generating again.")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func parseAIOutput(raw string) (aiOutput, error) {
	var out aiOutput
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if i := strings.LastIndex(s, "}"); i >= 0 {
		s = s[:i+1]
	}
	s = strings.TrimPrefix(strings.TrimSuffix(s, "```"), "```json")
	s = strings.TrimSpace(s)
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return out, fmt.Errorf("not valid JSON")
	}
	return out, nil
}

// parseMarkdownDraw handles models that answer with a markdown table or
// "A vs B" lines instead of JSON, e.g.:
//
//	| 1 | P_Tri (0) & Budi (5) | Agus (2) & Dedi (7) |
//
// Player names are matched against the pool (case-insensitive);
// parentheticals like (0) or (rank1) are ignored. Returns matchups with
// player INDEX refs, exactly like the JSON path, so the same downstream
// validation (2v2, mixed doubles, coverage, no repeat) still applies.
func parseMarkdownDraw(raw string, players []aiPlayer) ([]aiMatchup, error) {
	// stripParenthetical removes (...) / [...] / {...} chunks: models append
	// the index, rank, or grade there and it only confuses name matching.
	stripParenthetical := func(s string) string {
		var b strings.Builder
		depth := 0
		for _, r := range s {
			switch r {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			default:
				if depth == 0 {
					b.WriteRune(r)
				}
			}
		}
		return b.String()
	}
	// Index pool names under the full name AND the paren-stripped name, so
	// both "Aciel ( dari AYO )" and a model-shortened "Aciel" resolve.
	// A key claimed by two players is ambiguous (-1) and never matches.
	byName := map[string]int{}
	addKey := func(key string, idx int) {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return
		}
		if prev, ok := byName[key]; ok && prev != idx {
			byName[key] = -1
			return
		}
		byName[key] = idx
	}
	for _, p := range players {
		addKey(p.Name, p.Index)
		if stripped := stripParenthetical(p.Name); !strings.EqualFold(strings.TrimSpace(stripped), strings.TrimSpace(p.Name)) {
			addKey(stripped, p.Index)
		}
	}
	lookup := func(tok string) (int, bool) {
		if idx, ok := byName[strings.ToLower(strings.TrimSpace(tok))]; ok && idx >= 0 {
			return idx, true
		}
		if idx, ok := byName[strings.ToLower(strings.TrimSpace(stripParenthetical(tok)))]; ok && idx >= 0 {
			return idx, true
		}
		return 0, false
	}
	// teamToRefs turns one side ("P_Tri & Budi", "0 — Aby + 2 — Agus")
	// into index refs. Fragments may be names ("Aby"), bare indices ("0"),
	// or redundant "index — name" pairs, which dedupe to one player when
	// both halves agree. Markdown bold/quote marks are trimmed.
	teamToRefs := func(cell string) ([]string, error) {
		cleaned := stripParenthetical(cell)
		for _, sep := range []string{"&", "+", "/", ",", ";", "—", "–", "·", "•", " dan ", " DAN ", " and ", " AND "} {
			cleaned = strings.ReplaceAll(cleaned, sep, "|")
		}
		// " vs " only with spaces, so names like "Vs…" never split.
		cleaned = strings.ReplaceAll(cleaned, " vs ", "|")
		cleaned = strings.ReplaceAll(cleaned, " VS ", "|")
		refs := []string{}
		seen := map[string]bool{}
		push := func(r string) {
			if !seen[r] {
				seen[r] = true
				refs = append(refs, r)
			}
		}
		for _, tok := range strings.Split(cleaned, "|") {
			frag := strings.Trim(tok, " \t*_`\"'~#")
			if frag == "" {
				continue
			}
			if n, err := strconv.Atoi(frag); err == nil && n >= 0 && n < len(players) {
				push(strconv.Itoa(n))
				continue
			}
			idx, ok := lookup(frag)
			if !ok {
				return nil, fmt.Errorf("unknown player %q", frag)
			}
			push(strconv.Itoa(idx))
		}
		if len(refs) != 2 {
			return nil, fmt.Errorf("side has %d players, want 2", len(refs))
		}
		return refs, nil
	}
	isHeaderOrSep := func(line string) bool {
		l := strings.ToLower(line)
		return strings.Contains(line, "---") ||
			(strings.Contains(l, "court") && strings.Contains(l, "team"))
	}
	out := []aiMatchup{}
	// Shape 1: markdown table rows, team sides in the 2nd and 3rd cells.
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, "|") || isHeaderOrSep(line) {
			continue
		}
		cells := []string{}
		for _, c := range strings.Split(line, "|") {
			if t := strings.TrimSpace(c); t != "" {
				cells = append(cells, t)
			}
		}
		if len(cells) < 2 {
			continue
		}
		t1, t2 := cells[len(cells)-2], cells[len(cells)-1]
		r1, err1 := teamToRefs(t1)
		r2, err2 := teamToRefs(t2)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, aiMatchup{Team1: r1, Team2: r2})
	}
	// Shape 2: plain "A & B vs C & D" lines when no table parsed.
	if len(out) == 0 {
		for _, line := range strings.Split(raw, "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.Contains(l, "|") || isHeaderOrSep(l) {
				continue
			}
			sep := " vs "
			if !strings.Contains(strings.ToLower(l), sep) {
				continue
			}
			idx := strings.Index(strings.ToLower(l), sep)
			r1, err1 := teamToRefs(l[:idx])
			r2, err2 := teamToRefs(l[idx+len(sep):])
			if err1 != nil || err2 != nil {
				continue
			}
			out = append(out, aiMatchup{Team1: r1, Team2: r2})
		}
	}
	// Shape 4: bullet/list lines with explicit side labels, no table:
	//   **Court 1**
	//   - Team A: Agus (2) + Amir (5)
	//   - Team B: Alen (4) + Ari (6)
	// Consecutive A+B lines form one matchup; a garbled line drops the
	// pending side so courts never cross-pair.
	if len(out) == 0 {
		teamLine := regexp.MustCompile(`(?i)^[\s\-\*0-9\.\)]*team\s*([ab12])[\s\*]*:`)
		var pendA, pendB []string
		haveA, haveB := false, false
		flush := func() {
			if haveA && haveB {
				out = append(out, aiMatchup{Team1: append([]string{}, pendA...), Team2: append([]string{}, pendB...)})
			}
			pendA, pendB, haveA, haveB = nil, nil, false, false
		}
		for _, line := range strings.Split(raw, "\n") {
			m := teamLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			refs, err := teamToRefs(line[strings.Index(line, ":")+1:])
			if err != nil {
				flush()
				continue
			}
			switch strings.ToLower(m[1]) {
			case "a", "1":
				if haveA {
					flush()
				}
				pendA, haveA = refs, true
			default:
				if haveB {
					flush()
				}
				pendB, haveB = refs, true
			}
			if haveA && haveB {
				flush()
			}
		}
		flush()
	}
	//   ### Match 1
	//   | Team A | 0 | Aby | A2 | L | rank2 |
	//   | Team B | 2 | Agus | A1 | L | rank1 |
	// Rows accumulate per match until both sides have exactly 2 players.
	if len(out) == 0 {
		matchHeader := regexp.MustCompile(`(?i)match\s+(\d+)`)
		teamKey := func(cell string) string {
			l := strings.ToLower(strings.TrimSpace(cell))
			if strings.Contains(l, "team a") || l == "a" || strings.Contains(l, "tim a") || strings.Contains(l, "team 1") {
				return "A"
			}
			if strings.Contains(l, "team b") || l == "b" || strings.Contains(l, "tim b") || strings.Contains(l, "team 2") {
				return "B"
			}
			return ""
		}
		// playerRef prefers a numeric index cell, else a name lookup.
		playerRef := func(cells []string) (string, bool) {
			for _, c := range cells {
				if t := strings.TrimSpace(c); t != "" {
					if n, err := strconv.Atoi(t); err == nil && n >= 0 && n < len(players) {
						return strconv.Itoa(n), true
					}
				}
			}
			for _, c := range cells {
				if t := strings.TrimSpace(c); t != "" {
					if _, err := strconv.Atoi(t); err == nil {
						continue
					}
					if idx, ok := lookup(t); ok {
						return strconv.Itoa(idx), true
					}
				}
			}
			return "", false
		}
		type sides struct{ A, B []string }
		groups := map[int]*sides{}
		order := []int{}
		current := 0
		for _, line := range strings.Split(raw, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(line, "|") {
				if m := matchHeader.FindStringSubmatch(trimmed); m != nil {
					fmt.Sscanf(m[1], "%d", &current)
					if _, ok := groups[current]; !ok {
						groups[current] = &sides{}
						order = append(order, current)
					}
				}
				continue
			}
			if isHeaderOrSep(line) || current == 0 {
				continue
			}
			cells := []string{}
			for _, c := range strings.Split(line, "|") {
				if t := strings.TrimSpace(c); t != "" {
					cells = append(cells, t)
				}
			}
			if len(cells) < 2 {
				continue
			}
			key := teamKey(cells[0])
			if key == "" {
				continue
			}
			ref, ok := playerRef(cells[1:])
			if !ok {
				continue
			}
			g := groups[current]
			if key == "A" {
				g.A = append(g.A, ref)
			} else {
				g.B = append(g.B, ref)
			}
		}
		for _, no := range order {
			g := groups[no]
			if len(g.A) == 2 && len(g.B) == 2 {
				out = append(out, aiMatchup{Team1: append([]string{}, g.A...), Team2: append([]string{}, g.B...)})
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no team rows found")
	}
	return out, nil
}

func callOpenAI(ctx context.Context, base, key, model, user string) (string, error) {
	return callOpenAIWithMessages(ctx, base, key, model, []map[string]any{
		{"role": "system", "content": aiSystemPrompt},
		{"role": "user", "content": user},
	})
}

// callOpenAIRepair shows the model its rejected answer and demands JSON
// only, once, before the caller falls through to the next endpoint.
func callOpenAIRepair(ctx context.Context, base, key, model, user, badRaw string) (string, error) {
	if len(badRaw) > 1200 {
		badRaw = badRaw[:1200]
	}
	return callOpenAIWithMessages(ctx, base, key, model, []map[string]any{
		{"role": "system", "content": aiSystemPrompt},
		{"role": "user", "content": user},
		{"role": "assistant", "content": badRaw},
		{"role": "user", "content": `That reply was rejected because it was not valid JSON. Reply again with ONLY the JSON object, no markdown tables, no bullet lists, no headings, no explanations, no other text: {"rounds": [{"round": 1, "matches": [{"team1": ["0", "1"], "team2": ["2", "3"]}]}]}`},
	})
}

func callOpenAIWithMessages(ctx context.Context, base, key, model string, messages []map[string]any) (string, error) {
	// Strict schema first (guaranteed-valid JSON on providers that support
	// Structured Outputs); gateways that reject it fall back to lenient
	// json_object, then to plain instructions — parseAIOutput tolerates fences.
	modes := []map[string]any{
		{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "doubles_draw",
				"strict": true,
				"schema": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"rounds"},
					"properties": map[string]any{
						"rounds": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":                 "object",
								"additionalProperties": false,
								"required":             []string{"round", "matches"},
								"properties": map[string]any{
									"round": map[string]any{"type": "integer"},
									"matches": map[string]any{
										"type": "array",
										"items": map[string]any{
											"type":                 "object",
											"additionalProperties": false,
											"required":             []string{"team1", "team2"},
											"properties": map[string]any{
												"team1": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
												"team2": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{"type": "json_object"},
	}
	var lastErr error
	for _, mode := range modes {
		// Only schema/format rejections fall through to the next mode.
		body, _ := json.Marshal(map[string]any{
			"model":           model,
			"temperature":     0.7,
			"max_tokens":      aiMaxTokens,
			"stream":          false,
			"response_format": mode,
			"messages":        messages,
		})
		raw, err := doOpenAICall(ctx, base, key, body)
		if err == nil {
			return raw, nil
		}
		if !isSchemaRejection(err) {
			return "", err
		}
		lastErr = err
	}
	_ = lastErr
	// Plain instructions, no response_format at all.
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0.7,
		"max_tokens":  aiMaxTokens,
		"stream":      false,
		"messages":    messages,
	})
	return doOpenAICall(ctx, base, key, body)
}

func isSchemaRejection(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "json_validate_failed") ||
		strings.Contains(msg, "response_format") ||
		strings.Contains(msg, "json_schema")
}

// sseToJSON normalizes a text/event-stream body into the single JSON
// object the caller expects. Some gateways stream despite stream:false:
// either one full completion object per data: line, or delta chunks to
// concatenate. Bodies without data: lines come back untouched.
func sseToJSON(body []byte) []byte {
	var full []byte
	var deltas strings.Builder
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || string(payload) == "[DONE]" {
			continue
		}
		var probe struct {
			Choices []struct {
				Message *struct {
					Content string `json:"content"`
				} `json:"message"`
				Delta *struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil || len(probe.Choices) == 0 {
			continue
		}
		if probe.Choices[0].Message != nil && probe.Choices[0].Message.Content != "" {
			full = payload
			break
		}
		if probe.Choices[0].Delta != nil {
			deltas.WriteString(probe.Choices[0].Delta.Content)
		}
	}
	if full != nil {
		return full
	}
	if deltas.Len() > 0 {
		content, _ := json.Marshal(deltas.String())
		return []byte(`{"choices":[{"message":{"content":` + string(content) + `}}]}`)
	}
	return body
}

func doOpenAICall(ctx context.Context, base, key string, body []byte) (string, error) {
	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := postJSONWith(ctx, strings.TrimSuffix(base, "/")+"/chat/completions", key, "", body, &res, sseToJSON); err != nil {
		return "", err
	}
	if res.Error != nil {
		return "", httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider: "+res.Error.Message)
	}
	if len(res.Choices) == 0 {
		return "", httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider returned no choices.")
	}
	return res.Choices[0].Message.Content, nil
}

func callClaude(ctx context.Context, base, key, model, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": aiMaxTokens,
		"system":     aiSystemPrompt,
		"messages": []map[string]any{
			{"role": "user", "content": user},
		},
	})
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := postJSON(ctx, strings.TrimSuffix(base, "/")+"/v1/messages", "", key, body, &res); err != nil {
		return "", err
	}
	if res.Error != nil {
		return "", httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider: "+res.Error.Message)
	}
	for _, c := range res.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			return c.Text, nil
		}
	}
	return "", httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider returned no text.")
}

func postJSON(ctx context.Context, url, bearer, apiKey string, body []byte, out any) error {
	return postJSONWith(ctx, url, bearer, apiKey, body, out, nil)
}

// postJSONWith is postJSON with an optional body normalizer (e.g. SSE to
// JSON for gateways that stream despite stream:false).
func postJSONWith(ctx context.Context, url, bearer, apiKey string, body []byte, out any, normalize func([]byte) []byte) error {
	var lastErr error
	// One retry only: a 429 means the per-minute budget is spent, and the
	// caller's local fallback is faster than waiting out the provider.
	for attempt := 0; attempt < 2; attempt++ {
		done, wait, err := postJSONOnce(ctx, url, bearer, apiKey, body, out, normalize)
		if done {
			return err
		}
		if err != nil {
			lastErr = err
			break
		}
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(wait):
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return httpx.BadRequest("AI_PROVIDER_ERROR", "AI rate limit reached. Try generating again in a few seconds.")
}

var retryAfterMs = regexp.MustCompile(`(?i)try again in ([\d.]+)\s*ms`)

// rateLimitMsg spots a rate-limit complaint inside an error body. Some
// gateways wrap the provider's 429 as 503/529, so those statuses are also
// worth one retry when the body says rate limit.
var rateLimitMsg = regexp.MustCompile(`(?i)rate[\s_-]*limit|try again in`)

// isWrappedRateLimit reports a 503/529 that is really a rate limit.
func isWrappedRateLimit(status int, body []byte) bool {
	return (status == 503 || status == 529) && rateLimitMsg.Match(body)
}

// postJSONOnce performs one call. It reports (done=true, _, err) for a
// final outcome, or (done=false, wait, nil) when the caller should wait and
// retry after a 429.
func postJSONOnce(ctx context.Context, url, bearer, apiKey string, body []byte, out any, normalize func([]byte) []byte) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	fail := func(err error) (bool, time.Duration, error) { return true, 0, err }
	if err := aiThrottle(ctx); err != nil {
		return fail(httpx.BadRequest("AI_PROVIDER_ERROR", "AI request was cancelled."))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fail(httpx.BadRequest("AI_PROVIDER_ERROR", "Could not reach AI provider."))
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail(httpx.BadRequest("AI_PROVIDER_ERROR", "Could not reach AI provider."))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 429 || isWrappedRateLimit(resp.StatusCode, data) {
		wait := 2 * time.Second
		if m := retryAfterMs.FindSubmatch(data); m != nil {
			if ms, err := strconv.ParseFloat(string(m[1]), 64); err == nil && ms > 0 {
				wait = time.Duration(ms * float64(time.Millisecond))
			}
		}
		return false, wait, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fail(httpx.BadRequest("AI_PROVIDER_ERROR", fmt.Sprintf("AI provider HTTP %d: %s", resp.StatusCode, msg)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		// Gateways that stream despite stream:false: normalize SSE to one
		// JSON object and try once more before giving up.
		if normalize != nil {
			if normed := normalize(data); !bytes.Equal(normed, data) {
				if err := json.Unmarshal(normed, out); err == nil {
					return true, 0, nil
				}
			}
		}
		return fail(httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider returned an unreadable response."))
	}
	return true, 0, nil
}
