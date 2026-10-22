package matchmaker

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/config"
	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct {
	db  *pgxpool.Pool
	cfg config.Config
}

func NewService(db *pgxpool.Pool, cfg config.Config) *Service { return &Service{db: db, cfg: cfg} }

var validStatuses = map[string]bool{"UPCOMING": true, "PLAYING": true, "ENDED": true}

// TeamPlayer is one side member stored as an object so names/grades stay
// editable and readable after generation.
type TeamPlayer struct {
	PlayerID string  `json:"player_id"`
	Name     string  `json:"name"`
	Grade    *string `json:"grade"`
	Gender   *string `json:"gender"`
}

type Referee struct {
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
}

type GenMatch struct {
	ID              string       `json:"id"`
	EventID         string       `json:"event_id"`
	Round           int          `json:"round"`
	Wave            int          `json:"wave"`
	Team1           []TeamPlayer `json:"team1"`
	Team2           []TeamPlayer `json:"team2"`
	Referee         *Referee     `json:"referee"`
	Status          string       `json:"status"`
	Court           int          `json:"court"`
	StartedAt       *string      `json:"started_at"`
	EndedAt         *string      `json:"ended_at"`
	ShuttlecockUsed int          `json:"shuttlecock_used"`
	CreatedAt       string       `json:"created_at"`
	UpdatedAt       string       `json:"updated_at"`
}

type MatchEvent struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	CourtCount      int      `json:"court_count"`
	BasePlayed      int      `json:"base_played"`
	MaxRounds       int      `json:"max_rounds"`
	IsPublic        bool     `json:"is_public"`
	ShowGrades      bool     `json:"show_grades"`
	UseReferee      bool     `json:"use_referee"`
	PlayerIDs       []string `json:"player_ids"`
	SourceSessionID *string  `json:"source_session_id"`
	CreatedAt       string   `json:"created_at"`
}

type PlayerCount struct {
	PlayerID  string  `json:"player_id"`
	Name      string  `json:"name"`
	Grade     *string `json:"grade"`
	Gender    *string `json:"gender"`
	Arrival   int     `json:"arrival"`
	Played    int     `json:"played"`
	Refereed  int     `json:"refereed"`
	MaxPlayed *int    `json:"max_played"`
}

func parseTeam(raw string) []TeamPlayer {
	team := []TeamPlayer{}
	if raw == "" || raw == "null" {
		return team
	}
	_ = json.Unmarshal([]byte(raw), &team)
	if team == nil {
		team = []TeamPlayer{}
	}
	return team
}

func mustTeamJSON(team []TeamPlayer) string {
	b, _ := json.Marshal(team)
	return string(b)
}

type poolPlayer struct {
	ID     string
	Name   string
	Grade  *string
	Gender *string
}

// pending is one validated match waiting to be written: its round, its wave
// (courts running at the same time), both sides, and the picked referee.
type pending struct {
	round        int
	wave         int
	team1, team2 []TeamPlayer
	referee      *string
}

