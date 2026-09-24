package matchmaker

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

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

type GenMatch struct {
	ID             string       `json:"id"`
	EventID        string       `json:"event_id"`
	Round          int          `json:"round"`
	Team1          []TeamPlayer `json:"team1"`
	Team2          []TeamPlayer `json:"team2"`
	Status         string       `json:"status"`
	Court          int          `json:"court"`
	StartedAt      *string      `json:"started_at"`
	EndedAt        *string      `json:"ended_at"`
	ShuttlecockUsed int         `json:"shuttlecock_used"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
}

type MatchEvent struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	CourtCount int      `json:"court_count"`
	BasePlayed int      `json:"base_played"`
	IsPublic   bool     `json:"is_public"`
	ShowGrades bool     `json:"show_grades"`
	PlayerIDs  []string `json:"player_ids"`
	CreatedAt  string   `json:"created_at"`
}

type PlayerCount struct {
	PlayerID string  `json:"player_id"`
	Name     string  `json:"name"`
	Grade    *string `json:"grade"`
	Gender   *string `json:"gender"`
	Arrival  int     `json:"arrival"`
	Played   int     `json:"played"`
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
		// Mixed-doubles rule: a female player (P) must pair with a male (L).
		for i, t := range team {
			if t.Gender != nil && *t.Gender == "P" {
				other := team[1-i]
				if other.Gender == nil || *other.Gender != "L" {
					return nil, httpx.Unprocessable(t.Name + " (P) must pair with a male (L) player.")
				}
			}
		}
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

func (s *Service) listMatches(ctx context.Context, eventID string) ([]GenMatch, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, event_id::text, round, team1::text, team2::text, status, court,
		       started_at::text, ended_at::text, shuttlecock_used,
		       created_at::text, updated_at::text
		FROM generated_matches WHERE event_id = $1 ORDER BY round, created_at, id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	matches := []GenMatch{}
	for rows.Next() {
		var m GenMatch
		var t1, t2 string
		if err := rows.Scan(&m.ID, &m.EventID, &m.Round, &t1, &t2, &m.Status, &m.Court,
			&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt); err == nil {
			m.Team1 = parseTeam(t1)
			m.Team2 = parseTeam(t2)
			matches = append(matches, m)
		}
	}
	return matches, nil
}

// CreateEvent opens a match night. Without player_ids the pool defaults to
// every active player. court_count caps the court numbers usable on cards
// (0 = unlimited).
func (s *Service) CreateEvent(c *fiber.Ctx) error {
	var in struct {
		Name       string   `json:"name"`
		PlayerIDs  []string `json:"player_ids"`
		CourtCount *int     `json:"court_count"`
		BasePlayed *int     `json:"base_played"`
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
	base := 0
	if in.BasePlayed != nil {
		if *in.BasePlayed < 0 || *in.BasePlayed > 999 {
			return httpx.WriteAppError(c, httpx.Unprocessable("Starting count must be between 0 and 999."))
		}
		base = *in.BasePlayed
	}
	ids := in.PlayerIDs
	if len(ids) == 0 {
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
	}
	if len(ids) < 4 {
		return httpx.WriteAppError(c, httpx.Unprocessable("An event needs at least 4 players for 2v2 doubles."))
	}
	idsJSON, _ := json.Marshal(ids)
	var ev MatchEvent
	var poolRaw string
	err := s.db.QueryRow(c.Context(), `
		INSERT INTO match_events (id, name, court_count, base_played, player_ids) VALUES (gen_random_uuid(), $1, $2, $3, $4)
		RETURNING id::text, name, status, court_count, base_played, is_public, show_grades, player_ids::text, created_at::text`,
		strings.TrimSpace(in.Name), courts, base, string(idsJSON)).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create event.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
	return httpx.OK(c, http.StatusCreated, ev)
}

func (s *Service) ListEvents(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(), `
		SELECT e.id::text, e.name, e.status, e.court_count, e.is_public, e.created_at::text,
		       COALESCE(jsonb_array_length(e.player_ids), 0),
		       COUNT(m.id)
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
		IsPublic   bool   `json:"is_public"`
		CreatedAt  string `json:"created_at"`
		Players    int    `json:"players"`
		Matches    int    `json:"matches"`
	}
	list := []row{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.ID, &t.Name, &t.Status, &t.CourtCount, &t.IsPublic, &t.CreatedAt, &t.Players, &t.Matches); err == nil {
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
		`SELECT id::text, name, status, court_count, base_played, is_public, show_grades, player_ids::text, created_at::text FROM match_events WHERE id = $1`,
		id).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.CreatedAt); err != nil {
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
	played := map[string]int{}
	for _, m := range matches {
		if m.Status != "ENDED" && m.Status != "PLAYING" {
			continue
		}
		for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
			played[t.PlayerID]++
		}
	}
	arrival := map[string]int{}
	for i, pid := range ev.PlayerIDs {
		arrival[pid] = i + 1
	}
	counts := []PlayerCount{}
	for _, p := range pool {
		counts = append(counts, PlayerCount{PlayerID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Arrival: arrival[p.ID], Played: ev.BasePlayed + played[p.ID]})
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"event": ev, "matches": matches, "counts": counts})
}

