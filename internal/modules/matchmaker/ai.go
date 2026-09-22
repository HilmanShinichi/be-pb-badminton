package matchmaker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/pb-kecebong/backend/internal/httpx"
)

// aiPlayer is one pool member offered to the model. Rank is numeric strength:
// lower is stronger (A1=1 … C3=9, ungraded=99). Arrival is check-in order:
// 1 arrived first. Gender L = male, P = female.
type aiPlayer struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Grade   *string `json:"grade"`
	Gender  *string `json:"gender"`
	Rank    int     `json:"rank"`
	Arrival int     `json:"arrival"`
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
	Round   int        `json:"round"`
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
	`MIXED DOUBLES is mandatory for female players: any side containing a gender-P player must pair ` +
	`her with exactly one gender-L player (P+P or P+unknown is forbidden; L may pair with L or P); ` +
	`vary partnerships across rounds; within one round a player appears at most once; ` +
	`prioritize early arrivals (low arrival number) for earlier rounds and fuller schedules, ` +
	`but keep it fair: across all generated rounds no player may sit out more than one round extra ` +
	`compared to anyone else; ` +
	`NEVER repeat an exact matchup from history (same four players with the same sides). ` +
	`Reply with JSON only, no prose: {"rounds": [{"round": N, "matches": [{"team1": ["<player-id>", "<player-id>"], "team2": ["<player-id>", "<player-id>"]}]}]}.`

// generateMatchups asks the configured provider (openai|claude, model and
// base URL from env) for balanced 2v2 rounds. Returns structured matchups or
// an AppError the handler can write directly.
func generateMatchups(ctx context.Context, cfg config.Config, players []aiPlayer, history []aiHistoryMatch, rounds int) ([]aiRound, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.AIProvider))
	if provider == "" {
		provider = "openai"
	}
	model := strings.TrimSpace(cfg.AIModel)
	base := strings.TrimSpace(cfg.AIBaseURL)
	key := strings.TrimSpace(cfg.AIAPIKey)
	if key == "" {
		return nil, httpx.Unprocessable("AI generation is not configured. Set AI_API_KEY (plus AI_PROVIDER, AI_MODEL, AI_BASE_URL) to enable it.")
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
		return nil, httpx.Unprocessable("Unknown AI_PROVIDER '"+provider+"'. Use openai or claude.")
	}

	user := fmt.Sprintf("Generate %d round(s) of 2v2 doubles from these players (use every listed id, at most once per round):\n%s\nHistory to never repeat:\n%s",
		rounds, mustJSON(players), mustJSON(history))

	var raw string
	var err error
	if provider == "claude" {
		raw, err = callClaude(ctx, base, key, model, user)
	} else {
		raw, err = callOpenAI(ctx, base, key, model, user)
	}
	if err != nil {
		return nil, err
	}
	out, err := parseAIOutput(raw)
	if err != nil {
		return nil, httpx.Unprocessable("AI returned unusable matchups ("+err.Error()+"). Try generating again.")
	}
	if len(out.Rounds) == 0 {
		return nil, httpx.Unprocessable("AI returned no rounds. Try generating again.")
	}
	return out.Rounds, nil
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

func callOpenAI(ctx context.Context, base, key, model, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0.7,
		// Strict JSON schema: the provider itself guarantees parseable output
		// shaped exactly like aiOutput, instead of best-effort json_object.
		"response_format": map[string]any{
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
		"messages": []map[string]any{
			{"role": "system", "content": aiSystemPrompt},
			{"role": "user", "content": user},
		},
	})
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
	if err := postJSON(ctx, strings.TrimSuffix(base, "/")+"/chat/completions", key, "", body, &res); err != nil {
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
		"max_tokens": 3000,
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
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		done, wait, err := postJSONOnce(ctx, url, bearer, apiKey, body, out)
		if done {
			return err
		}
		if err != nil {
			lastErr = err
			break
		}
		// 429: wait as advised, then retry. Capped so the request
		// still fits inside the server timeout.
		if wait > 10*time.Second {
			wait = 10 * time.Second
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

// postJSONOnce performs one call. It reports (done=true, _, err) for a
// final outcome, or (done=false, wait, nil) when the caller should wait and
// retry after a 429.
func postJSONOnce(ctx context.Context, url, bearer, apiKey string, body []byte, out any) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	fail := func(err error) (bool, time.Duration, error) { return true, 0, err }
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
	if resp.StatusCode == 429 {
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
		return fail(httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider returned an unreadable response."))
	}
	return true, 0, nil
}