func (s *Service) pool(ctx context.Context, eventID string) ([]poolPlayer, error) {
	var idsRaw string
	if err := s.db.QueryRow(ctx,
		`SELECT player_ids::text FROM match_events WHERE id = $1`, eventID).Scan(&idsRaw); err != nil {
		return nil, err
	}
	var ids []string
	_ = json.Unmarshal([]byte(idsRaw), &ids)
	if len(ids) == 0 {
		return []poolPlayer{}, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT id::text, name, grade, gender FROM players WHERE id::text = ANY($1) AND status = 'ACTIVE'
		 ORDER BY array_position($1, id::text)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pool := []poolPlayer{}
	for rows.Next() {
		var p poolPlayer
		if err := rows.Scan(&p.ID, &p.Name, &p.Grade, &p.Gender); err == nil {
			pool = append(pool, p)
		}
	}
	return pool, nil
}

func (s *Service) resolveTeams(pool []poolPlayer, t1, t2 []string) ([]TeamPlayer, []TeamPlayer, error) {
	byID := map[string]poolPlayer{}
	for _, p := range pool {
		byID[p.ID] = p
	}
	build := func(ids []string) ([]TeamPlayer, error) {
		if len(ids) != 2 {
			return nil, httpx.Unprocessable("Each side must have exactly 2 players.")
		}
		if ids[0] == ids[1] {
			return nil, httpx.Unprocessable("A side cannot list the same player twice.")
		}
		team := []TeamPlayer{}
		for _, id := range ids {
			p, ok := byID[id]
			if !ok {
				return nil, httpx.Unprocessable("Player is not in this event pool.")
			}
			team = append(team, TeamPlayer{PlayerID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender})
		}
		// Any gender pairing is allowed (including P+P women's doubles).
		return team, nil
	}
	team1, err := build(t1)
	if err != nil {
		return nil, nil, err
	}
	team2, err := build(t2)
	if err != nil {
		return nil, nil, err
	}
	return team1, team2, nil
}

// matchupKey canonicalizes a 2v2 so repeats are detectable regardless of
// side or within-side ordering.
func matchupKey(t1, t2 []string) string {
	a := append([]string{}, t1...)
	b := append([]string{}, t2...)
	sort.Strings(a)
	sort.Strings(b)
	sides := []string{strings.Join(a, "+"), strings.Join(b, "+")}
	sort.Strings(sides)
	return strings.Join(sides, " vs ")
}

// assignReferees gives every match a referee from the pool: the player who
// has played least so far (longest idle) and is not on a court in that
// match's wave, so a referee never has to watch their own game while it runs.
// One person can only hold one whistle per wave; matches in a full wave get
// no referee and can be set by hand.
func assignReferees(pool []poolPlayer, out []pending, waves map[int]int, played map[string]int) {
	byWave := map[int][]int{}
	for i := range out {
		byWave[waves[i]] = append(byWave[waves[i]], i)
	}
	arrival := map[string]int{}
	for i, p := range pool {
		arrival[p.ID] = i
	}
	for _, idxs := range byWave {
		onCourt := map[string]bool{}
		for _, i := range idxs {
			for _, id := range append(idsOf(out[i].team1), idsOf(out[i].team2)...) {
				onCourt[id] = true
			}
		}
		busy := map[string]bool{}
		for _, i := range idxs {
			best := ""
			bestPlayed, bestArrival := 1<<30, -1
			for _, p := range pool {
				if onCourt[p.ID] || busy[p.ID] {
					continue
				}
				pc, a := played[p.ID], arrival[p.ID]
				if pc < bestPlayed || (pc == bestPlayed && a > bestArrival) {
					best, bestPlayed, bestArrival = p.ID, pc, a
				}
			}
			if best != "" {
				busy[best] = true
				out[i].referee = &best
			}
		}
	}
}

// localMatchups draws one round locally under the same rules the AI has to
// follow: 2v2, mixed doubles, no repeat, and the pool covered apart from the
// unavoidable sit-outs. It is the fallback whenever the provider is
// rate-limited or answers with something unusable, so generating never fails
// just because the model could not be reached.
func (s *Service) localMatchups(players []aiPlayer, pool []poolPlayer, seen map[string]bool,
	roundNo, waveBase, wavesPerRound int, played map[string]int,
	newcomers []string, borrow map[string]bool, useReferee bool, caps map[string]int, eventMax int,
	rest map[string]bool) ([]pending, error) {

	rank := map[string]int{}
	arrival := map[string]int{}
	for i, p := range players {
		rank[p.ID] = p.Rank
		arrival[p.ID] = i
	}
	// Any gender pairing is allowed (including P+P women's doubles).
	must := map[string]bool{}
	for _, id := range newcomers {
		must[id] = true
	}
	for id := range borrow {
		must[id] = true
	}
	// Sit-outs go to whoever hit their personal cap first, then whoever has
	// played most, then whoever just came off the court (rest set); ties
	// after that fall back to latest arrival. Late arrivals and borrowed
	// top-up players always play.
	optional := []string{}
	for _, p := range players {
		if !must[p.ID] {
			optional = append(optional, p.ID)
		}
	}
	atCap := func(id string) bool {
		cap := eventMax
		if c, ok := caps[id]; ok {
			cap = c
		}
		return cap > 0 && played[id] >= cap
	}
	sort.Slice(optional, func(i, j int) bool {
		if ai, aj := atCap(optional[i]), atCap(optional[j]); ai != aj {
			return ai
		}
		if played[optional[i]] != played[optional[j]] {
			return played[optional[i]] > played[optional[j]]
		}
		if ri, rj := rest[optional[i]], rest[optional[j]]; ri != rj {
			return ri
		}
		return arrival[optional[i]] > arrival[optional[j]]
	})
	sitOut := len(players) % 4
	if sitOut > len(optional) {
		sitOut = len(optional)
	}
	sitting := map[string]bool{}
	for _, id := range optional[:sitOut] {
		sitting[id] = true
	}
	playing := []string{}
	for _, p := range players {
		if !sitting[p.ID] {
			playing = append(playing, p.ID)
		}
	}
	blocks := len(playing) / 4
	if blocks == 0 {
		return nil, httpx.Unprocessable("Not enough players to build a 2v2 matchup.")
	}

	seed := uint64(roundNo)*7919 + uint64(len(playing))*104729 + 1
	rand64 := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	pairings := [][4]int{{0, 1, 2, 3}, {0, 2, 1, 3}, {0, 3, 1, 2}}
	for try := 0; try < 80; try++ {
		draw := append([]string{}, playing...)
		for i := len(draw) - 1; i > 0; i-- {
			j := int(rand64() % uint64(i+1))
			draw[i], draw[j] = draw[j], draw[i]
		}
		matches := []aiMatchup{}
		added := map[string]bool{}
		ok := true
		for b := 0; b < blocks; b++ {
			q := draw[b*4 : b*4+4]
			best := [4]int{}
			bestDiff := 0
			found := false
			for _, pr := range pairings {
				t1 := []string{q[pr[0]], q[pr[1]]}
				t2 := []string{q[pr[2]], q[pr[3]]}
				diff := rank[t1[0]] + rank[t1[1]] - rank[t2[0]] - rank[t2[1]]
				if diff < 0 {
					diff = -diff
				}
				if !found || diff < bestDiff {
					best, bestDiff, found = pr, diff, true
				}
			}
			if !found {
				ok = false
				break
			}
			mu := aiMatchup{Team1: []string{q[best[0]], q[best[1]]}, Team2: []string{q[best[2]], q[best[3]]}}
			key := matchupKey(mu.Team1, mu.Team2)
			if seen[key] || added[key] {
				ok = false
				break
			}
			added[key] = true
			matches = append(matches, mu)
		}
		if !ok {
			continue
		}
		out := []pending{}
		waves := map[int]int{}
		for i, mu := range matches {
			t1, t2, err := s.resolveTeams(pool, mu.Team1, mu.Team2)
			if err != nil {
				ok = false
				break
			}
			waves[i] = waveBase + i/wavesPerRound + 1
			out = append(out, pending{round: roundNo, wave: waves[i], team1: t1, team2: t2})
		}
		if !ok {
			continue
		}
		for _, p := range out {
			seen[matchupKey(idsOf(p.team1), idsOf(p.team2))] = true
		}
		if useReferee {
			assignReferees(pool, out, waves, played)
		}
		return out, nil
	}
	return nil, httpx.Unprocessable("Could not build a valid matchup. Try generating again.")
}

func (s *Service) listMatches(ctx context.Context, eventID string) ([]GenMatch, error) {
	rows, err := s.db.Query(ctx, `
		SELECT m.id::text, m.event_id::text, m.round, m.wave, m.team1::text, m.team2::text, m.status, m.court,
		       m.started_at::text, m.ended_at::text, m.shuttlecock_used,
		       m.referee_id::text, pl.name,
		       m.created_at::text, m.updated_at::text
		FROM generated_matches m
		LEFT JOIN players pl ON pl.id = m.referee_id
		WHERE m.event_id = $1 ORDER BY m.round, m.wave, m.created_at, m.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	matches := []GenMatch{}
	for rows.Next() {
		var m GenMatch
		var t1, t2 string
		var refID, refName *string
		if err := rows.Scan(&m.ID, &m.EventID, &m.Round, &m.Wave, &t1, &t2, &m.Status, &m.Court,
			&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed,
			&refID, &refName, &m.CreatedAt, &m.UpdatedAt); err == nil {
			m.Team1 = parseTeam(t1)
			m.Team2 = parseTeam(t2)
			if refID != nil {
				name := ""
				if refName != nil {
					name = *refName
				}
				m.Referee = &Referee{PlayerID: *refID, Name: name}
			}
			matches = append(matches, m)
		}
	}
	return matches, nil
}

// eventPeriodID resolves the membership period of an event through its
// source session (match_events -> mabar_sessions.period_id). Events without
// a source session have no period link.
func (s *Service) eventPeriodID(ctx context.Context, eventID string) (string, bool) {
	var periodID *string
	if err := s.db.QueryRow(ctx, `
		SELECT ms.period_id::text
		FROM match_events e
		LEFT JOIN mabar_sessions ms ON ms.id = e.source_session_id
		WHERE e.id = $1`, eventID).Scan(&periodID); err != nil {
		return "", false
	}
	if periodID == nil || *periodID == "" {
		return "", false
	}
	return *periodID, true
}

// periodRecentSoftEvents is how many earlier events in the same period feed the
// cross-week no-repeat guard. The guard used to look only at the final round of
// each event, which left most repeat matchups unguarded: an event with five
// waves only contributed its last one.
const periodRecentSoftEvents = 3

// softHistoryPromptLimit caps how many soft rows reach the AI prompt. The
// validator may reject hundreds of keys, but the prompt only needs a taste of
// recent history: a long list dilutes the rest/cap rules and pushes the model
// into retries that end up on the local fallback.
const softHistoryPromptLimit = 20

// periodRecentSoft loads every matchup from the last few OTHER events in the
// same period as soft-avoid history: generate should not repeat these matchups
// unless no other valid 2v2 exists ("kecuali terpaksa").
//
// Events are ranked by match_events.created_at, not by their latest card: a
// leftover card tapped days later would otherwise make an old night look like
// the most recent one.
func (s *Service) periodRecentSoft(ctx context.Context, periodID, currentEventID string) (map[string]bool, []aiHistoryMatch) {
	softSeen := map[string]bool{}
	softHistory := []aiHistoryMatch{}
	rows, err := s.db.Query(ctx, `
		WITH recent AS (
			SELECT e.id
			FROM match_events e
			JOIN mabar_sessions ms ON ms.id = e.source_session_id
			WHERE ms.period_id = $1
			  AND e.id <> $2
			ORDER BY e.created_at DESC
			LIMIT $3
		)
		SELECT m.round, m.team1::text, m.team2::text, m.created_at
		FROM generated_matches m
		JOIN recent r ON r.id = m.event_id
		ORDER BY m.created_at DESC`, periodID, currentEventID, periodRecentSoftEvents)
	if err != nil {
		return softSeen, softHistory
	}
	defer rows.Close()
	for rows.Next() {
		var round int
		var t1raw, t2raw string
		var at time.Time
		if err := rows.Scan(&round, &t1raw, &t2raw, &at); err != nil {
			continue
		}
		t1 := parseTeam(t1raw)
		t2 := parseTeam(t2raw)
		if len(t1) != 2 || len(t2) != 2 {
			continue
		}
		ids1, ids2 := idsOf(t1), idsOf(t2)
		softSeen[matchupKey(ids1, ids2)] = true
		// Rows arrive newest first, so the cap keeps the most recent ones.
		if len(softHistory) >= softHistoryPromptLimit {
			continue
		}
		names := func(team []TeamPlayer) []string {
			out := []string{}
			for _, t := range team {
				out = append(out, t.Name)
			}
			return out
		}
		softHistory = append(softHistory, aiHistoryMatch{Round: round, Team1: names(t1), Team2: names(t2)})
	}
	// Hand the prompt a chronological list; it reads as a timeline.
	for i, j := 0, len(softHistory)-1; i < j; i, j = i+1, j-1 {
		softHistory[i], softHistory[j] = softHistory[j], softHistory[i]
	}
	return softSeen, softHistory
}

// CreateEvent opens a match night. Without player_ids the pool defaults to
// every active player. court_count caps the court numbers usable on cards
// (0 = unlimited), max_rounds is the planned number of rounds (0 = no limit).
func (s *Service) CreateEvent(c *fiber.Ctx) error {
	var in struct {
		Name            string    `json:"name"`
		PlayerIDs       *[]string `json:"player_ids"`
		CourtCount      *int      `json:"court_count"`
		BasePlayed      *int      `json:"base_played"`
		MaxRounds       *int      `json:"max_rounds"`
		UseReferee      *bool     `json:"use_referee"`
		SourceSessionID *string   `json:"source_session_id"`
	}
	if err := httpx.Decode(c, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Event name is required.")
	}
	courts := 0
	if in.CourtCount != nil {
		if *in.CourtCount < 0 || *in.CourtCount > 99 {
			return httpx.WriteAppError(c, httpx.Unprocessable("Court count must be between 0 and 99."))
		}
		courts = *in.CourtCount
	}
	maxRounds := 0
	if in.MaxRounds != nil {
		if *in.MaxRounds < 0 || *in.MaxRounds > 99 {
			return httpx.WriteAppError(c, httpx.Unprocessable("Match limit must be between 0 and 99."))
		}
		maxRounds = *in.MaxRounds
	}
	var sourceSession *string
	if in.SourceSessionID != nil && *in.SourceSessionID != "" {
		if _, err := uuid.Parse(*in.SourceSessionID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid source session ID."))
		}
		if err := s.db.QueryRow(c.Context(), `SELECT 1 FROM mabar_sessions WHERE id = $1`, *in.SourceSessionID).Scan(new(int)); err != nil {
			return httpx.WriteAppError(c, httpx.NotFound("Source session not found."))
		}
		sourceSession = in.SourceSessionID
	}
	base := 0
	if in.BasePlayed != nil {
		if *in.BasePlayed < 0 || *in.BasePlayed > 999 {
			return httpx.WriteAppError(c, httpx.Unprocessable("Starting count must be between 0 and 999."))
		}
		base = *in.BasePlayed
	}
	ids := []string{}
	if in.PlayerIDs == nil {
		// Omitted: default to every active player. An explicitly empty
		// list is allowed too — the pool starts empty and players join
		// later via late arrivals or top-up auto-pull.
		rows, err := s.db.Query(c.Context(), `SELECT id::text FROM players WHERE status = 'ACTIVE' ORDER BY name`)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create event.")
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	} else {
		ids = *in.PlayerIDs
	}
	idsJSON, _ := json.Marshal(ids)
	useReferee := true
	if in.UseReferee != nil {
		useReferee = *in.UseReferee
	}
	var ev MatchEvent
	var poolRaw string
	err := s.db.QueryRow(c.Context(), `
		INSERT INTO match_events (id, name, court_count, base_played, max_rounds, use_referee, player_ids, source_session_id) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, use_referee, player_ids::text, source_session_id::text, created_at::text`,
		strings.TrimSpace(in.Name), courts, base, maxRounds, useReferee, string(idsJSON), sourceSession).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &ev.UseReferee, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create event.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	return httpx.OK(c, http.StatusCreated, ev)
}

func (s *Service) ListEvents(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(), `
		SELECT e.id::text, e.name, e.status, e.court_count, e.max_rounds, e.is_public, e.created_at::text,
		       COALESCE(jsonb_array_length(e.player_ids), 0),
		       COUNT(m.id), COUNT(DISTINCT m.round)
		FROM match_events e
		LEFT JOIN generated_matches m ON m.event_id = e.id
		GROUP BY e.id ORDER BY e.created_at DESC, e.id DESC`)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load events.")
	}
	defer rows.Close()
	type row struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		CourtCount int    `json:"court_count"`
		MaxRounds  int    `json:"max_rounds"`
		IsPublic   bool   `json:"is_public"`
		CreatedAt  string `json:"created_at"`
		Players    int    `json:"players"`
		Matches    int    `json:"matches"`
		Rounds     int    `json:"rounds"`
	}
	list := []row{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.ID, &t.Name, &t.Status, &t.CourtCount, &t.MaxRounds, &t.IsPublic, &t.CreatedAt, &t.Players, &t.Matches, &t.Rounds); err == nil {
			list = append(list, t)
		}
	}
	return httpx.OK(c, http.StatusOK, list)
}

// GetEvent returns the event with its matches and per-player play counts.
// Only ENDED and PLAYING matches count as played; UPCOMING is excluded.
func (s *Service) GetEvent(c *fiber.Ctx) error {
	id := c.Params("id")
	var ev MatchEvent
	var poolRaw string
	if err := s.db.QueryRow(c.Context(),
		`SELECT id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, use_referee, player_ids::text, source_session_id::text, created_at::text FROM match_events WHERE id = $1`,
		id).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &ev.UseReferee, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt); err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	matches, err := s.listMatches(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load matches.")
	}
	pool, err := s.pool(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load players.")
	}
	counts, err := s.playerCounts(c.Context(), ev, pool, matches)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load play counts.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"event": ev, "matches": matches, "counts": counts})
}

// playerCounts adds up each player's played and refereed matches and then
// applies the manual corrections. Matches only count once they are PLAYING or
// ENDED, and a correction can never push a count below zero.
func (s *Service) playerCounts(ctx context.Context, ev MatchEvent, pool []poolPlayer, matches []GenMatch) ([]PlayerCount, error) {
	played := map[string]int{}
	refereed := map[string]int{}
	for _, m := range matches {
		if m.Status != "ENDED" && m.Status != "PLAYING" {
			continue
		}
		for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
			played[t.PlayerID]++
		}
		if m.Referee != nil {
			refereed[m.Referee.PlayerID]++
		}
	}
	adjust, err := s.countAdjustments(ctx, ev.ID)
	if err != nil {
		return nil, err
	}
	caps := map[string]int{}
	if crows, err := s.db.Query(ctx,
		`SELECT player_id::text, max_played FROM match_player_caps WHERE event_id = $1`, ev.ID); err == nil {
		defer crows.Close()
		for crows.Next() {
			var pid string
			var cap int
			if err := crows.Scan(&pid, &cap); err == nil {
				caps[pid] = cap
			}
		}
	}
	arrival := map[string]int{}
	for i, pid := range ev.PlayerIDs {
		arrival[pid] = i + 1
	}
	counts := make([]PlayerCount, 0, len(pool))
	for _, p := range pool {
		a := adjust[p.ID]
		pCount := ev.BasePlayed + played[p.ID] + a.Played
		rCount := refereed[p.ID] + a.Refereed
		if pCount < 0 {
			pCount = 0
		}
		if rCount < 0 {
			rCount = 0
		}
		var maxPlayed *int
		if c, ok := caps[p.ID]; ok {
			v := c
			maxPlayed = &v
		}
		counts = append(counts, PlayerCount{
			PlayerID:  p.ID,
			Name:      p.Name,
			Grade:     p.Grade,
			Gender:    p.Gender,
			Arrival:   arrival[p.ID],
			Played:    pCount,
			Refereed:  rCount,
			MaxPlayed: maxPlayed,
		})
	}
	return counts, nil
}

type countAdjustment struct {
	Played   int
	Refereed int
}

func (s *Service) countAdjustments(ctx context.Context, eventID string) (map[string]countAdjustment, error) {
	rows, err := s.db.Query(ctx,
		`SELECT player_id::text, played_delta, refereed_delta FROM match_count_adjustments WHERE event_id = $1`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]countAdjustment{}
	for rows.Next() {
		var pid string
		var a countAdjustment
		if err := rows.Scan(&pid, &a.Played, &a.Refereed); err == nil {
			out[pid] = a
		}
	}
	return out, nil
}

// AdjustCount moves one player's played or refereed count by a delta on top of
// the counted matches, so notes can be corrected without editing match cards.
// The resulting count may never drop below zero. It also sets a personal
// play cap (max_played >= 0); omitted leaves the cap untouched.
func (s *Service) AdjustCount(c *fiber.Ctx) error {
	id := c.Params("id")
	playerID := c.Params("player_id")
	var in struct {
		PlayedDelta   int  `json:"played_delta"`
		RefereedDelta int  `json:"refereed_delta"`
		MaxPlayed     *int `json:"max_played"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	// max_played:null (clear back to event default) arrives as a nil pointer
	// just like an omitted key, so detect key presence from the raw body.
	var rawBody map[string]json.RawMessage
	hasMax := json.Unmarshal(c.Body(), &rawBody) == nil && rawBody["max_played"] != nil
	if in.PlayedDelta == 0 && in.RefereedDelta == 0 && !hasMax {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pick a change of at least 1."))
	}
	if in.PlayedDelta < -50 || in.PlayedDelta > 50 || in.RefereedDelta < -50 || in.RefereedDelta > 50 {
		return httpx.WriteAppError(c, httpx.Unprocessable("A single change may not pass 50."))
	}
	if in.MaxPlayed != nil && (*in.MaxPlayed < 0 || *in.MaxPlayed > 999) {
		return httpx.WriteAppError(c, httpx.Unprocessable("Personal max must be between 0 and 999."))
	}
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	if _, err := uuid.Parse(playerID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid player ID."))
	}
	pool, err := s.pool(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	inPool := false
	for _, p := range pool {
		if p.ID == playerID {
			inPool = true
			break
		}
	}
	if !inPool {
		return httpx.WriteAppError(c, httpx.Unprocessable("That player is not in this event."))
	}
	var ev MatchEvent
	var poolRaw string
	if err := s.db.QueryRow(c.Context(),
		`SELECT id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, use_referee, player_ids::text, source_session_id::text, created_at::text
		 FROM match_events WHERE id = $1`, id).
		Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &ev.UseReferee, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt); err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	matches, err := s.listMatches(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load matches.")
	}
	before, err := s.playerCounts(c.Context(), ev, pool, matches)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load play counts.")
	}
	current := PlayerCount{}
	found := false
	for _, pc := range before {
		if pc.PlayerID == playerID {
			current, found = pc, true
			break
		}
	}
	if !found {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player not found in this event.")
	}
	if current.Played+in.PlayedDelta < 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Played count is already 0."))
	}
	if current.Refereed+in.RefereedDelta < 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Refereed count is already 0."))
	}
	if _, err := s.db.Exec(c.Context(), `
		INSERT INTO match_count_adjustments (event_id, player_id, played_delta, refereed_delta, is_auto)
		VALUES ($1, $2, $3, $4, FALSE)
		ON CONFLICT (event_id, player_id) DO UPDATE
		SET played_delta = match_count_adjustments.played_delta + EXCLUDED.played_delta,
		    refereed_delta = match_count_adjustments.refereed_delta + EXCLUDED.refereed_delta,
		    is_auto = FALSE,
		    updated_at = now()`,
		id, playerID, in.PlayedDelta, in.RefereedDelta); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save the change.")
	}
	if in.MaxPlayed != nil {
		if _, err := s.db.Exec(c.Context(), `
			INSERT INTO match_player_caps (event_id, player_id, max_played)
			VALUES ($1, $2, $3)
			ON CONFLICT (event_id, player_id) DO UPDATE SET max_played = EXCLUDED.max_played, updated_at = now()`,
			id, playerID, *in.MaxPlayed); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save the personal max.")
		}
	} else if hasMax {
		// Explicit null: back to following the event limit.
		if _, err := s.db.Exec(c.Context(),
			`DELETE FROM match_player_caps WHERE event_id = $1 AND player_id = $2`,
			id, playerID); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not clear the personal max.")
		}
	}
	after, err := s.playerCounts(c.Context(), ev, pool, matches)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load play counts.")
	}
	for _, pc := range after {
		if pc.PlayerID == playerID {
			return httpx.OK(c, http.StatusOK, pc)
		}
	}
	return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player not found in this event.")
}

// publicEventDetail loads the read-only payload for the public live page.
// Non-public events look exactly like missing ones.
func (s *Service) publicEventDetail(ctx context.Context, id string) (MatchEvent, []GenMatch, []PlayerCount, error) {
	var ev MatchEvent
	var poolRaw string
	if err := s.db.QueryRow(ctx,
		`SELECT id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, use_referee, player_ids::text, source_session_id::text, created_at::text
		 FROM match_events WHERE id = $1 AND is_public`,
		id).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &ev.UseReferee, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt); err != nil {
		return ev, nil, nil, err
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	matches, err := s.listMatches(ctx, id)
	if err != nil {
		return ev, nil, nil, err
	}
	pool, err := s.pool(ctx, id)
	if err != nil {
		return ev, nil, nil, err
	}
	if !ev.ShowGrades {
		for i := range matches {
			for j := range matches[i].Team1 {
				matches[i].Team1[j].Grade = nil
			}
			for j := range matches[i].Team2 {
				matches[i].Team2[j].Grade = nil
			}
		}
	}
	counts, err := s.playerCounts(ctx, ev, pool, matches)
	if err != nil {
		return ev, nil, nil, err
	}
	if !ev.ShowGrades {
		for i := range counts {
			counts[i].Grade = nil
		}
	}
	// Pool ids are an admin concern; the public payload carries names only.
	ev.PlayerIDs = []string{}
	return ev, matches, counts, nil
}

