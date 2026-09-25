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
	PlayerIDs       []string `json:"player_ids"`
	SourceSessionID *string  `json:"source_session_id"`
	CreatedAt       string   `json:"created_at"`
}

type PlayerCount struct {
	PlayerID string  `json:"player_id"`
	Name     string  `json:"name"`
	Grade    *string `json:"grade"`
	Gender   *string `json:"gender"`
	Arrival  int     `json:"arrival"`
	Played   int     `json:"played"`
	Refereed int     `json:"refereed"`
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
	newcomers []string, borrow map[string]bool) ([]pending, error) {

	rank := map[string]int{}
	gender := map[string]string{}
	arrival := map[string]int{}
	for i, p := range players {
		rank[p.ID] = p.Rank
		arrival[p.ID] = i
		if p.Gender != nil {
			gender[p.ID] = *p.Gender
		}
	}
	// A side is legal when no female (P) is paired with P or unknown gender.
	sideOK := func(a, b string) bool {
		ga, gb := gender[a], gender[b]
		return !((ga == "P" && gb != "L") || (gb == "P" && ga != "L"))
	}
	must := map[string]bool{}
	for _, id := range newcomers {
		must[id] = true
	}
	for id := range borrow {
		must[id] = true
	}
	// Sit-outs go to whoever has played most (ties: latest arrival); late
	// arrivals and borrowed top-up players always play.
	optional := []string{}
	for _, p := range players {
		if !must[p.ID] {
			optional = append(optional, p.ID)
		}
	}
	sort.Slice(optional, func(i, j int) bool {
		if played[optional[i]] != played[optional[j]] {
			return played[optional[i]] > played[optional[j]]
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
				if !sideOK(t1[0], t1[1]) || !sideOK(t2[0], t2[1]) {
					continue
				}
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
		assignReferees(pool, out, waves, played)
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
		WHERE m.event_id = $1 ORDER BY m.round, m.created_at, m.id`, eventID)
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

// CreateEvent opens a match night. Without player_ids the pool defaults to
// every active player. court_count caps the court numbers usable on cards
// (0 = unlimited), max_rounds is the planned number of rounds (0 = no limit).
func (s *Service) CreateEvent(c *fiber.Ctx) error {
	var in struct {
		Name            string   `json:"name"`
		PlayerIDs       []string `json:"player_ids"`
		CourtCount      *int     `json:"court_count"`
		BasePlayed      *int     `json:"base_played"`
		MaxRounds       *int     `json:"max_rounds"`
		SourceSessionID *string  `json:"source_session_id"`
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
		INSERT INTO match_events (id, name, court_count, base_played, max_rounds, player_ids, source_session_id) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)
		RETURNING id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, player_ids::text, source_session_id::text, created_at::text`,
		strings.TrimSpace(in.Name), courts, base, maxRounds, string(idsJSON), sourceSession).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt)
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
		`SELECT id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, player_ids::text, source_session_id::text, created_at::text FROM match_events WHERE id = $1`,
		id).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt); err != nil {
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
		counts = append(counts, PlayerCount{
			PlayerID: p.ID,
			Name:     p.Name,
			Grade:    p.Grade,
			Gender:   p.Gender,
			Arrival:  arrival[p.ID],
			Played:   pCount,
			Refereed: rCount,
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
// The resulting count may never drop below zero.
func (s *Service) AdjustCount(c *fiber.Ctx) error {
	id := c.Params("id")
	playerID := c.Params("player_id")
	var in struct {
		PlayedDelta   int `json:"played_delta"`
		RefereedDelta int `json:"refereed_delta"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if in.PlayedDelta == 0 && in.RefereedDelta == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pick a change of at least 1."))
	}
	if in.PlayedDelta < -50 || in.PlayedDelta > 50 || in.RefereedDelta < -50 || in.RefereedDelta > 50 {
		return httpx.WriteAppError(c, httpx.Unprocessable("A single change may not pass 50."))
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
		`SELECT id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, player_ids::text, source_session_id::text, created_at::text
		 FROM match_events WHERE id = $1`, id).
		Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt); err != nil {
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
		INSERT INTO match_count_adjustments (event_id, player_id, played_delta, refereed_delta)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id, player_id) DO UPDATE
		SET played_delta = match_count_adjustments.played_delta + EXCLUDED.played_delta,
		    refereed_delta = match_count_adjustments.refereed_delta + EXCLUDED.refereed_delta,
		    updated_at = now()`,
		id, playerID, in.PlayedDelta, in.RefereedDelta); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save the change.")
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
		`SELECT id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, player_ids::text, source_session_id::text, created_at::text
		 FROM match_events WHERE id = $1 AND is_public`,
		id).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt); err != nil {
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

// Generate asks the AI for balanced rounds, validates every matchup (2v2,
// pool members, one appearance per player per round, no repeat of any
// previous matchup in this event), and stores them as UPCOMING.
func (s *Service) Generate(c *fiber.Ctx) error {
	id := c.Params("id")
	var in struct {
		Rounds int  `json:"rounds"`
		Round  int  `json:"round"`
		TopUp  bool `json:"topup"`
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
	// Matches per wave = courts running simultaneously (event cap, else 3).
	wavesPerRound := 3
	maxRounds := 0
	_ = s.db.QueryRow(c.Context(),
		`SELECT COALESCE(NULLIF(court_count, 0), 3), max_rounds FROM match_events WHERE id = $1`, id).Scan(&wavesPerRound, &maxRounds)
	if wavesPerRound <= 0 {
		wavesPerRound = 3
	}
	matches, err := s.listMatches(c.Context(), id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load history.")
	}
	// The event's match limit caps how many rounds this night may reach; one
	// round is one match per player, so it also drives the count bars.
	if in.Round > 0 && maxRounds > 0 && in.Round > maxRounds {
		return httpx.WriteAppError(c, httpx.Unprocessable(
			"Round "+strconv.Itoa(in.Round)+" is past this event's match limit ("+strconv.Itoa(maxRounds)+"). Raise the limit in event settings first."))
	}
	aiPlayers := []aiPlayer{}
	for i, p := range pool {
		aiPlayers = append(aiPlayers, aiPlayer{Index: i, ID: p.ID, Name: p.Name, Grade: p.Grade, Gender: p.Gender, Rank: gradeRank(p.Grade), Arrival: i + 1})
	}
	history := []aiHistoryMatch{}
	seen := map[string]bool{}
	maxRound := 0
	// playedCounts drives referee duty: the least-played player referees.
	playedCounts := map[string]int{}
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
			playedCounts[t.PlayerID]++
		}
		seen[matchupKey(ids[:2], ids[2:])] = true
		if m.Round > maxRound {
			maxRound = m.Round
		}
	}
	if in.Round == 0 && maxRounds > 0 {
		remaining := maxRounds - maxRound
		if remaining <= 0 {
			return httpx.WriteAppError(c, httpx.Unprocessable(
				"This event already has all its planned matches ("+strconv.Itoa(maxRounds)+"). Raise the match limit in event settings to add more."))
		}
		if in.Rounds > remaining {
			in.Rounds = remaining
		}
	}

	// One AI call per round: small responses stay valid JSON, and every
	// earlier round (existing or generated just now) joins the history of the
	// next call, so nothing repeats. Nothing is stored until all rounds
	// validate — existing rounds are never touched.
	pendings := []pending{}
	coverageN := len(pool)
	// borrowAllow marks round players borrowed to complete a top-up group
	// (they may appear twice); requireNewcomers must all play.
	borrowAllow := map[string]bool{}
	requireNewcomers := []string{}
	// aiUnavailable latches once the provider fails so the remaining rounds
	// are drawn locally instead of waiting on throttled calls.
	aiUnavailable := false

	// validateRound checks one AI round: 2v2, pool members, mixed-doubles
	// rule, one appearance per player, no repeat, and FULL coverage. Waves
	// chunk the returned order by courts that can run simultaneously; the
	// referee of every match is picked afterwards from the same pool.
	validateRound := func(ms []aiMatchup, roundNo, waveBase int) ([]pending, error) {
		used := map[string]int{}
		out := []pending{}
		waves := map[int]int{}
		for idx := range ms {
			waves[idx] = waveBase + idx/wavesPerRound + 1
		}
		for _, mu := range ms {
			if len(mu.Team1) != 2 || len(mu.Team2) != 2 {
				return nil, httpx.Unprocessable("AI returned a non-2v2 matchup. Try generating again.")
			}
			t1, t2, err := s.resolveTeams(pool, mu.Team1, mu.Team2)
			if err != nil {
				return nil, httpx.Unprocessable("AI used players outside this event (" + err.Error() + "). Try generating again.")
			}
			for _, pid := range append(append([]string{}, mu.Team1...), mu.Team2...) {
				used[pid]++
				if used[pid] > 1 && !borrowAllow[pid] {
					return nil, httpx.Unprocessable("AI listed a player twice in one round. Try generating again.")
				}
				if used[pid] > 2 {
					return nil, httpx.Unprocessable("AI listed a player three times in one round. Try generating again.")
				}
			}
			key := matchupKey(mu.Team1, mu.Team2)
			if seen[key] {
				return nil, httpx.Unprocessable("AI repeated a previous matchup. Try generating again.")
			}
			out = append(out, pending{round: roundNo, wave: waves[len(out)], team1: t1, team2: t2})
		}
		for _, req := range requireNewcomers {
			if used[req] == 0 {
				return nil, httpx.Unprocessable("AI left out a late arrival. Try generating again.")
			}
		}
		n := coverageN
		if len(requireNewcomers) == 0 && (len(used) == 0 || len(used)%4 != 0 || len(used) < n-3) {
			return nil, httpx.Unprocessable(
				"AI covered only part of the pool. Try generating again.")
		}
		if len(requireNewcomers) > 0 && len(used)%4 != 0 {
			return nil, httpx.Unprocessable("AI made an incomplete top-up group. Try generating again.")
		}
		for _, p := range out {
			seen[matchupKey(idsOf(p.team1), idsOf(p.team2))] = true
		}
		assignReferees(pool, out, waves, playedCounts)
		return out, nil
	}

	for i := 0; i < in.Rounds; i++ {
		// Explicit round number fills one empty round (e.g. after deleting
		// it); otherwise rounds append after the highest existing one.
		// TopUp adds matches for pool members missing from a NON-empty round
		// (late arrivals) without touching its existing matches.
		roundNo := maxRound + i + 1
		coverageN = len(pool)
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
						for rows.Next() {
							var pid string
							if err := rows.Scan(&pid); err == nil && !have[pid] {
								have[pid] = true
								merged = append(merged, pid)
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
			} else if exists {
				return httpx.WriteAppError(c, httpx.Conflict("ROUND_EXISTS", "That round already has matches. Delete it first to regenerate."))
			}
			roundNo = in.Round
		}
		var accepted []pending
		var genErr error
		// Two AI tries only: each one spends provider tokens, and when the
		// model still cannot satisfy the rules the deterministic local draw
		// below finishes the round instead of hammering the rate limit. A
		// provider error (rate limit, auth, network) is not worth a second
		// call, and once it happens the rest of the rounds stay local too.
		for attempt := 0; attempt < 2; attempt++ {
			if aiUnavailable {
				genErr = httpx.BadRequest("AI_PROVIDER_ERROR", "AI provider unavailable.")
				break
			}
			var aiRounds []aiRound
			aiRounds, genErr = generateMatchups(c.Context(), s.cfg, aiPlayers, history, 1, attempt)
			if genErr != nil {
				if httpx.AsAppError(genErr).Code == "AI_PROVIDER_ERROR" {
					aiUnavailable = true
					break
				}
				continue
			}
			if len(aiRounds) == 0 || len(aiRounds[0].Matches) == 0 {
				genErr = httpx.Unprocessable("AI returned no usable matchups. Try generating again.")
				continue
			}
			accepted, genErr = validateRound(aiRounds[0].Matches, roundNo, waveBase)
			if genErr == nil {
				break
			}
		}
		if genErr != nil {
			local, localErr := s.localMatchups(aiPlayers, pool, seen, roundNo, waveBase, wavesPerRound, playedCounts, requireNewcomers, borrowAllow)
			if localErr == nil {
				accepted, genErr = local, nil
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
			INSERT INTO generated_matches (id, event_id, round, wave, team1, team2, referee_id, status)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'UPCOMING')
			RETURNING id::text, event_id::text, round, wave, team1::text, team2::text, status, court,
			          started_at::text, ended_at::text, shuttlecock_used, created_at::text, updated_at::text`,
			id, p.round, p.wave, mustTeamJSON(p.team1), mustTeamJSON(p.team2), p.referee,
		).Scan(&m.ID, &m.EventID, &m.Round, &m.Wave, &t1, &t2, &m.Status, &m.Court,
			&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.CreatedAt, &m.UpdatedAt)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save matchups.")
		}
		m.Team1 = parseTeam(t1)
		m.Team2 = parseTeam(t2)
		if p.referee != nil {
			var refName string
			_ = tx.QueryRow(c.Context(), `SELECT name FROM players WHERE id = $1`, *p.referee).Scan(&refName)
			m.Referee = &Referee{PlayerID: *p.referee, Name: refName}
		}
		saved = append(saved, m)
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save matchups.")
	}
	return httpx.OK(c, http.StatusCreated, saved)
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
		    source_session_id = COALESCE(NULLIF($8, '')::uuid, source_session_id),
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, player_ids::text, source_session_id::text, created_at::text`,
		id, in.Name, in.CourtCount, in.BasePlayed, in.MaxRounds, in.IsPublic, in.ShowGrades, strOrEmpty(in.SourceSessionID),
	).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.SourceSessionID, &ev.CreatedAt)
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
		RETURNING id::text, name, status, court_count, base_played, max_rounds, is_public, show_grades, player_ids::text, created_at::text`,
		id, in.PlayerIDs,
	).Scan(&ev.ID, &ev.Name, &ev.Status, &ev.CourtCount, &ev.BasePlayed, &ev.MaxRounds, &ev.IsPublic, &ev.ShowGrades, &poolRaw, &ev.CreatedAt)
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