// publicEventDetail loads the read-only payload for the public live page.
// Non-public events look exactly like missing ones.
func (s *Service) publicEventDetail(ctx context.Context, id string) (MatchEvent, []GenMatch, []PlayerCount, error) {
	var ev MatchEvent
	var poolRaw string
	if err := s.db.QueryRow(ctx,
		`SELECT id::text, name, status, court_count, base_played, is_public, show_grades, player_ids::text, created_at::text
		 FROM match_events WHERE id = $1 AND is_public`,
		id).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.CreatedAt); err != nil {
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
	played := map[string]int{}
	for _, m := range matches {
		if m.Status != "ENDED" && m.Status != "PLAYING" {
			continue
		}
		for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
			played[t.PlayerID]++
		}
	}
	arrival := map[string]int{}
	for i, pid := range ev.PlayerIDs {
		arrival[pid] = i + 1
	}
	counts := []PlayerCount{}
	for _, p := range pool {
		grade := p.Grade
		if !ev.ShowGrades {
			grade = nil
		}
		counts = append(counts, PlayerCount{PlayerID: p.ID, Name: p.Name, Grade: grade, Gender: p.Gender, Arrival: arrival[p.ID], Played: ev.BasePlayed + played[p.ID]})
	}
	// Pool ids are an admin concern; the public payload carries names only.
	ev.PlayerIDs = []string{}
	return ev, matches, counts, nil
}