// PublicEvents lists events flagged public — the live index.
func (s *Service) PublicEvents(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(), `
		SELECT e.id::text, e.name, e.created_at::text, COUNT(m.id), COUNT(DISTINCT m.round), e.max_rounds
		FROM match_events e
		LEFT JOIN generated_matches m ON m.event_id = e.id
		WHERE e.is_public
		GROUP BY e.id ORDER BY e.created_at DESC`)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load events.")
	}
	defer rows.Close()
	type row struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
		Matches   int    `json:"matches"`
		Rounds    int    `json:"rounds"`
		MaxRounds int    `json:"max_rounds"`
	}
	list := []row{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.Matches, &t.Rounds, &t.MaxRounds); err == nil {
			list = append(list, t)
		}
	}
	return httpx.OK(c, http.StatusOK, list)
}

// PublicEvent is the read-only live view of one public event.
func (s *Service) PublicEvent(c *fiber.Ctx) error {
	ev, matches, counts, err := s.publicEventDetail(c.Context(), c.Params("id"))
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"event": ev, "matches": matches, "counts": counts})
}

// genBase is everything loaded once per generate call: pool, history,
// soft guards, and play counts. API generate, prompt preview, and manual
// paste share it so all three see identical state.
type genBase struct {
	pool          []poolPlayer
	aiPlayers     []aiPlayer
	history       []aiHistoryMatch
	seen          map[string]bool
	softSeen      map[string]bool
	softHistory   []aiHistoryMatch
	playedCounts  map[string]int
	matches       []GenMatch
	wavesPerRound int
	maxRounds     int
	maxRound      int
	basePlayed    int
	useReferee    bool
	caps          map[string]int
}

// capOf resolves a player's personal cap: explicit cap wins, else the event
// limit (0 = unlimited).
func (gb *genBase) capOf(pid string) int {
	if gb == nil {
		return 0
	}
	if c, ok := gb.caps[pid]; ok {
		return c
	}
	return gb.maxRounds
}

func (s *Service) loadGenBase(ctx context.Context, id string) (*genBase, error) {
	pool, err := s.pool(ctx, id)
	if err != nil {
		return nil, &httpx.AppError{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "Event not found."}
	}
	if len(pool) < 4 {
		return nil, httpx.Unprocessable("An event needs at least 4 players for 2v2 doubles.")
	}
	gb := &genBase{pool: pool, seen: map[string]bool{}, playedCounts: map[string]int{}, caps: map[string]int{}}
	// Matches per wave = courts running simultaneously (event cap, else 3).
	gb.wavesPerRound = 3
	_ = s.db.QueryRow(ctx,
		`SELECT COALESCE(NULLIF(court_count, 0), 3), max_rounds, COALESCE(base_played, 0), COALESCE(use_referee, TRUE) FROM match_events WHERE id = $1`, id).Scan(&gb.wavesPerRound, &gb.maxRounds, &gb.basePlayed, &gb.useReferee)
	if gb.wavesPerRound <= 0 {
		gb.wavesPerRound = 3
	}
	if crows, err := s.db.Query(ctx,
		`SELECT player_id::text, max_played FROM match_player_caps WHERE event_id = $1`, id); err == nil {
		defer crows.Close()
		for crows.Next() {
			var pid string
			var cap int
			if err := crows.Scan(&pid, &cap); err == nil {
				gb.caps[pid] = cap
			}
		}
	}
	matches, err := s.listMatches(ctx, id)
	if err != nil {
		return nil, &httpx.AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Could not load history."}
	}
	gb.matches = matches
	// Counts must match what the UI shows: base_played (matches credited from
	// a previous session) plus manual adjustments, on top of the stored rows.
	// Skipping them made every cap check off by base_played — on the real
	// event a player on 5x looked like 4x, so the draw happily scheduled a
	// sixth game for someone already at their limit.
	adjust, err := s.countAdjustments(ctx, id)
	if err != nil {
		return nil, &httpx.AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Could not load play counts."}
	}
	for i, p := range pool {
		gb.aiPlayers = append(gb.aiPlayers, aiPlayer{Index: i, ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: i + 1})
	}
	for _, m := range matches {
		idsOfTeam := func(team []TeamPlayer) []string {
			out := []string{}
			for _, t := range team {
				out = append(out, t.PlayerID)
			}
			return out
		}
		gb.history = append(gb.history, aiHistoryMatch{Round: m.Round, Team1: idsOfTeam(m.Team1), Team2: idsOfTeam(m.Team2)})
		// Only cards that actually ran count toward a player's allowance, which
		// is what playerCounts and the UI show. UPCOMING rows are a plan, not a
		// game: counting them made a drawn-but-unsaved round inflate every
		// count by one and could push a 5x player to 6x.
		if m.Status == "ENDED" || m.Status == "PLAYING" {
			for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
				gb.playedCounts[t.PlayerID]++
			}
		}
		ids := []string{}
		for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
			ids = append(ids, t.PlayerID)
		}
		gb.seen[matchupKey(ids[:2], ids[2:])] = true
		if m.Round > gb.maxRound {
			gb.maxRound = m.Round
		}
	}
	// Load signal for the AI: lowest played count plays first. base_played and
	// manual adjustments count too, so this matches the Played column the UI
	// renders and the cap arithmetic below.
	for i := range gb.aiPlayers {
		id := gb.aiPlayers[i].ID
		gb.playedCounts[id] += gb.basePlayed + adjust[id].Played
		if gb.playedCounts[id] < 0 {
			gb.playedCounts[id] = 0
		}
		gb.aiPlayers[i].Played = gb.playedCounts[id]
	}
	applyCaps(gb.aiPlayers, gb.caps, gb.maxRounds)
	// Soft cross-week guard: recent events of the same period (via
	// source_session -> mabar_sessions.period_id).
	gb.softSeen = map[string]bool{}
	if periodID, ok := s.eventPeriodID(ctx, id); ok {
		gb.softSeen, gb.softHistory = s.periodRecentSoft(ctx, periodID, id)
		// Keys already covered by this event need no soft treatment.
		for k := range gb.seen {
			delete(gb.softSeen, k)
		}
	}
	return gb, nil
}

// liveSetFor collects players whose matches are still PLAYING. It scans
// every round, not just the latest one: a forgotten PLAYING status on an
// older round (e.g. a manual card added late) must still bench that player
// in the next draw, otherwise they get scheduled again immediately.
//
// A match only counts as live while it can still plausibly be running. The
// status column is not reset on its own, so a card left on PLAYING by a
// forgotten tap would otherwise bench its four players forever. A match
// older than liveMatchWindow is treated as abandoned and ignored.
func liveSetFor(matches []GenMatch) map[string]bool {
	return liveSetForAt(matches, time.Now())
}

// liveMatchWindow is how long a match may stay on PLAYING before the status
// is considered abandoned. One session evening is a few hours, so anything
// older than a day is a stale flag, not a running game.
const liveMatchWindow = 24 * time.Hour

// liveSetForAt is liveSetFor with an explicit clock, so the window can be
// tested without waiting for real time to pass.
func liveSetForAt(matches []GenMatch, now time.Time) map[string]bool {
	live := map[string]bool{}
	cutoff := now.Add(-liveMatchWindow)
	for _, m := range matches {
		if m.Status != "PLAYING" {
			continue
		}
		if at := liveAt(m); at.IsZero() || at.Before(cutoff) {
			continue
		}
		for _, id := range append(idsOf(m.Team1), idsOf(m.Team2)...) {
			live[id] = true
		}
	}
	return live
}

// parseMatchTime reads a Postgres timestamp text. The database returns
// "2026-10-18 07:30:39.315735+00", so the usual RFC3339 layout does not
// parse; several fallbacks are tried in order.
func parseMatchTime(v *string) *time.Time {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	s := strings.TrimSpace(*v)
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		// Postgres renders a timestamptz as "2026-10-18 06:26:43.473381+00",
		// and a UTC one as "…+00:00": two-digit offset without the minutes.
		// Without these the format above never matches and every timestamp
		// reads as zero, which silently emptied the rest set.
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// liveAt returns when a match actually went on court, for the live window.
// started_at is the right clock here: a card drawn on Monday and started on
// Tuesday was live on Tuesday, whatever the draw time says. It falls back to
// the draw time for a card that was never started.
func liveAt(m GenMatch) time.Time {
	if t := parseMatchTime(m.StartedAt); t != nil {
		return *t
	}
	if t := parseMatchTime(&m.CreatedAt); t != nil {
		return *t
	}
	return time.Time{}
}

// playedAt returns when a match took the court. Draw time (created_at) is the
// clock used here, not started_at: started_at is rewritten whenever a leftover
// card is started again days later, which would drag stale cards into the
// newest wave and bench players who never played that evening.
func playedAt(m GenMatch) time.Time {
	if t := parseMatchTime(&m.CreatedAt); t != nil {
		return *t
	}
	if t := parseMatchTime(m.StartedAt); t != nil {
		return *t
	}
	return time.Time{}
}

// wasPlayed reports whether the card already took a court.
func wasPlayed(m GenMatch) bool {
	return m.Status == "ENDED" || m.Status == "PLAYING"
}

// lastWave rest returns the cards of the most recent wave: one wave is
// courtCount matches, because the courts of a session run side by side.
// Ordering runs on draw time, so a wave is picked by what was drawn last
// instead of by round numbers, which are only display labels and may be
// sparse, out of order, or full of leftover single cards.
func lastWave(matches []GenMatch, roundNo, courtCount int) []GenMatch {
	if courtCount <= 0 {
		courtCount = 3
	}
	played := make([]GenMatch, 0, len(matches))
	for _, m := range matches {
		if !wasPlayed(m) || m.Round >= roundNo {
			continue
		}
		if playedAt(m).IsZero() {
			continue
		}
		played = append(played, m)
	}
	sort.SliceStable(played, func(i, j int) bool {
		ti, tj := playedAt(played[i]), playedAt(played[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if played[i].Round != played[j].Round {
			return played[i].Round > played[j].Round
		}
		return played[i].Wave > played[j].Wave
	})
	if len(played) > courtCount {
		played = played[:courtCount]
	}
	return played
}

// lastPendingWave returns the newest wave drawn earlier in this same call:
// the highest round, then the highest wave inside it.
func lastPendingWave(pendings []pending, roundNo int) (round, wave int, found bool) {
	for _, p := range pendings {
		if p.round >= roundNo {
			continue
		}
		if !found || p.round > round || (p.round == round && p.wave > wave) {
			round, wave, found = p.round, p.wave, true
		}
	}
	return round, wave, found
}

// restSetFor collects the players from the last wave so the next round lets
// them rest first.
//
// It reads chronology instead of round-relative numbers: round is a display
// label and may be sparse (a deleted round leaves a hole) or hold leftover
// single cards, so keying off roundNo-1 hides exactly the players who need
// the break — someone who played the last wave of round 1 while round 3
// exists was invisible to a round-relative window.
//
// A wave is at most courtCount matches, matching how many courts run at once.
// Rounds drawn earlier in this same call count as the newest wave, since a
// generated round must not be followed immediately by its own players.
// Live players (still PLAYING) are merged in by the caller.
func restSetFor(matches []GenMatch, pendings []pending, roundNo, courtCount int) map[string]bool {
	return restSetForAt(matches, pendings, roundNo, courtCount, time.Now())
}

// restSetForAt is restSetFor with an explicit clock. The clock is carried for
// symmetry with liveSetFor, but it is not used to expire the set: a rest set
// answers "who came off the court last", and that stays true however long the
// event has been open. The 24-hour window belongs to liveSetFor only, where it
// means "a PLAYING flag nobody ever cleared". Mixing the two would empty the
// set on a resumed event, letting the last wave out again the moment the
// session is picked back up.
func restSetForAt(matches []GenMatch, pendings []pending, roundNo, courtCount int, _ time.Time) map[string]bool {
	rest := map[string]bool{}
	if roundNo <= 1 {
		return rest
	}
	if pRound, pWave, ok := lastPendingWave(pendings, roundNo); ok {
		for _, p := range pendings {
			if p.round != pRound || p.wave != pWave {
				continue
			}
			for _, id := range append(idsOf(p.team1), idsOf(p.team2)...) {
				rest[id] = true
			}
		}
		return rest
	}
	for _, m := range lastWave(matches, roundNo, courtCount) {
		for _, id := range append(idsOf(m.Team1), idsOf(m.Team2)...) {
			rest[id] = true
		}
	}
	return rest
}

// Generate strategies decide how a draw treats players who are still on court.
//
// strategyLeveling keeps them in the pool (bench-first), so an odd pool can
// still fill its cards and play counts keep levelling up.
// strategyRest drops them entirely: nobody is scheduled while their match is
// still PLAYING. Fewer cards is acceptable, an incomplete card is not.
const (
	strategyLeveling = "leveling"
	strategyRest     = "rest"
)

// normalizeStrategy returns the canonical strategy name, or "" when the value
// is not one of the supported names. An empty request means leveling, the
// behaviour every existing caller expects.
func normalizeStrategy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", strategyLeveling, "level":
		return strategyLeveling
	case strategyRest, "istirahat":
		return strategyRest
	}
	return ""
}

// dropBench removes players the resting strategy must not schedule: anyone
// whose match is still PLAYING, anyone who played the last wave, and anyone
// who has no allowance left. All three would otherwise start a game the mode
// is supposed to prevent — a second game in a row, or a sixth game for a
// player whose limit is five. Leveling keeps the first two in the pool and
// merely benches them first, which is why an odd pool there can still fill
// every court; at-cap players stay visible there so coverage can force them in
// when bodies run short.
func dropBench(players []aiPlayer, bench map[string]bool, strategy string) []aiPlayer {
	if strategy != strategyRest || len(bench) == 0 {
		return players
	}
	out := make([]aiPlayer, 0, len(players))
	for _, p := range players {
		if !bench[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// cappedSet collects players with no allowance left: an explicit personal cap
// wins, else the event limit. Everyone in it would break their own limit by
// playing one more game, so no draw may schedule them.
func cappedSet(players []aiPlayer, played map[string]int, caps map[string]int, eventMax int) map[string]bool {
	out := map[string]bool{}
	for _, p := range players {
		if lim := effCap(caps, eventMax, p.ID); lim > 0 && played[p.ID] >= lim {
			out[p.ID] = true
		}
	}
	return out
}

// enforceCaps drops players who have already used their allowance. A cap is a
// hard limit: a player on 5x with max_rounds 5 must never be offered to the
// model, or it schedules them for a sixth game — which is exactly what happened
// on the real event, where four capped players were drawn while Hari (2x) and
// Ivan (3x) sat out.
//
// Capped players are still reachable when the remaining group cannot fill a
// single 2v2: then the pool is returned untouched and allowancePick (or the
// coverage rule) borrows the minimum needed. Fewer than four under-cap players
// means there is no legal card without a borrow.
func enforceCaps(players []aiPlayer, played map[string]int, caps map[string]int, eventMax int) []aiPlayer {
	capped := cappedSet(players, played, caps, eventMax)
	if len(capped) == 0 {
		return players
	}
	under := make([]aiPlayer, 0, len(players))
	for _, p := range players {
		if !capped[p.ID] {
			under = append(under, p)
		}
	}
	if len(under) < 4 {
		return players
	}
	return under
}

// requireDrawable rejects a draw that cannot fill a single 2v2.
func requireDrawable(n int, strategy string) error {
	if n >= 4 {
		return nil
	}
	if strategy == strategyRest {
		return httpx.Unprocessable("Mode istirahat: cuma " + strconv.Itoa(n) +
			" pemain yang sudah turun dari lapangan. Selesaikan kartu yang berjalan dulu, atau pakai mode leveling.")
	}
	return httpx.Unprocessable("Not enough players to build a 2v2 matchup.")
}

// orderPendingsByLoad applies the tired-first/load-first wave order to
// locally drawn rounds (the AI-answer path is ordered before validating).
// Waves are renumbered after sorting and referees re-picked for the new
// waves, so the least-played run first everywhere.
func orderPendingsByLoad(pend []pending, rest map[string]bool, played map[string]int, live map[string]bool, waveBase, wavesPerRound int, pool []poolPlayer, useReferee bool) []pending {
	if len(pend) < 2 {
		return pend
	}
	restN := func(p pending) int {
		n := 0
		for _, id := range append(idsOf(p.team1), idsOf(p.team2)...) {
			if rest[id] {
				n++
			}
		}
		return n
	}
	minPlayed := func(p pending) int {
		best := -1
		for _, id := range append(idsOf(p.team1), idsOf(p.team2)...) {
			if c := played[id]; best < 0 || c < best {
				best = c
			}
		}
		if best < 0 {
			return 0
		}
		return best
	}
	hasLive := func(p pending) bool {
		for _, id := range append(idsOf(p.team1), idsOf(p.team2)...) {
			if live[id] {
				return true
			}
		}
		return false
	}
	out := append([]pending{}, pend...)
	sort.SliceStable(out, func(i, j int) bool {
		if li, lj := hasLive(out[i]), hasLive(out[j]); li != lj {
			return !li
		}
		if a, b := minPlayed(out[i]), minPlayed(out[j]); a != b {
			return a < b
		}
		return restN(out[i]) < restN(out[j])
	})
	waves := map[int]int{}
	for i := range out {
		out[i].wave = waveBase + i/wavesPerRound + 1
		out[i].referee = nil
		waves[i] = out[i].wave
	}
	if useReferee {
		assignReferees(pool, out, waves, played)
	}
	return out
}

// orderMatchesByRest stable-sorts AI-answer matchups: sides holding a
// live player always run last (they need rest after their current game),
// then the lowest play counts run first, then the well-rested ones.
func orderMatchesByRest(ms []aiMatchup, rest map[string]bool, played map[string]int, live map[string]bool) []aiMatchup {
	if len(ms) < 2 {
		return ms
	}
	count := func(mu aiMatchup) int {
		n := 0
		for _, id := range append(append([]string{}, mu.Team1...), mu.Team2...) {
			if rest[id] {
				n++
			}
		}
		return n
	}
	minPlayed := func(mu aiMatchup) int {
		best := -1
		for _, id := range append(append([]string{}, mu.Team1...), mu.Team2...) {
			if c := played[id]; best < 0 || c < best {
				best = c
			}
		}
		if best < 0 {
			return 0
		}
		return best
	}
	hasLive := func(mu aiMatchup) bool {
		for _, id := range append(append([]string{}, mu.Team1...), mu.Team2...) {
			if live[id] {
				return true
			}
		}
		return false
	}
	out := append([]aiMatchup{}, ms...)
	sort.SliceStable(out, func(i, j int) bool {
		if li, lj := hasLive(out[i]), hasLive(out[j]); li != lj {
			return !li
		}
		if mi, mj := minPlayed(out[i]), minPlayed(out[j]); mi != mj {
			return mi < mj
		}
		return count(out[i]) < count(out[j])
	})
	return out
}

// playedSubset builds the AI player subset for the "generate by play count"
// mode: everyone sitting exactly at cap is required, completed toward a
// multiple of 4 by borrowing upward (cap+1, cap+2, …), lowest count then
// earliest arrival first. When nobody is left to borrow, the leftovers sit
// out by arrival rotation — the standard coverage and leveling rules below
// still decide who finally plays, so a zero never sits for a higher count.
func playedSubset(pool []poolPlayer, counts []PlayerCount, cap int) (sub []aiPlayer, required []string, err error) {
	playedOf := map[string]int{}
	posOf := map[string]int{}
	for i, p := range pool {
		posOf[p.ID] = i
	}
	for _, c := range counts {
		playedOf[c.PlayerID] = c.Played
	}
	for _, p := range pool {
		if playedOf[p.ID] == cap {
			required = append(required, p.ID)
		}
	}
	if len(required) == 0 {
		return nil, nil, httpx.Unprocessable("Tidak ada pemain dengan jumlah main " + strconv.Itoa(cap) + ".")
	}
	rest := []poolPlayer{}
	inReq := map[string]bool{}
	for _, id := range required {
		inReq[id] = true
	}
	rankOf := map[string]int{}
	avgRank := 0
	for _, p := range pool {
		rankOf[p.ID] = gradeRank(p.Grade)
	}
	for _, id := range required {
		avgRank += rankOf[id]
	}
	if len(required) > 0 {
		avgRank /= len(required)
	}
	for _, p := range pool {
		if !inReq[p.ID] {
			rest = append(rest, p)
		}
	}
	// Borrow upward level by level; within a level prefer the grade closest
	// to the required group's average so the 2v2 stays balanced.
	sort.Slice(rest, func(i, j int) bool {
		if playedOf[rest[i].ID] != playedOf[rest[j].ID] {
			return playedOf[rest[i].ID] < playedOf[rest[j].ID]
		}
		di := rankOf[rest[i].ID] - avgRank
		if di < 0 {
			di = -di
		}
		dj := rankOf[rest[j].ID] - avgRank
		if dj < 0 {
			dj = -dj
		}
		if di != dj {
			return di < dj
		}
		return posOf[rest[i].ID] < posOf[rest[j].ID]
	})
	need := (4 - len(required)%4) % 4
	if need > len(rest) {
		need = len(rest)
	}
	borrowed := []string{}
	for _, p := range rest[:need] {
		borrowed = append(borrowed, p.ID)
	}
	for _, id := range append(append([]string{}, required...), borrowed...) {
		p := pool[posOf[id]]
		sub = append(sub, aiPlayer{Index: len(sub), ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: posOf[id] + 1, Played: playedOf[id]})
	}
	return sub, required, nil
}

// loadCounts reads display play counts (base + ended/playing + manual
// corrections) shared by the play-count filter modes.
func (s *Service) loadCounts(ctx context.Context, id string, pool []poolPlayer, matches []GenMatch) ([]PlayerCount, error) {
	var base int
	var idsRaw string
	var evIDs []string
	if err := s.db.QueryRow(ctx,
		`SELECT COALESCE(base_played, 0), player_ids::text FROM match_events WHERE id = $1`, id).Scan(&base, &idsRaw); err != nil {
		return nil, &httpx.AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Could not load event."}
	}
	_ = json.Unmarshal([]byte(idsRaw), &evIDs)
	counts, err := s.playerCounts(ctx, MatchEvent{ID: id, BasePlayed: base, PlayerIDs: evIDs}, pool, matches)
	if err != nil {
		return nil, &httpx.AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Could not load play counts."}
	}
	return counts, nil
}

// playedCapSubset loads display play counts and builds the filtered AI
// subset for the "generate by play count" mode. Shared by API generate,
// prompt preview, and manual paste so all three filter identically.
func (s *Service) playedCapSubset(ctx context.Context, id string, pool []poolPlayer, matches []GenMatch, cap int) ([]aiPlayer, error) {
	if cap < 0 {
		return nil, httpx.Unprocessable("Jumlah main tidak valid.")
	}
	counts, err := s.loadCounts(ctx, id, pool, matches)
	if err != nil {
		return nil, err
	}
	sub, _, err := playedSubset(pool, counts, cap)
	return sub, err
}

// catchupSubset builds the "even out first" subset: every pool member below
// their personal cap (explicit wins, else the target) plays, nobody at or
// above it does. No borrowing — leftovers sit by arrival rotation and catch
// up next time.
func catchupSubset(pool []poolPlayer, counts []PlayerCount, target int, caps map[string]int) ([]aiPlayer, error) {
	playedOf := map[string]int{}
	posOf := map[string]int{}
	for i, p := range pool {
		posOf[p.ID] = i
	}
	for _, c := range counts {
		playedOf[c.PlayerID] = c.Played
	}
	effOf := func(pid string) int {
		if c, ok := caps[pid]; ok {
			return c
		}
		return target
	}
	sub := []aiPlayer{}
	for _, p := range pool {
		cap := effOf(p.ID)
		if cap > 0 && playedOf[p.ID] >= cap {
			continue
		}
		sub = append(sub, aiPlayer{Index: len(sub), ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: posOf[p.ID] + 1, Played: playedOf[p.ID]})
	}
	if len(sub) == 0 {
		return nil, httpx.Unprocessable("Semua pemain sudah rata di " + strconv.Itoa(target) + ". Naikkan batas match untuk lanjut.")
	}
	if len(sub) < 4 {
		return nil, httpx.Unprocessable("Hanya " + strconv.Itoa(len(sub)) + " pemain di bawah target — butuh minimal 4 untuk 2v2.")
	}
	return sub, nil
}

// catchupPlayers builds the even-out subset for all three flows: everyone
// below target (maxRounds, else the pool max) plays, nobody at/above it.
func (s *Service) catchupPlayers(ctx context.Context, id string, gb *genBase) ([]aiPlayer, error) {
	counts, err := s.loadCounts(ctx, id, gb.pool, gb.matches)
	if err != nil {
		return nil, err
	}
	target := gb.maxRounds
	if target <= 0 {
		for _, cc := range counts {
			if cc.Played > target {
				target = cc.Played
			}
		}
	}
	return catchupSubset(gb.pool, counts, target, gb.caps)
}

// allowancePick chooses who plays once the planned rounds are spent. It walks
// the play counts upward from the lowest and stops as soon as a full card can
// be filled, so the short-changed players catch up before anyone above them
// plays again: with four players on 3x and the rest already on 5x, exactly
// those four play — nobody's allowance is raised just because other people
// have more games. A level that spills past a card leaves its extras on the
// bench, which self-corrects because their count is then the lowest and they
// lead the next draw. Only when fewer than four players sit below their cap at
// all does the card borrow from the capped ones, nearest to their limit first.
//
// The result keeps arrival order: the prompt lists players by position and the
// late-arrival rule reads the same index.
func allowancePick(pool []poolPlayer, playedOf map[string]int, caps map[string]int, eventMax int) ([]poolPlayer, error) {
	posOf := map[string]int{}
	for i, p := range pool {
		posOf[p.ID] = i
	}

	under := []poolPlayer{}
	capped := []poolPlayer{}
	for _, p := range pool {
		lim := effCap(caps, eventMax, p.ID)
		if lim > 0 && playedOf[p.ID] >= lim {
			capped = append(capped, p)
			continue
		}
		under = append(under, p)
	}
	if len(under) == 0 {
		return nil, httpx.Unprocessable(
			"Semua pemain sudah mencapai batas main (" + strconv.Itoa(eventMax) + "x). Naikkan batas match di pengaturan event untuk lanjut.")
	}

	// Fewest games first, so the draw opens with whoever is behind.
	sort.SliceStable(under, func(i, j int) bool {
		if playedOf[under[i].ID] != playedOf[under[j].ID] {
			return playedOf[under[i].ID] < playedOf[under[j].ID]
		}
		return posOf[under[i].ID] < posOf[under[j].ID]
	})

	// Walk up the count levels until a card can be filled, then trim to whole
	// cards. The lowest level always goes in first; only when it cannot fill a
	// card alone is the next level pulled in, and only as far as the card
	// needs. Extras left over from a level sit out and lead the next draw,
	// because their count is then the lowest.
	take := 0
	for take < len(under) && take < 4 {
		lvl := playedOf[under[take].ID]
		for take < len(under) && playedOf[under[take].ID] == lvl {
			take++
		}
	}
	picked := under[:take]
	if cards := take / 4; cards > 0 {
		picked = under[:cards*4]
	} else {
		// Too few players below the cap to fill a card: borrow the minimum
		// from capped players so the low ones still get on court.
		sort.SliceStable(capped, func(i, j int) bool {
			if playedOf[capped[i].ID] != playedOf[capped[j].ID] {
				return playedOf[capped[i].ID] < playedOf[capped[j].ID]
			}
			return posOf[capped[i].ID] < posOf[capped[j].ID]
		})
		need := 4 - len(picked)
		if need > len(capped) {
			need = len(capped)
		}
		picked = append(append([]poolPlayer{}, picked...), capped[:need]...)
	}
	picked = append([]poolPlayer{}, picked...)
	sort.SliceStable(picked, func(i, j int) bool {
		return posOf[picked[i].ID] < posOf[picked[j].ID]
	})
	return picked, nil
}

// allowancePlayers builds the draw pool once an event's planned rounds are
// used up. max_rounds is an allowance per player, not a wave count: a
// 29-player night spends five waves inside its first round, so the round label
// runs out long before the players do.
//
// It returns the pool and how many cards that pool can support.
func (s *Service) allowancePlayers(ctx context.Context, id string, gb *genBase) ([]aiPlayer, int, error) {
	counts, err := s.loadCounts(ctx, id, gb.pool, gb.matches)
	if err != nil {
		return nil, 0, err
	}
	playedOf := map[string]int{}
	for _, c := range counts {
		playedOf[c.PlayerID] = c.Played
	}

	picked, err := allowancePick(gb.pool, playedOf, gb.caps, gb.maxRounds)
	if err != nil {
		return nil, 0, err
	}

	sub := make([]aiPlayer, 0, len(picked))
	for i, p := range picked {
		sub = append(sub, aiPlayer{
			Index: i, ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender,
			Rank: gradeRank(p.Grade), Arrival: i + 1, Played: playedOf[p.ID],
		})
	}
	if len(sub) < 4 {
		return nil, 0, httpx.Unprocessable(
			"Hanya " + strconv.Itoa(len(sub)) + " pemain tersisa di bawah batas main — butuh minimal 4 untuk 2v2.")
	}
	return sub, len(sub) / 4, nil
}

// applyCaps stamps the effective personal cap (explicit wins, else event
// limit, 0 = unlimited) onto AI players for the prompt.
func applyCaps(players []aiPlayer, caps map[string]int, eventMax int) {
	for i := range players {
		if c, ok := caps[players[i].ID]; ok {
			players[i].Cap = c
		} else {
			players[i].Cap = eventMax
		}
	}
}

// genValidator carries per-round validation state. seen is shared by
// reference so accepted matchups join the no-repeat set immediately.
type genValidator struct {
	pool             []poolPlayer
	players          []aiPlayer
	borrowAllow      map[string]bool
	requireNewcomers []string
	seen             map[string]bool
	softSeen         map[string]bool
	rest             map[string]bool
	coverageN        int
	wavesPerRound    int
	playedCounts     map[string]int
	useReferee       bool
	caps             map[string]int
	eventMax         int
}

// effCap resolves a personal cap: explicit wins, else the event limit
// (0 = unlimited, represented as a huge allowance).
func effCap(caps map[string]int, eventMax int, pid string) int {
	if c, ok := caps[pid]; ok {
		return c
	}
	return eventMax
}

func remAfter(caps map[string]int, eventMax int, played map[string]int, pid string, playingNow bool) int {
	cap := effCap(caps, eventMax, pid)
	if cap <= 0 {
		return 1 << 30
	}
	n := played[pid]
	if playingNow {
		n++
	}
	return cap - n
}

// validateGenRound checks one AI round: 2v2, pool members, mixed-doubles
// rule, one appearance per player, no repeat, and FULL coverage. Waves
// chunk the returned order by courts that can run simultaneously; the
// referee of every match is picked afterwards from the same pool.
// strictSoft also rejects last week's final round(s) from the same
// period; lenient mode (retry) allows repeating them when forced.
func (s *Service) validateGenRound(v *genValidator, ms []aiMatchup, roundNo, waveBase int, strictSoft bool) ([]pending, error) {
	used := map[string]int{}
	out := []pending{}
	waves := map[int]int{}
	for idx := range ms {
		waves[idx] = waveBase + idx/v.wavesPerRound + 1
	}
	for _, mu := range ms {
		if len(mu.Team1) != 2 || len(mu.Team2) != 2 {
			return nil, httpx.Unprocessable("AI returned a non-2v2 matchup. Try generating again.")
		}
		t1, t2, err := s.resolveTeams(v.pool, mu.Team1, mu.Team2)
		if err != nil {
			return nil, httpx.Unprocessable("AI used players outside this event (" + err.Error() + "). Try generating again.")
		}
		for _, pid := range append(append([]string{}, mu.Team1...), mu.Team2...) {
			used[pid]++
			if used[pid] > 1 && !v.borrowAllow[pid] {
				return nil, httpx.Unprocessable("AI listed a player twice in one round. Try generating again.")
			}
			if used[pid] > 2 {
				return nil, httpx.Unprocessable("AI listed a player three times in one round. Try generating again.")
			}
		}
		key := matchupKey(mu.Team1, mu.Team2)
		if v.seen[key] {
			return nil, httpx.Unprocessable("AI repeated a previous matchup. Try generating again.")
		}
		if strictSoft && v.softSeen[key] {
			return nil, httpx.Unprocessable("AI repeated last week's final round in the same period. Try generating again.")
		}
		out = append(out, pending{round: roundNo, wave: waves[len(out)], team1: t1, team2: t2})
	}
	for _, req := range v.requireNewcomers {
		if used[req] == 0 {
			return nil, httpx.Unprocessable("AI left out a late arrival. Try generating again.")
		}
	}
	n := v.coverageN
	if len(v.requireNewcomers) == 0 && (len(used) == 0 || len(used)%4 != 0 || len(used) < n-3) {
		return nil, httpx.Unprocessable(
			"AI covered only part of the pool. Try generating again.")
	}
	if len(v.requireNewcomers) > 0 && len(used)%4 != 0 {
		return nil, httpx.Unprocessable("AI made an incomplete top-up group. Try generating again.")
	}
	// Leveling (non-topup only): nobody sits out while someone who played
	// more takes the round. With odd pools someone must go ahead first, but
	// a lower-played player may never be benched for a higher-played one.
	if len(v.requireNewcomers) == 0 && len(v.players) > 0 {
		maxPlaying := -1
		for _, ap := range v.players {
			if used[ap.ID] > 0 && v.playedCounts[ap.ID] > maxPlaying {
				maxPlaying = v.playedCounts[ap.ID]
			}
		}
		if maxPlaying >= 0 {
			for _, ap := range v.players {
				if used[ap.ID] == 0 && v.playedCounts[ap.ID] < maxPlaying {
					return nil, httpx.Unprocessable(ap.Name + " played " + strconv.Itoa(v.playedCounts[ap.ID]) + "x but sat out while others with more games played. Level play counts first (lowest plays).")
				}
			}
			// Rest priority (equal counts): a rested player never sits out
			// for someone coming straight off the previous round's last wave.
			for _, ap := range v.players {
				if used[ap.ID] != 0 || v.playedCounts[ap.ID] != maxPlaying || v.rest[ap.ID] {
					continue
				}
				for _, bp := range v.players {
					if used[bp.ID] > 0 && v.playedCounts[bp.ID] == maxPlaying && v.rest[bp.ID] {
						return nil, httpx.Unprocessable(bp.Name + " just played the last wave with no break — rest them before " + ap.Name + ".")
					}
				}
			}
			// Personal caps: a player with allowance left never sits out for
			// one already at their cap. (Coverage may still force capped
			// players in when bodies run short.)
			for _, ap := range v.players {
				if used[ap.ID] != 0 || remAfter(v.caps, v.eventMax, v.playedCounts, ap.ID, false) <= 0 {
					continue
				}
				for _, bp := range v.players {
					if used[bp.ID] > 0 && remAfter(v.caps, v.eventMax, v.playedCounts, bp.ID, true) <= 0 {
						return nil, httpx.Unprocessable(bp.Name + " sudah mencapai batas main — beri ke " + ap.Name + " yang masih punya jatah.")
					}
				}
			}
		}
	}
	for _, p := range out {
		v.seen[matchupKey(idsOf(p.team1), idsOf(p.team2))] = true
	}
	if v.useReferee {
		assignReferees(v.pool, out, waves, v.playedCounts)
	}
	return out, nil
}

// savePendings stores validated matchups as UPCOMING in one transaction.
func (s *Service) savePendings(ctx context.Context, id string, pendings []pending) ([]GenMatch, error) {
	fail := &httpx.AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Could not save matchups."}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fail
	}
	defer tx.Rollback(ctx)
	saved := []GenMatch{}
	for _, p := range pendings {
		var m GenMatch
		var t1, t2 string
		err := tx.QueryRow(ctx, `
			INSERT INTO generated_matches (id, event_id, round, wave, team1, team2, referee_id, status)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'UPCOMING')
			RETURNING id::text, event_id::text, round, wave, team1::text, team2::text, status, court,
			          started_at::text, ended_at::text, shuttlecock_used, created_at::text, updated_at::text`,
			id, p.round, p.wave, mustTeamJSON(p.team1), mustTeamJSON(p.team2), p.referee,
		).Scan(&m.ID, &m.EventID, &m.Round, &m.Wave, &t1, &t2, &m.Status, &m.Court,
			&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt)
		if err != nil {
			return nil, fail
		}
		m.Team1 = parseTeam(t1)
		m.Team2 = parseTeam(t2)
		if p.referee != nil {
			var refName string
			_ = tx.QueryRow(ctx, `SELECT name FROM players WHERE id = $1`, *p.referee).Scan(&refName)
			m.Referee = &Referee{PlayerID: *p.referee, Name: refName}
		}
		saved = append(saved, m)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fail
	}
	return saved, nil
}

// Generate asks the AI for balanced rounds, validates every matchup (2v2,
// pool members, one appearance per player per round, no repeat of any
// previous matchup in this event), and stores them as UPCOMING.
func (s *Service) Generate(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Rounds    int    `json:"rounds"`
		Round     int    `json:"round"`
		TopUp     bool   `json:"topup"`
		PlayedCap *int   `json:"played_cap"`
		CatchUp   bool   `json:"catchup"`
		Strategy  string `json:"strategy"`
	}
	_ = httpx.Decode(c, &in)
	strategy := normalizeStrategy(in.Strategy)
	if strategy == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Strategy tidak dikenal. Pakai \"leveling\" atau \"rest\"."))
	}
	restStrategy := strategy == strategyRest
	if in.Rounds <= 0 {
		in.Rounds = 1
	}
	if in.Rounds > 5 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Generate at most 5 rounds at a time."))
	}
	// Filter modes are exclusive and single-round: top-up fills an existing
	// round, played_cap targets one level, catch-up evens out below target.
	modes := 0
	if in.TopUp {
		modes++
	}
	if in.PlayedCap != nil {
		modes++
	}
	if in.CatchUp {
		modes++
	}
	if modes > 1 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pilih salah satu: susulan, filter jumlah main, atau perataan."))
	}
	if (in.PlayedCap != nil || in.CatchUp) && in.Rounds > 1 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Filter dan perataan hanya satu ronde per generate."))
	}
	if in.PlayedCap != nil || in.CatchUp {
		in.Rounds = 1
	}
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	gb, err := s.loadGenBase(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	pool, aiPlayers, history := gb.pool, gb.aiPlayers, gb.history
	seen, softSeen, softHistory := gb.seen, gb.softSeen, gb.softHistory
	playedCounts, matches := gb.playedCounts, gb.matches
	wavesPerRound, maxRounds, maxRound := gb.wavesPerRound, gb.maxRounds, gb.maxRound
	// Filtered mode swaps the whole pool for the chosen play-count subset
	// (required level + borrowed top-up), resolved against the same history.
	if in.PlayedCap != nil {
		sub, err := s.playedCapSubset(c.Context(), id, pool, matches, *in.PlayedCap)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		aiPlayers = sub
	}
	// Catch-up mode: only players below target play, nobody at/above it.
	if in.CatchUp {
		sub, err := s.catchupPlayers(c.Context(), id, gb)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		aiPlayers = sub
	}
	// max_rounds is a per-player allowance, not a wave budget. A 29-player
	// night burns five waves inside round 1, so the round counter runs out
	// while players still owe matches. Once the planned rounds are spent, keep
	// drawing for whoever is under their cap and top the last card up from
	// capped players when the leftovers cannot make a 2v2 alone.
	coverageN := len(pool)
	// coverageDefault is what every iteration resets coverage to: the whole
	// pool normally, the filtered subset in cap/catch-up mode, and the
	// allowance pool once the planned rounds are spent. Without this the loop
	// reset it to len(pool) and the allowance validator demanded full coverage
	// from a deliberately partial pool.
	coverageDefault := len(pool)
	allowance := false
	if in.Round > 0 && maxRounds > 0 && in.Round > maxRounds && !in.CatchUp {
		return httpx.WriteAppError(c, httpx.Unprocessable(
			"Round "+strconv.Itoa(in.Round)+" is past this event's match limit ("+strconv.Itoa(maxRounds)+"). Raise the limit in event settings first."))
	}
	if in.Round == 0 && maxRounds > 0 && maxRound >= maxRounds && !in.CatchUp && !in.TopUp {
		allowance = true
		in.Rounds = 1
	}
	if allowance {
		sub, cards, err := s.allowancePlayers(c.Context(), id, gb)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		aiPlayers = sub
		coverageN = len(sub)
		coverageDefault = len(sub)
		if in.Rounds > cards {
			in.Rounds = cards
		}
	}
	// Resting strategy: players still on court, and players from the last
	// wave, are not scheduled at all — that is the whole point of the mode.
	// Leveling keeps them in the pool and only benches them first.
	if restStrategy {
		bench := liveSetFor(matches)
		for id := range restSetFor(matches, nil, maxRound+1, gb.wavesPerRound) {
			bench[id] = true
		}
		aiPlayers = dropBench(aiPlayers, bench, strategy)
		aiPlayers = enforceCaps(aiPlayers, playedCounts, gb.caps, gb.maxRounds)
		if !allowance {
			coverageN, coverageDefault = len(aiPlayers), len(aiPlayers)
		}
		if err := requireDrawable(len(aiPlayers), strategy); err != nil {
			return httpx.WriteAppError(c, err)
		}
	} else {
		// Leveling also never schedules a capped player while somebody is
		// still short of their allowance: the whole mode exists to even the
		// counts out, and a sixth game for a 5x player is the opposite.
		aiPlayers = enforceCaps(aiPlayers, playedCounts, gb.caps, gb.maxRounds)
		if !allowance {
			coverageN, coverageDefault = len(aiPlayers), len(aiPlayers)
		}
	}

	// One AI call per round: small responses stay valid JSON, and every
	// earlier round (existing or generated just now) joins the history of the
	// next call, so nothing repeats. Nothing is stored until all rounds
	// validate — existing rounds are never touched.
	pendings := []pending{}
	// borrowAllow marks round players borrowed to complete a top-up group
	// (they may appear twice); requireNewcomers must all play.
	borrowAllow := map[string]bool{}
	requireNewcomers := []string{}
	// aiUnavailable latches once ALL providers fail so the remaining rounds
	// are drawn locally instead of waiting on throttled calls.
	aiUnavailable := false
	// usedAI tracks which endpoint produced the draw, shown in the loading
	// UI ("Berhasil diproses AI 2"). usedFallback is true when any round
	// came from AI 2/3, usedLocal when the deterministic draw was used.
	usedAI := aiEndpoint{}
	usedAISet := false
	usedFallback := false
	usedLocal := false
	// aiErrMsg keeps the first AI failure message so the UI can show WHY the
	// draw fell back (e.g. "AI provider HTTP 401: Invalid API Key") instead
	// of silently using the local draw.
	aiErrMsg := ""

	// Shared validation state; per-round fields (pool, borrowAllow,
	// requireNewcomers, coverageN) are synced at the top of each iteration
	// because top-up can grow the pool mid-loop.
	v := &genValidator{seen: seen, softSeen: softSeen, wavesPerRound: wavesPerRound, playedCounts: playedCounts, useReferee: gb.useReferee, caps: gb.caps, eventMax: gb.maxRounds}
	// playerBase is the pool before any per-round benching: each iteration in
	// resting mode re-filters from here, so round 2 of a multi-round request
	// also skips the wave drawn in round 1.
	playerBase := aiPlayers
	// Live players (still PLAYING, in any round) rest in every round
	// prepared while their games run. Static per request: statuses here
	// never change mid-loop (new rounds are stored UPCOMING).
	liveSet := liveSetFor(matches)

	for i := 0; i < in.Rounds; i++ {
		// Explicit round number fills one empty round (e.g. after deleting
		// it); otherwise rounds append after the highest existing one.
		// TopUp adds matches for pool members missing from a NON-empty round
		// (late arrivals) without touching its existing matches.
		roundNo := maxRound + i + 1
		coverageN = coverageDefault
		waveBase := 0
		borrowAllow = map[string]bool{}
		requireNewcomers = []string{}
		if in.Round > 0 {
			if in.Rounds > 1 {
				return httpx.WriteAppError(c, httpx.Unprocessable("Generate one round at a time when targeting a round number."))
			}
			if in.Round < 1 || in.Round > 99 {
				return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid round number."))
			}
			var exists bool
			if err := s.db.QueryRow(c.Context(),
				`SELECT EXISTS (SELECT 1 FROM generated_matches WHERE event_id = $1 AND round = $2)`,
				id, in.Round).Scan(&exists); err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate.")
			}
			if in.TopUp {
				if !exists {
					return httpx.WriteAppError(c, httpx.Unprocessable("That round is empty. Generate it normally instead."))
				}
				// Appended matches continue the wave sequence of the round.
				existing := 0
				for _, m := range matches {
					if m.Round == in.Round {
						existing++
					}
				}
				waveBase = existing / wavesPerRound
				// Auto-pull: anyone PRESENT in the source session joins the
				// pool first, so no manual picking is needed.
				var source *string
				if err := s.db.QueryRow(c.Context(),
					`SELECT source_session_id::text FROM match_events WHERE id = $1`, id).Scan(&source); err == nil && source != nil {
					rows, err := s.db.Query(c.Context(), `
						SELECT a.player_id::text
						FROM attendances a
						JOIN players pl ON pl.id = a.player_id
						WHERE a.session_id = $1 AND a.status IN ('PRESENT', 'LISTED', 'CONFIRMED')
						  AND pl.status = 'ACTIVE'
						ORDER BY a.listed_at`, *source)
					if err == nil {
						have := map[string]bool{}
						for _, p := range pool {
							have[p.ID] = true
						}
						merged := []string{}
						for _, p := range pool {
							merged = append(merged, p.ID)
						}
						added := false
						addedIDs := []string{}
						for rows.Next() {
							var pid string
							if err := rows.Scan(&pid); err == nil && !have[pid] {
								have[pid] = true
								merged = append(merged, pid)
								addedIDs = append(addedIDs, pid)
								added = true
							}
						}
						rows.Close()
						if added {
							idsJSON, _ := json.Marshal(merged)
							if _, err := s.db.Exec(c.Context(),
								`UPDATE match_events SET player_ids = $2, updated_at = now() WHERE id = $1`,
								id, string(idsJSON)); err == nil {
								if np, err := s.pool(c.Context(), id); err == nil {
									pool = np
								}
								// Auto-pulled newcomers start from 0, not base.
								if gb.basePlayed > 0 && len(addedIDs) > 0 {
									if _, err := s.db.Exec(c.Context(), `
										INSERT INTO match_count_adjustments (event_id, player_id, played_delta, refereed_delta, is_auto)
										SELECT $1, u.id::uuid, -($3::integer), 0, TRUE
										FROM unnest($2::text[]) WITH ORDINALITY AS u(id, ordinality)
										JOIN players p ON p.id::text = u.id AND p.status = 'ACTIVE'
										ON CONFLICT (event_id, player_id) DO NOTHING`, id, addedIDs, gb.basePlayed); err != nil {
										log.Printf("matchmaker: newcomer base offset failed for event %s: %v", id, err)
									}
								}
							}
							// Newcomers join the AI subset with pool-order arrival.
							aiPlayers = []aiPlayer{}
							for i, p := range pool {
								aiPlayers = append(aiPlayers, aiPlayer{Index: i, ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: i + 1})
							}
						}
					}
				}
				inRound := map[string]bool{}
				for _, m := range matches {
					if m.Round != in.Round {
						continue
					}
					for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
						inRound[t.PlayerID] = true
					}
				}
				sub := []aiPlayer{}
				for pos, p := range pool {
					if inRound[p.ID] {
						continue
					}
					sub = append(sub, aiPlayer{Index: len(sub), ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: pos + 1})
				}
				// Fewer than 4 newcomers: borrow from this round's players so
				// the group still makes 2v2. Earliest arrivals are borrowed
				// first (they waited longest); each plays at most twice.
				borrowAllow = map[string]bool{}
				requireNewcomers = []string{}
				for _, p := range sub {
					requireNewcomers = append(requireNewcomers, p.ID)
				}
				if need := (4 - len(sub)%4) % 4; need > 0 {
					borrowed := 0
					for _, p := range pool {
						if borrowed >= need {
							break
						}
						if inRound[p.ID] && !borrowAllow[p.ID] {
							borrowAllow[p.ID] = true
							borrowed++
						}
					}
					if borrowed < need {
						return httpx.WriteAppError(c, httpx.Unprocessable("Not enough players to complete a top-up group."))
					}
					// Borrowed players join the AI subset with their real arrival.
					for _, p := range pool {
						if borrowAllow[p.ID] {
							pos := 0
							for i, q := range pool {
								if q.ID == p.ID {
									pos = i
									break
								}
							}
							sub = append(sub, aiPlayer{Index: len(sub), ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: pos + 1})
						}
					}
				}
				if len(sub) < 4 {
					return httpx.WriteAppError(c, httpx.Unprocessable("Fewer than 4 players are missing from this round."))
				}
				aiPlayers = sub
				coverageN = len(sub)
				coverageDefault = len(sub)
			} else if exists {
				return httpx.WriteAppError(c, httpx.Conflict("ROUND_EXISTS", "That round already has matches. Delete it first to regenerate."))
			}
			roundNo = in.Round
		}
		// Rest set: players from the previous round's last wave get a break
		// (bench first, else last waves), plus anyone still PLAYING live.
		// Skipped for top-up: borrowed players are mid-round by design.
		rest := map[string]bool{}
		if !in.TopUp {
			rest = restSetFor(matches, pendings, roundNo, gb.wavesPerRound)
			for id := range liveSet {
				rest[id] = true
			}
		}
		restNames := []string{}
		for _, p := range pool {
			if rest[p.ID] {
				restNames = append(restNames, p.Name)
			}
		}
		// Resting mode re-filters the pool each round: the wave just drawn in
		// this same request must sit out the next one too, otherwise a 5-round
		// request would schedule the same people back to back.
		if restStrategy && !in.TopUp {
			aiPlayers = append([]aiPlayer{}, playerBase...)
			drop := map[string]bool{}
			for id := range rest {
				drop[id] = true
			}
			// Pendings just accepted also count as the newest wave.
			if pRound, pWave, ok := lastPendingWave(pendings, roundNo); ok {
				for _, p := range pendings {
					if p.round != pRound || p.wave != pWave {
						continue
					}
					for _, id := range append(idsOf(p.team1), idsOf(p.team2)...) {
						drop[id] = true
					}
				}
			}
			aiPlayers = dropBench(aiPlayers, drop, strategy)
			if !allowance {
				coverageN, coverageDefault = len(aiPlayers), len(aiPlayers)
			}
			if err := requireDrawable(len(aiPlayers), strategy); err != nil {
				return httpx.WriteAppError(c, err)
			}
		}
		// Refresh load counts (earlier rounds generated just now count too).
		for i := range aiPlayers {
			aiPlayers[i].Played = playedCounts[aiPlayers[i].ID]
		}
		applyCaps(aiPlayers, gb.caps, gb.maxRounds)
		if in.PlayedCap != nil || in.CatchUp {
			// Filtered/catch-up mode: standard coverage over the subset (all
			// but at most 3 play); leveling plus load-first ordering protect
			// the required level, leftovers sit by arrival rotation.
			coverageN = len(aiPlayers)
			coverageDefault = len(aiPlayers)
		}
		// Sync per-round validation state (top-up may have rebuilt the pool).
		v.pool, v.borrowAllow, v.requireNewcomers, v.coverageN = pool, borrowAllow, requireNewcomers, coverageN
		v.players, v.rest = aiPlayers, rest
		var accepted []pending
		var genErr error
		// Two AI tries only: each one spends provider tokens, and when the
		// model still cannot satisfy the rules the deterministic local draw
		// below finishes the round instead of hammering the rate limit. A
		// provider error means the whole chain (AI 1 -> AI 2 -> AI 3) failed,
		// and once it happens the rest of the rounds stay local too.
		// retryNote feeds the previous attempt's rejection reason back so
		// the retry fixes exactly what failed (e.g. partial coverage).
		retryNote := ""
		for attempt := 0; attempt < 2; attempt++ {
			if aiUnavailable {
				genErr = httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider unavailable.")
				break
			}
			var aiRounds []aiRound
			var ep aiEndpoint
			var skips []string
			strictSoft := attempt == 0 && len(softSeen) > 0
			aiRounds, ep, skips, genErr = generateMatchups(c.Context(), s.cfg, aiPlayers, history, softHistory, 1, attempt, restNames, retryNote, pool)
			// Skipped primaries are worth reporting even when a fallback AI
			// succeeds, so the UI can show why AI 1 was not used.
			if len(skips) > 0 && aiErrMsg == "" {
				aiErrMsg = strings.Join(skips, "; ")
			}
			if genErr != nil {
				if httpx.AsAppError(genErr).Code == "AI_PROVIDER_ERROR" {
					if aiErrMsg == "" {
						aiErrMsg = httpx.AsAppError(genErr).Message
					}
					aiUnavailable = true
					break
				}
				continue
			}
			if len(aiRounds) == 0 || len(aiRounds[0].Matches) == 0 {
				genErr = httpx.Unprocessable("AI returned no usable matchups. Try generating again.")
				retryNote = "no usable matchups were returned"
				continue
			}
			// Tired players run last so they get a break before their game.
			accepted, genErr = s.validateGenRound(v, orderMatchesByRest(aiRounds[0].Matches, rest, playedCounts, liveSet), roundNo, waveBase, strictSoft)
			if genErr == nil {
				if !usedAISet {
					usedAI, usedAISet = ep, true
				}
				if ep.Label != "" && ep.Label != "AI 1" {
					usedFallback = true
					// Last writer wins so the UI names the actual AI used.
					usedAI = ep
				}
				break
			}
			retryNote = httpx.AsAppError(genErr).Message
		}
		if genErr != nil {
			if ae := httpx.AsAppError(genErr); aiErrMsg == "" {
				aiErrMsg = ae.Message
			}
			// Local fallback tries strict first (avoid last week's final
			// round too) and only repeats it when no other 2v2 exists.
			if len(softSeen) > 0 {
				strictSeen := make(map[string]bool, len(seen)+len(softSeen))
				for k := range seen {
					strictSeen[k] = true
				}
				for k := range softSeen {
					strictSeen[k] = true
				}
				if local, localErr := s.localMatchups(aiPlayers, pool, strictSeen, roundNo, waveBase, wavesPerRound, playedCounts, requireNewcomers, borrowAllow, gb.useReferee, gb.caps, gb.maxRounds, rest); localErr == nil {
					for _, p := range local {
						seen[matchupKey(idsOf(p.team1), idsOf(p.team2))] = true
					}
					accepted, genErr = orderPendingsByLoad(local, rest, playedCounts, liveSet, waveBase, wavesPerRound, pool, gb.useReferee), nil
					usedLocal = true
				}
			}
			if genErr != nil {
				local, localErr := s.localMatchups(aiPlayers, pool, seen, roundNo, waveBase, wavesPerRound, playedCounts, requireNewcomers, borrowAllow, gb.useReferee, gb.caps, gb.maxRounds, rest)
				if localErr == nil {
					accepted, genErr = orderPendingsByLoad(local, rest, playedCounts, liveSet, waveBase, wavesPerRound, pool, gb.useReferee), nil
					usedLocal = true
				}
			}
		}
		if genErr != nil {
			return httpx.WriteAppError(c, genErr)
		}
		pendings = append(pendings, accepted...)
		for _, p := range accepted {
			for _, id := range append(idsOf(p.team1), idsOf(p.team2)...) {
				playedCounts[id]++
			}
		}
		// Newly accepted matchups join the history for the next round call.
		for _, p := range accepted {
			history = append(history, aiHistoryMatch{
				Round: p.round,
				Team1: []string{p.team1[0].PlayerID, p.team1[1].PlayerID},
				Team2: []string{p.team2[0].PlayerID, p.team2[1].PlayerID},
			})
		}
	}
	if len(pendings) == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("AI returned no usable matchups. Try generating again."))
	}
	saved, err := s.savePendings(c.Context(), id, pendings)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	// Tell the UI which AI produced the draw so the loading modal can show
	// "Berhasil diproses AI 2 (model ...)". Local draw reports source lokal.
	aiMeta := map[string]any{
		"label":          "Lokal",
		"provider":       "local",
		"model":          "local-draw",
		"fallback":       usedFallback,
		"local_fallback": true,
		"error":          aiErrMsg,
	}
	if usedAISet {
		aiMeta = map[string]any{
			"label":          usedAI.Label,
			"provider":       usedAI.Provider,
			"model":          usedAI.Model,
			"fallback":       usedFallback,
			"local_fallback": usedLocal,
			"error":          aiErrMsg,
		}
	}
	return httpx.OK(c, http.StatusCreated, map[string]any{"matches": saved, "ai": aiMeta})
}