// PublicEvents lists events flagged public — the live index.
func (s *Service) PublicEvents(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(), `
		SELECT e.id::text, e.name, e.created_at::text, COUNT(m.id)
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
	}
	list := []row{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.Matches); err == nil {
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

// Generate asks the AI for balanced rounds, validates every matchup (2v2,
// pool members, one appearance per player per round, no repeat of any
// previous matchup in this event), and stores them as UPCOMING.
func (s *Service) Generate(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Rounds int `json:"rounds"`
		Round  int `json:"round"`
	}
	_ = httpx.Decode(c, &in)
	if in.Rounds <= 0 {
		in.Rounds = 1
	}
	if in.Rounds > 5 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Generate at most 5 rounds at a time."))
	}
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid event ID."))
	}
	pool, err := s.pool(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	if len(pool) < 4 {
		return httpx.WriteAppError(c, httpx.Unprocessable("An event needs at least 4 players for 2v2 doubles."))
	}
	matches, err := s.listMatches(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load history.")
	}
	aiPlayers := []aiPlayer{}
	for i, p := range pool {
		aiPlayers = append(aiPlayers, aiPlayer{Index: i, ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: i + 1})
	}
	history := []aiHistoryMatch{}
	seen := map[string]bool{}
	maxRound := 0
	for _, m := range matches {
		names := func(team []TeamPlayer) []string {
			out := []string{}
			for _, t := range team {
				out = append(out, t.Name)
			}
			return out
		}
		history = append(history, aiHistoryMatch{Round: m.Round, Team1: names(m.Team1), Team2: names(m.Team2)})
		ids := []string{}
		for _, t := range append(append([]TeamPlayer{}, m.Team1...), m.Team2...) {
			ids = append(ids, t.PlayerID)
		}
		seen[matchupKey(ids[:2], ids[2:])] = true
		if m.Round > maxRound {
			maxRound = m.Round
		}
	}

	// One AI call per round: small responses stay valid JSON, and every
	// earlier round (existing or generated just now) joins the history of the
	// next call, so nothing repeats. Nothing is stored until all rounds
	// validate — existing rounds are never touched.
	type pending struct {
		round       int
		team1, team2 []TeamPlayer
	}
	pendings := []pending{}

	// validateRound checks one AI round: 2v2, pool members, mixed-doubles
	// rule, one appearance per player, no repeat, and FULL coverage (every
	// player plays except unavoidable sit-outs: used%4==0 and at most 3 out).
	validateRound := func(ms []aiMatchup, roundNo int) ([]pending, error) {
		used := map[string]bool{}
		out := []pending{}
		for _, mu := range ms {
			if len(mu.Team1) != 2 || len(mu.Team2) != 2 {
				return nil, httpx.Unprocessable("AI returned a non-2v2 matchup. Try generating again.")
			}
			t1, t2, err := s.resolveTeams(pool, mu.Team1, mu.Team2)
			if err != nil {
				return nil, httpx.Unprocessable("AI used players outside this event (" + err.Error() + "). Try generating again.")
			}
			for _, pid := range append(append([]string{}, mu.Team1...), mu.Team2...) {
				if used[pid] {
					return nil, httpx.Unprocessable("AI listed a player twice in one round. Try generating again.")
				}
				used[pid] = true
			}
			key := matchupKey(mu.Team1, mu.Team2)
			if seen[key] {
				return nil, httpx.Unprocessable("AI repeated a previous matchup. Try generating again.")
			}
			out = append(out, pending{round: roundNo, team1: t1, team2: t2})
		}
		n := len(pool)
		if len(used) == 0 || len(used)%4 != 0 || len(used) < n-3 {
			return nil, httpx.Unprocessable(
				"AI covered only part of the pool. Try generating again.")
		}
		for _, p := range out {
			seen[matchupKey(idsOf(p.team1), idsOf(p.team2))] = true
		}
		return out, nil
	}

	for i := 0; i < in.Rounds; i++ {
		// Explicit round number fills one empty round (e.g. after deleting
		// it); otherwise rounds append after the highest existing one.
		roundNo := maxRound + i + 1
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
			if exists {
				return httpx.WriteAppError(c, httpx.Conflict("ROUND_EXISTS", "That round already has matches. Delete it first to regenerate."))
			}
			roundNo = in.Round
		}
		var accepted []pending
		var genErr error
		for attempt := 0; attempt < 3; attempt++ {
			var aiRounds []aiRound
			aiRounds, genErr = generateMatchups(c.Context(), s.cfg, aiPlayers, history, 1)
			if genErr != nil {
				continue
			}
			if len(aiRounds) == 0 || len(aiRounds[0].Matches) == 0 {
				genErr = httpx.Unprocessable("AI returned no usable matchups. Try generating again.")
				continue
			}
			accepted, genErr = validateRound(aiRounds[0].Matches, roundNo)
			if genErr == nil {
				break
			}
		}
		if genErr != nil {
			return httpx.WriteAppError(c, genErr)
		}
		pendings = append(pendings, accepted...)
		// Newly accepted matchups join the history for the next round call.
		for _, p := range accepted {
			history = append(history, aiHistoryMatch{
				Round: p.round,
				Team1: []string{p.team1[0].Name, p.team1[1].Name},
				Team2: []string{p.team2[0].Name, p.team2[1].Name},
			})
		}
	}
	if len(pendings) == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("AI returned no usable matchups. Try generating again."))
	}
	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save matchups.")
	}
	defer tx.Rollback(c.Context())
	saved := []GenMatch{}
	for _, p := range pendings {
		var m GenMatch
		var t1, t2 string
		err := tx.QueryRow(c.Context(), `
			INSERT INTO generated_matches (id, event_id, round, team1, team2, status)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'UPCOMING')
			RETURNING id::text, event_id::text, round, team1::text, team2::text, status, court,
			          started_at::text, ended_at::text, shuttlecock_used, created_at::text, updated_at::text`,
			id, p.round, mustTeamJSON(p.team1), mustTeamJSON(p.team2),
		).Scan(&m.ID, &m.EventID, &m.Round, &t1, &t2, &m.Status, &m.Court,
			&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save matchups.")
		}
		m.Team1 = parseTeam(t1)
		m.Team2 = parseTeam(t2)
		saved = append(saved, m)
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save matchups.")
	}
	return httpx.OK(c, http.StatusCreated, saved)
}

// UpdateMatch edits teams (by player ids, resolved to fresh name/grade
// snapshots), the court number, and/or advances the status. Court is locked
// once the match has ENDED.
func (s *Service) UpdateMatch(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Team1           []string `json:"team1"`
		Team2           []string `json:"team2"`
		Status          *string  `json:"status"`
		Court           *int     `json:"court"`
		ShuttlecockUsed *int     `json:"shuttlecock_used"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	var eventID, status, t1raw, t2raw string
	var court, courtCap, cocks int
	if err := s.db.QueryRow(c.Context(),
		`SELECT m.event_id::text, m.status, m.team1::text, m.team2::text, m.court, e.court_count, m.shuttlecock_used
		 FROM generated_matches m JOIN match_events e ON e.id = m.event_id WHERE m.id = $1`,
		id).Scan(&eventID, &status, &t1raw, &t2raw, &court, &courtCap, &cocks); err != nil {
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
		SET team1 = $2, team2 = $3, status = $4, court = $5, shuttlecock_used = $6,
		    started_at = CASE WHEN $10 THEN NULL WHEN $7 AND started_at IS NULL THEN now() ELSE started_at END,
		    ended_at = CASE WHEN $8 THEN now() WHEN $9 THEN NULL ELSE ended_at END,
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, event_id::text, round, team1::text, team2::text, status, court,
		          started_at::text, ended_at::text, shuttlecock_used, created_at::text, updated_at::text`,
		id, mustTeamJSON(team1), mustTeamJSON(team2), status, court, cocks,
		startTimer, stopTimer, clearStop, resetTimer,
	).Scan(&m.ID, &m.EventID, &m.Round, &t1, &t2, &m.Status, &m.Court,
		&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update match.")
	}
	m.Team1 = parseTeam(t1)
	m.Team2 = parseTeam(t2)
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
		Name       *string `json:"name"`
		CourtCount *int    `json:"court_count"`
		BasePlayed *int    `json:"base_played"`
		IsPublic   *bool   `json:"is_public"`
		ShowGrades *bool   `json:"show_grades"`
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
	if in.CourtCount != nil && (*in.CourtCount < 0 || *in.CourtCount > 99) {
		return httpx.WriteAppError(c, httpx.Unprocessable("Court count must be between 0 and 99."))
	}
	var ev MatchEvent
	var poolRaw string
	err := s.db.QueryRow(c.Context(), `
		UPDATE match_events
		SET name = COALESCE(NULLIF(TRIM(COALESCE($2, '')), ''), name),
		    court_count = COALESCE($3, court_count),
		    base_played = COALESCE($4, base_played),
		    is_public = COALESCE($5, is_public),
		    show_grades = COALESCE($6, show_grades),
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, name, status, court_count, base_played, is_public, show_grades, player_ids::text, created_at::text`,
		id, in.Name, in.CourtCount, in.BasePlayed, in.IsPublic, in.ShowGrades,
	).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.CreatedAt)
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
		RETURNING id::text, name, status, court_count, base_played, is_public, show_grades, player_ids::text, created_at::text`,
		id, in.PlayerIDs,
	).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Event not found.")
	}
	_ = json.Unmarshal([]byte(poolRaw), &ev.PlayerIDs)
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