// GeneratePrompt returns the exact prompt the API flow would send for one
// round, so it can be copied into a web AI (ChatGPT/Groq web) that has no
// API key. No AI is called and nothing is stored. Only the next round or
// one empty round number is supported (one paste = one round).
func (s *Service) GeneratePrompt(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Round     int    `json:"round"`
		PlayedCap *int   `json:"played_cap"`
		CatchUp   bool   `json:"catchup"`
		Strategy  string `json:"strategy"`
	}
	_ = httpx.Decode(c, &in)
	strategy := normalizeStrategy(in.Strategy)
	if strategy == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Strategy tidak dikenal. Pakai \"leveling\" atau \"rest\"."))
	}
	if in.PlayedCap != nil && in.CatchUp {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pilih salah satu: susulan, filter jumlah main, atau perataan."))
	}
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	gb, err := s.loadGenBase(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	roundNo := gb.maxRound + 1
	if in.Round > 0 {
		if in.Round < 1 || in.Round > 99 {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid round number."))
		}
		var exists bool
		if err := s.db.QueryRow(c.Context(),
			`SELECT EXISTS (SELECT 1 FROM generated_matches WHERE event_id = $1 AND round = $2)`,
			id, in.Round).Scan(&exists); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load round.")
		}
		if exists {
			return httpx.WriteAppError(c, httpx.Conflict("ROUND_EXISTS", "That round already has matches. Delete it first to regenerate."))
		}
		roundNo = in.Round
	} else if gb.maxRounds > 0 && roundNo > gb.maxRounds && !in.CatchUp {
		// Planned rounds are spent, but max_rounds is a per-player allowance:
		// draw for whoever still owes matches instead of refusing outright.
		sub, _, aerr := s.allowancePlayers(c.Context(), id, gb)
		if aerr != nil {
			return httpx.WriteAppError(c, aerr)
		}
		gb.aiPlayers = sub
	}
	rest := restSetFor(gb.matches, nil, roundNo, gb.wavesPerRound)
	for id := range liveSetFor(gb.matches) {
		rest[id] = true
	}
	promptPlayers := gb.aiPlayers
	if in.PlayedCap != nil {
		sub, err := s.playedCapSubset(c.Context(), id, gb.pool, gb.matches, *in.PlayedCap)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		promptPlayers = sub
	}
	if in.CatchUp {
		sub, err := s.catchupPlayers(c.Context(), id, gb)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		promptPlayers = sub
	}
	applyCaps(promptPlayers, gb.caps, gb.maxRounds)
	if strategy == strategyRest {
		// Resting mode: the bench is not offered to the model at all, so it
		// cannot put a just-played player back on court.
		bench := map[string]bool{}
		for id := range rest {
			bench[id] = true
		}
		promptPlayers = dropBench(promptPlayers, bench, strategy)
		promptPlayers = enforceCaps(promptPlayers, gb.playedCounts, gb.caps, gb.maxRounds)
		if err := requireDrawable(len(promptPlayers), strategy); err != nil {
			return httpx.WriteAppError(c, err)
		}
		for i := range promptPlayers {
			promptPlayers[i].Index = i
		}
	} else {
		promptPlayers = enforceCaps(promptPlayers, gb.playedCounts, gb.caps, gb.maxRounds)
		for i := range promptPlayers {
			promptPlayers[i].Index = i
		}
	}
	// Resting mode already removed the bench from the player list, so naming
	// them again as "rest these first" would reference players the model
	// cannot see. Only leveling needs the hint.
	restNames := []string{}
	if strategy == strategyLeveling {
		for _, p := range gb.pool {
			if rest[p.ID] {
				restNames = append(restNames, p.Name)
			}
		}
	}
	prompt := aiSystemPrompt + "\n\n" + buildUserPrompt(promptPlayers, gb.history, gb.softHistory, 1, 0, restNames, "", gb.pool)
	if strategy == strategyRest {
		prompt += "\nMode istirahat: jangan memakai pemain yang masih bermain atau baru selesai di wave terakhir. Pakai hanya daftar di atas."
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"round": roundNo, "strategy": strategy, "prompt": prompt})
}

// ManualGenerate validates a pasted web-AI answer through the same pipeline
// as API generate and stores it on success:
//  1. valid JSON (or a readable team table) — else 422,
//  2. index refs resolve to pool players — else 422,
//  3. full rules incl. no-repeat and coverage — else 422 with the reason.
//
// There is deliberately no local fallback: a bad paste is reported so the
// user can re-ask the web AI, never silently replaced.
func (s *Service) ManualGenerate(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Round     int    `json:"round"`
		Raw       string `json:"raw"`
		PlayedCap *int   `json:"played_cap"`
		CatchUp   bool   `json:"catchup"`
		Strategy  string `json:"strategy"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Paste the AI answer as text.")
	}
	strategy := normalizeStrategy(in.Strategy)
	if strategy == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Strategy tidak dikenal. Pakai \"leveling\" atau \"rest\"."))
	}
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	raw := strings.TrimSpace(in.Raw)
	if raw == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Tempel jawaban AI web dulu sebelum disimpan."))
	}
	gb, err := s.loadGenBase(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	roundNo := gb.maxRound + 1
	if in.Round > 0 {
		if in.Round < 1 || in.Round > 99 {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid round number."))
		}
		var exists bool
		if err := s.db.QueryRow(c.Context(),
			`SELECT EXISTS (SELECT 1 FROM generated_matches WHERE event_id = $1 AND round = $2)`,
			id, in.Round).Scan(&exists); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load round.")
		}
		if exists {
			return httpx.WriteAppError(c, httpx.Conflict("ROUND_EXISTS", "That round already has matches. Delete it first to regenerate."))
		}
		roundNo = in.Round
	} else if gb.maxRounds > 0 && roundNo > gb.maxRounds && !in.CatchUp {
		// Planned rounds are spent, but max_rounds is a per-player allowance:
		// draw for whoever still owes matches instead of refusing outright.
		sub, _, aerr := s.allowancePlayers(c.Context(), id, gb)
		if aerr != nil {
			return httpx.WriteAppError(c, aerr)
		}
		gb.aiPlayers = sub
	}
	// Filtered mode answers reference the subset positions (0..k), so every
	// later step (markdown names, index resolve, validation) uses them.
	mPlayers := gb.aiPlayers
	mCoverage := len(gb.pool)
	if gb.maxRounds > 0 && roundNo > gb.maxRounds && !in.CatchUp {
		// Allowance mode shrank the pool above; coverage follows it.
		mCoverage = len(gb.aiPlayers)
	}
	if in.PlayedCap != nil && in.CatchUp {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pilih salah satu: susulan, filter jumlah main, atau perataan."))
	}
	if in.PlayedCap != nil {
		sub, err := s.playedCapSubset(c.Context(), id, gb.pool, gb.matches, *in.PlayedCap)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		mPlayers, mCoverage = sub, len(sub)
	}
	if in.CatchUp {
		sub, err := s.catchupPlayers(c.Context(), id, gb)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		mPlayers, mCoverage = sub, len(sub)
	}
	applyCaps(mPlayers, gb.caps, gb.maxRounds)
	if strategy == strategyRest {
		// Resting mode: a pasted answer may not use anyone who is still on
		// court or just came off. Shrinking the accepted pool makes the
		// validator reject those references instead of scheduling them.
		bench := liveSetFor(gb.matches)
		for id := range restSetFor(gb.matches, nil, roundNo, gb.wavesPerRound) {
			bench[id] = true
		}
		mPlayers = dropBench(mPlayers, bench, strategy)
		mPlayers = enforceCaps(mPlayers, gb.playedCounts, gb.caps, gb.maxRounds)
		mCoverage = len(mPlayers)
		if err := requireDrawable(len(mPlayers), strategy); err != nil {
			return httpx.WriteAppError(c, err)
		}
		for i := range mPlayers {
			mPlayers[i].Index = i
		}
	}
	// 1) Valid JSON (or a readable team table)?
	out, perr := parseAIOutput(raw)
	if perr != nil {
		if md, mderr := parseMarkdownDraw(raw, mPlayers); mderr == nil && len(md) > 0 {
			out = aiOutput{Rounds: []aiRound{{Round: 1, Matches: md}}}
		} else {
			return httpx.WriteAppError(c, httpx.Unprocessable("Tempelannya bukan JSON valid dan tidak terbaca sebagai tabel pasangan. Minta AI web balas HANYA JSON lalu tempel ulang."))
		}
	}
	if len(out.Rounds) == 0 || len(out.Rounds[0].Matches) == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Jawaban tidak berisi ronde/match. Minta AI web balas dengan bentuk yang diminta di prompt."))
	}
	// 2) Index refs resolve to pool players?
	resolve := resolveIdxRefs(mPlayers)
	for mi := range out.Rounds[0].Matches {
		t1, err := resolve(out.Rounds[0].Matches[mi].Team1)
		if err != nil {
			return httpx.WriteAppError(c, httpx.Unprocessable("Referensi pemain tidak dikenal ("+err.Error()+"). Pakai nomor index 0.."+strconv.Itoa(len(mPlayers)-1)+" seperti di prompt."))
		}
		t2, err := resolve(out.Rounds[0].Matches[mi].Team2)
		if err != nil {
			return httpx.WriteAppError(c, httpx.Unprocessable("Referensi pemain tidak dikenal ("+err.Error()+"). Pakai nomor index 0.."+strconv.Itoa(len(mPlayers)-1)+" seperti di prompt."))
		}
		out.Rounds[0].Matches[mi].Team1 = t1
		out.Rounds[0].Matches[mi].Team2 = t2
	}
	// 3) Full rules: 2v2, mixed doubles, one appearance, no repeat, coverage,
	// leveling, and rest for last-wave + live players.
	rest := restSetFor(gb.matches, nil, roundNo, gb.wavesPerRound)
	live := liveSetFor(gb.matches)
	for id := range live {
		rest[id] = true
	}
	v := &genValidator{pool: gb.pool, players: mPlayers, borrowAllow: map[string]bool{}, requireNewcomers: []string{},
		seen: gb.seen, softSeen: gb.softSeen, rest: rest, coverageN: mCoverage,
		wavesPerRound: gb.wavesPerRound, playedCounts: gb.playedCounts, useReferee: gb.useReferee, caps: gb.caps, eventMax: gb.maxRounds}
	accepted, verr := s.validateGenRound(v, orderMatchesByRest(out.Rounds[0].Matches, rest, gb.playedCounts, live), roundNo, 0, len(gb.softSeen) > 0)
	if verr != nil {
		return httpx.WriteAppError(c, verr)
	}
	saved, err := s.savePendings(c.Context(), id, accepted)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	aiMeta := map[string]any{"label": "AI Web", "provider": "manual", "model": "tempel-manual",
		"fallback": false, "local_fallback": false, "error": ""}
	return httpx.OK(c, http.StatusCreated, map[string]any{"matches": saved, "ai": aiMeta})
}

// UpdateMatch edits teams (by player ids, resolved to fresh name/grade
// snapshots), the court number, referee, shuttlecock count, and/or advances
// the status. Court, referee, and shuttlecock count lock once ENDED.
func (s *Service) UpdateMatch(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Team1           []string `json:"team1"`
		Team2           []string `json:"team2"`
		Status          *string  `json:"status"`
		Court           *int     `json:"court"`
		ShuttlecockUsed *int     `json:"shuttlecock_used"`
		RefereeID       *string  `json:"referee_id"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	var eventID, status, t1raw, t2raw string
	var court, courtCap, cocks int
	var refID *string
	if err := s.db.QueryRow(c.Context(),
		`SELECT m.event_id::text, m.status, m.team1::text, m.team2::text, m.court, e.court_count, m.shuttlecock_used,
		        m.referee_id::text
		 FROM generated_matches m JOIN match_events e ON e.id = m.event_id WHERE m.id = $1`,
		id).Scan(&eventID, &status, &t1raw, &t2raw, &court, &courtCap, &cocks, &refID); err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Match not found.")
	}
	prevStatus := status
	if in.Status != nil {
		st := strings.ToUpper(strings.TrimSpace(*in.Status))
		if !validStatuses[st] {
			return httpx.WriteAppError(c, httpx.Unprocessable("Invalid status. Use UPCOMING, PLAYING, or ENDED."))
		}
		status = st
	}
	// Timer follows the status: PLAYING starts it (first time), ENDED stops
	// it, reopening from ENDED resets it so replaying starts from 00:00.
	startTimer := status == "PLAYING" && prevStatus != "PLAYING"
	stopTimer := status == "ENDED"
	clearStop := prevStatus == "ENDED" && status != "ENDED"
	resetTimer := prevStatus == "ENDED" && status != "ENDED"
	if in.ShuttlecockUsed != nil {
		if prevStatus == "ENDED" {
			return httpx.WriteAppError(c, httpx.Unprocessable("Shuttlecock count is locked once the match has ended."))
		}
		if *in.ShuttlecockUsed < 0 || *in.ShuttlecockUsed > 999 {
			return httpx.WriteAppError(c, httpx.Unprocessable("Shuttlecock count must be between 0 and 999."))
		}
		cocks = *in.ShuttlecockUsed
	}
	if in.Court != nil {
		if prevStatus == "ENDED" {
			return httpx.WriteAppError(c, httpx.Unprocessable("Court is locked once the match has ended."))
		}
		maxCourt := 99
		if courtCap > 0 {
			maxCourt = courtCap
		}
		if *in.Court < 0 || *in.Court > maxCourt {
			return httpx.WriteAppError(c, httpx.Unprocessable(
				"Court must be between 0 and "+strconv.Itoa(maxCourt)+" for this event."))
		}
		court = *in.Court
	}
	if in.RefereeID != nil {
		if prevStatus == "ENDED" {
			return httpx.WriteAppError(c, httpx.Unprocessable("Referee is locked once the match has ended."))
		}
		ref := strings.TrimSpace(*in.RefereeID)
		if ref == "" {
			refID = nil
		} else {
			if _, err := uuid.Parse(ref); err != nil {
				return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid referee ID."))
			}
			pool, err := s.pool(c.Context(), eventID)
			if err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load players.")
			}
			found := false
			for _, p := range pool {
				if p.ID == ref {
					found = true
					break
				}
			}
			if !found {
				return httpx.WriteAppError(c, httpx.Unprocessable("Referee must be in this event pool."))
			}
			refID = &ref
		}
	}
	team1 := parseTeam(t1raw)
	team2 := parseTeam(t2raw)
	if in.Team1 != nil || in.Team2 != nil {
		pool, err := s.pool(c.Context(), eventID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load players.")
		}
		cur1, cur2 := idsOf(team1), idsOf(team2)
		if in.Team1 != nil {
			cur1 = in.Team1
		}
		if in.Team2 != nil {
			cur2 = in.Team2
		}
		t1, t2, err := s.resolveTeams(pool, cur1, cur2)
		if err != nil {
			return httpx.WriteAppError(c, err)
		}
		team1, team2 = t1, t2
	}
	var m GenMatch
	var t1, t2 string
	err := s.db.QueryRow(c.Context(), `
		UPDATE generated_matches
		SET team1 = $2, team2 = $3, status = $4, court = $5, shuttlecock_used = $6, referee_id = $11,
		    started_at = CASE WHEN $10 THEN NULL WHEN $7 AND started_at IS NULL THEN now() ELSE started_at END,
		    ended_at = CASE WHEN $8 THEN now() WHEN $9 THEN NULL ELSE ended_at END,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, event_id::text, round, wave, team1::text, team2::text, status, court,
		          started_at::text, ended_at::text, shuttlecock_used, created_at::text, updated_at::text`,
		id, mustTeamJSON(team1), mustTeamJSON(team2), status, court, cocks,
		startTimer, stopTimer, clearStop, resetTimer, refID,
	).Scan(&m.ID, &m.EventID, &m.Round, &m.Wave, &t1, &t2, &m.Status, &m.Court,
		&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update match.")
	}
	m.Team1 = parseTeam(t1)
	m.Team2 = parseTeam(t2)
	if refID != nil {
		var refName string
		_ = s.db.QueryRow(c.Context(), `SELECT name FROM players WHERE id = $1`, *refID).Scan(&refName)
		m.Referee = &Referee{PlayerID: *refID, Name: refName}
	}
	return httpx.OK(c, http.StatusOK, m)
}

func idsOf(team []TeamPlayer) []string {
	ids := []string{}
	for _, t := range team {
		ids = append(ids, t.PlayerID)
	}
	return ids
}

// UpdateEvent renames the event or changes its court cap and public flags.
func (s *Service) UpdateEvent(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Name            *string `json:"name"`
		CourtCount      *int    `json:"court_count"`
		BasePlayed      *int    `json:"base_played"`
		MaxRounds       *int    `json:"max_rounds"`
		IsPublic        *bool   `json:"is_public"`
		ShowGrades      *bool   `json:"show_grades"`
		UseReferee      *bool   `json:"use_referee"`
		SourceSessionID *string `json:"source_session_id"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Event name must not be empty."))
	}
	if in.BasePlayed != nil && (*in.BasePlayed < 0 || *in.BasePlayed > 999) {
		return httpx.WriteAppError(c, httpx.Unprocessable("Starting count must be between 0 and 999."))
	}
	// Starting count is a starting offset: once matches exist it rewrites
	// everyone's history, so it locks. Delete all matches to change it.
	if in.BasePlayed != nil {
		var currentBase, matchCount int
		if err := s.db.QueryRow(c.Context(),
			`SELECT base_played, (SELECT COUNT(*) FROM generated_matches WHERE event_id = $1)
			 FROM match_events WHERE id = $1`, id).Scan(&currentBase, &matchCount); err == nil {
			if matchCount > 0 && *in.BasePlayed != currentBase {
				return httpx.WriteAppError(c, httpx.Unprocessable("Starting count is locked once matches exist. Delete all matches to change it."))
			}
			// No matches yet: keep automatic newcomer offsets in sync so
			// late joiners still start from 0. Manual corrections stay.
			if matchCount == 0 && *in.BasePlayed != currentBase {
				if _, err := s.db.Exec(c.Context(),
					`UPDATE match_count_adjustments SET played_delta = $2, updated_at = now()
					 WHERE event_id = $1 AND is_auto`, id, -*in.BasePlayed); err != nil {
					return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not sync starting counts.")
				}
			}
		}
	}
	if in.SourceSessionID != nil {
		if *in.SourceSessionID != "" {
			if _, err := uuid.Parse(*in.SourceSessionID); err != nil {
				return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid source session ID."))
			}
			var one int
			if err := s.db.QueryRow(c.Context(), `SELECT 1 FROM mabar_sessions WHERE id = $1`, *in.SourceSessionID).Scan(&one); err != nil {
				return httpx.WriteAppError(c, httpx.NotFound("Source session not found."))
			}
		}
	}
	if in.CourtCount != nil && (*in.CourtCount < 0 || *in.CourtCount > 99) {
		return httpx.WriteAppError(c, httpx.Unprocessable("Court count must be between 0 and 99."))
	}
	if in.MaxRounds != nil && (*in.MaxRounds < 0 || *in.MaxRounds > 99) {
		return httpx.WriteAppError(c, httpx.Unprocessable("Match limit must be between 0 and 99."))
	}
	var ev MatchEvent
	var poolRaw string
	err := s.db.QueryRow(c.Context(), `
		UPDATE match_events
		SET name = COALESCE(NULLIF(TRIM(COALESCE($2, '')), ''), name),
		    court_count = COALESCE($3, court_count),
		    base_played = COALESCE($4, base_played),
		    max_rounds = COALESCE($5, max_rounds),
		    is_public = COALESCE($6, is_public),
		    show_grades = COALESCE($7, show_grades),
		    use_referee = COALESCE($9, use_referee),
		    source_session_id = COALESCE(NULLIF($8, '')::uuid, source_session_id),
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, use_referee, player_ids::text, source_session_id::text, created_at::text`,
		id, in.Name, in.CourtCount, in.BasePlayed, in.MaxRounds, in.IsPublic, in.ShowGrades, strOrEmpty(in.SourceSessionID), in.UseReferee,
	).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &ev.UseReferee, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	return httpx.OK(c, http.StatusOK, ev)
}

// AddEventPlayers appends late arrivals to the pool (check-in order is the
// given order). Existing pool members, matches, and history are untouched —
// the next generated round simply includes the newcomers last.
func (s *Service) AddEventPlayers(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		PlayerIDs []string `json:"player_ids"`
	}
	if err := httpx.Decode(c, &in); err != nil || len(in.PlayerIDs) == 0 {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "player_ids is required.")
	}
	for _, pid := range in.PlayerIDs {
		if _, err := uuid.Parse(pid); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid player ID."))
		}
	}
	var ev MatchEvent
	var poolRaw string
	var beforeRaw string
	_ = s.db.QueryRow(c.Context(),
		`SELECT player_ids::text FROM match_events WHERE id = $1`, id).Scan(&beforeRaw)
	err := s.db.QueryRow(c.Context(), `
		UPDATE match_events
		SET player_ids = (
			SELECT COALESCE(jsonb_agg(e ORDER BY o, n), '[]'::jsonb)
			FROM (
				SELECT DISTINCT ON (e) e, o, n FROM (
					SELECT e, 0 AS o, ordinality AS n
					FROM jsonb_array_elements_text(player_ids) WITH ORDINALITY AS j(e, ordinality)
					UNION ALL
					SELECT u.id, 1, u.ordinality
					FROM unnest($2::text[]) WITH ORDINALITY AS u(id, ordinality)
					JOIN players p ON p.id::text = u.id AND p.status = 'ACTIVE'
				) all_rows ORDER BY e, o, n
			) dedup
		),
		updated_at = now()
		WHERE id = $1
		RETURNING id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, use_referee, player_ids::text, created_at::text`,
		id, in.PlayerIDs,
	).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &ev.UseReferee, &poolRaw, &ev.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	// Newcomers missed the pre-event games the base stands for, so they
	// start from 0: book a correction of -base on top of the shared base.
	// Only genuinely new pool members (not re-added ones), and later base
	// changes never touch their personal offset.
	if ev.BasePlayed > 0 {
		before := map[string]bool{}
		var beforeIDs []string
		_ = json.Unmarshal([]byte(beforeRaw), &beforeIDs)
		for _, pid := range beforeIDs {
			before[pid] = true
		}
		newIDs := []string{}
		for _, pid := range in.PlayerIDs {
			if !before[pid] {
				newIDs = append(newIDs, pid)
			}
		}
		if len(newIDs) > 0 {
			if _, err := s.db.Exec(c.Context(), `
				INSERT INTO match_count_adjustments (event_id, player_id, played_delta, refereed_delta, is_auto)
				SELECT $1, u.id::uuid, -($3::integer), 0, TRUE
				FROM unnest($2::text[]) WITH ORDINALITY AS u(id, ordinality)
				JOIN players p ON p.id::text = u.id AND p.status = 'ACTIVE'
				ON CONFLICT (event_id, player_id) DO NOTHING`, id, newIDs, ev.BasePlayed); err != nil {
				log.Printf("matchmaker: newcomer base offset failed: %v", err)
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Pool updated, but starting counts could not be synced.")
			}
		}
	}
	// Late arrivals show up in person: mark them PRESENT on the source
	// session so attendance (and billing) follows the pool. LISTED and
	// CONFIRMED flip to PRESENT; ABSENT/CANCELLED rows are left alone,
	// mirroring the session's own PresentAll behavior.
	var source *string
	if err := s.db.QueryRow(c.Context(),
		`SELECT source_session_id::text FROM match_events WHERE id = $1`, id).Scan(&source); err == nil && source != nil && *source != "" {
		if _, err := s.db.Exec(c.Context(), `
			INSERT INTO attendances (id, session_id, player_id, status, is_member, listed_at)
			SELECT gen_random_uuid(), $1, u.id::uuid, 'PRESENT',
			       EXISTS (SELECT 1 FROM memberships m
			               JOIN mabar_sessions ms ON ms.period_id = m.period_id
			               WHERE ms.id = $1 AND m.player_id::text = u.id AND m.status <> 'WITHDRAWN'),
			       now()
			FROM unnest($2::text[]) WITH ORDINALITY AS u(id, ordinality)
			JOIN players p ON p.id::text = u.id AND p.status = 'ACTIVE'
			ON CONFLICT (session_id, player_id) DO UPDATE SET
			    status = 'PRESENT', listed_at = now()
			WHERE attendances.status IN ('LISTED', 'CONFIRMED')`, *source, in.PlayerIDs); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Pool updated, but attendance could not be synced.")
		}
	}
	return httpx.OK(c, http.StatusOK, ev)
}

// DeleteRound removes every match of one round group (e.g. a bad draw).
// The round number stays free: generating with {"round": N} fills it again.
func (s *Service) DeleteRound(c *fiber.Ctx) error {
	eventID := c.Params("id")
	round, err := parseRoundParam(c.Params("round"))
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	if _, err := uuid.Parse(eventID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	tag, err := s.db.Exec(c.Context(),
		`DELETE FROM generated_matches WHERE event_id = $1 AND round = $2`, eventID, round)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete round.")
	}
	if tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Round not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true, "round": round})
}

// DeleteMatch removes one generated card. Players freed by the deletion
// become missing from their round, so TopUp (or a round regenerate) can fill
// the round again afterwards with freshly drawn matchups.
func (s *Service) DeleteMatch(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid match ID."))
	}
	tag, err := s.db.Exec(c.Context(), `DELETE FROM generated_matches WHERE id = $1`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete match.")
	}
	if tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Match not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

// CreateMatch records one hand-made card (e.g. games already played before
// the event was created). Teams go through the same 2v2 / pool /
// mixed-doubles checks as generated ones, and the card lands in the round's
// last wave. No-repeat is deliberately NOT enforced: reality is recorded
// as-is, and the card joins the history future AI draws avoid.
func (s *Service) CreateMatch(c *fiber.Ctx) error {
	var in struct {
		EventID         string   `json:"event_id"`
		Round           int      `json:"round"`
		Team1           []string `json:"team1"`
		Team2           []string `json:"team2"`
		Court           *int     `json:"court"`
		RefereeID       *string  `json:"referee_id"`
		Status          *string  `json:"status"`
		ShuttlecockUsed *int     `json:"shuttlecock_used"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if _, err := uuid.Parse(in.EventID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	if in.Round < 1 || in.Round > 99 {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid round number."))
	}
	pool, err := s.pool(c.Context(), in.EventID)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	wavesPerRound, maxRounds, courtCap := 3, 0, 0
	_ = s.db.QueryRow(c.Context(),
		`SELECT COALESCE(NULLIF(court_count, 0), 3), max_rounds, court_count FROM match_events WHERE id = $1`,
		in.EventID).Scan(&wavesPerRound, &maxRounds, &courtCap)
	if wavesPerRound <= 0 {
		wavesPerRound = 3
	}
	if maxRounds > 0 && in.Round > maxRounds {
		return httpx.WriteAppError(c, httpx.Unprocessable(
			"Round "+strconv.Itoa(in.Round)+" is past this event's match limit ("+strconv.Itoa(maxRounds)+"). Raise the limit in event settings first."))
	}
	t1, t2, err := s.resolveTeams(pool, in.Team1, in.Team2)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	seenIDs := map[string]bool{}
	for _, pid := range append(append([]string{}, in.Team1...), in.Team2...) {
		if seenIDs[pid] {
			return httpx.WriteAppError(c, httpx.Unprocessable("Satu pemain tidak boleh di dua tim sekaligus."))
		}
		seenIDs[pid] = true
	}
	court := 0
	if in.Court != nil {
		court = *in.Court
	}
	maxCourt := 99
	if courtCap > 0 {
		maxCourt = courtCap
	}
	if court < 0 || court > maxCourt {
		return httpx.WriteAppError(c, httpx.Unprocessable(
			"Court must be between 0 and "+strconv.Itoa(maxCourt)+" for this event."))
	}
	status := "UPCOMING"
	if in.Status != nil {
		status = strings.ToUpper(strings.TrimSpace(*in.Status))
	}
	if !validStatuses[status] {
		return httpx.WriteAppError(c, httpx.Unprocessable("Invalid status. Use UPCOMING, PLAYING, or ENDED."))
	}
	cocks := 0
	if in.ShuttlecockUsed != nil {
		cocks = *in.ShuttlecockUsed
	}
	if cocks < 0 || cocks > 999 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Shuttlecock count must be between 0 and 999."))
	}
	var refID *string
	if in.RefereeID != nil && strings.TrimSpace(*in.RefereeID) != "" {
		ref := strings.TrimSpace(*in.RefereeID)
		if _, err := uuid.Parse(ref); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid referee ID."))
		}
		found := false
		for _, p := range pool {
			if p.ID == ref {
				found = true
				break
			}
		}
		if !found {
			return httpx.WriteAppError(c, httpx.Unprocessable("Referee must be in this event pool."))
		}
		refID = &ref
	}
	var existing int
	_ = s.db.QueryRow(c.Context(),
		`SELECT COUNT(*) FROM generated_matches WHERE event_id = $1 AND round = $2`,
		in.EventID, in.Round).Scan(&existing)
	wave := existing/wavesPerRound + 1
	var m GenMatch
	var t1raw, t2raw string
	err = s.db.QueryRow(c.Context(), `
		INSERT INTO generated_matches (id, event_id, round, wave, team1, team2, referee_id, status, court, shuttlecock_used, started_at, ended_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9,
		        CASE WHEN $7 IN ('PLAYING', 'ENDED') THEN now() END,
		        CASE WHEN $7 = 'ENDED' THEN now() END)
		RETURNING id::text, event_id::text, round, wave, team1::text, team2::text, status, court,
		          started_at::text, ended_at::text, shuttlecock_used, created_at::text, updated_at::text`,
		in.EventID, in.Round, wave, mustTeamJSON(t1), mustTeamJSON(t2), refID, status, court, cocks,
	).Scan(&m.ID, &m.EventID, &m.Round, &m.Wave, &t1raw, &t2raw, &m.Status, &m.Court,
		&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save match.")
	}
	m.Team1 = parseTeam(t1raw)
	m.Team2 = parseTeam(t2raw)
	if refID != nil {
		var refName string
		_ = s.db.QueryRow(c.Context(), `SELECT name FROM players WHERE id = $1`, *refID).Scan(&refName)
		m.Referee = &Referee{PlayerID: *refID, Name: refName}
	}
	return httpx.OK(c, http.StatusCreated, m)
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parseRoundParam(raw string) (int, error) {
	n := 0
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, httpx.BadRequest("BAD_REQUEST", "Invalid round number.")
		}
		n = n*10 + int(ch-'0')
	}
	if n <= 0 || n > 99 {
		return 0, httpx.BadRequest("BAD_REQUEST", "Invalid round number.")
	}
	return n, nil
}
func (s *Service) DeleteEvent(c *fiber.Ctx) error {
	id := c.Params("id")
	tag, err := s.db.Exec(c.Context(), `DELETE FROM match_events WHERE id = $1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}
