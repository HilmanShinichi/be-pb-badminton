package mabar

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/pb-kecebong/backend/internal/httpx"
)

type Matches struct {
	ID              string   `json:"id"`
	SessionID       string   `json:"session_id"`
	CourtID         *string  `json:"court_id"`
	Sequence        int      `json:"sequence"`
	StartedAt       *string  `json:"started_at"`
	EndedAt         *string  `json:"ended_at"`
	ShuttlecockUsed int      `json:"shuttlecock_used"`
	Players         []string `json:"players"`
}

const matchCols = `m.id::text, m.session_id::text, m.court_id::text, m.sequence,
	m.started_at::text, m.ended_at::text, m.shuttlecock_used`

func (s *Service) ListMatches(c *fiber.Ctx) error {
	id := c.Params("id")
	rows, err := s.db.Query(c.Context(), `
		SELECT `+matchCols+`, COALESCE(
			(SELECT array_agg(mp.player_id::text) FROM match_players mp
			 WHERE mp.match_id = m.id), '{}')
		FROM matches m WHERE m.session_id = $1 ORDER BY m.sequence`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load matches.")
	}
	defer rows.Close()
	matches := []Matches{}
	for rows.Next() {
		var m Matches
		if err := rows.Scan(&m.ID, &m.SessionID, &m.CourtID, &m.Sequence,
			&m.StartedAt, &m.EndedAt, &m.ShuttlecockUsed, &m.Players); err == nil {
			matches = append(matches, m)
		}
	}
	return httpx.OK(c, http.StatusOK, matches)
}

type matchInput struct {
	CourtID         *string `json:"court_id"`
	Sequence        *int    `json:"sequence"`
	ShuttlecockUsed *int    `json:"shuttlecock_used"`
	Players         []struct {
		PlayerID string `json:"player_id"`
		Team     *int   `json:"team"`
	} `json:"players"`
}

// Player names are optional. The mandatory figure on a match is the
// shuttlecock count, which always moves the stock ledger. Names only add
// per-player attribution when the recorder knows who played.
func (in matchInput) validate() error {
	if in.ShuttlecockUsed != nil && *in.ShuttlecockUsed < 0 {
		return httpx.Unprocessable("Shuttlecock count must not be negative.")
	}
	return nil
}

// CreateMatch also writes the inventory USAGE transaction so stock always
// moves with match data; total usage is derived, never re-entered. PRD §56.
func (s *Service) CreateMatch(c *fiber.Ctx) error {
	id := c.Params("id")
	sess, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	var in matchInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if err := in.validate(); err != nil {
		return httpx.WriteAppError(c, err)
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save match.")
	}
	defer tx.Rollback(c.Context())

	used := 0
	if in.ShuttlecockUsed != nil {
		used = *in.ShuttlecockUsed
	}
	seq := 0
	if in.Sequence != nil {
		seq = *in.Sequence
	}
	var matchID string
	err = tx.QueryRow(c.Context(), `
		INSERT INTO matches (id, session_id, court_id, sequence, shuttlecock_used)
		VALUES (gen_random_uuid(), $1, $2,
		        COALESCE($3::int, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM matches WHERE session_id = $1)),
		        $4)
		RETURNING id::text`, id, in.CourtID, nullableInt(seq), used).Scan(&matchID)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save match.")
	}
	for _, p := range in.Players {
		if _, err := uuid.Parse(p.PlayerID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid player ID."))
		}
		if _, err := tx.Exec(c.Context(),
			`INSERT INTO match_players (match_id, player_id, team) VALUES ($1, $2, $3)
			 ON CONFLICT (match_id, player_id) DO UPDATE SET team = EXCLUDED.team`,
			matchID, p.PlayerID, p.Team); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save match players.")
		}
	}
	if used > 0 {
		// Auto-write only when a suitable bucket covers it. Otherwise the
		// ledger is left untouched and the session must be allocated
		// explicitly via shuttle allocation (which supports splits).
		covered := false
		coverProduct := ""
		if productID, err := pickUsageProduct(c.Context(), tx, sess.Type); err == nil {
			if name, stock, serr := productStock(c.Context(), tx, productID); serr != nil {
				covered = true
				coverProduct = productID
			} else if err := checkStockFit(name, stock, 0, int64(used)); err == nil {
				covered = true
				coverProduct = productID
			}
		}
		if covered {
			if _, err := tx.Exec(c.Context(), `
				DELETE FROM shuttlecock_transactions WHERE match_id = $1`, matchID); err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save shuttlecock usage.")
			}
			if _, err := tx.Exec(c.Context(), `
				INSERT INTO shuttlecock_transactions (id, product_id, type, units, session_id, match_id)
				VALUES (gen_random_uuid(), $1, 'USAGE', $2, $3, $4)`, coverProduct, used, id, matchID); err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save shuttlecock usage.")
			}
		}
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save match.")
	}
	return httpx.OK(c, http.StatusCreated, map[string]any{"id": matchID})
}

func (s *Service) UpdateMatch(c *fiber.Ctx) error {
	matchID := c.Params("id")
	var in struct {
		ShuttlecockUsed *int `json:"shuttlecock_used"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if in.ShuttlecockUsed != nil && *in.ShuttlecockUsed < 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Shuttlecock count must not be negative."))
	}
	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update match.")
	}
	defer tx.Rollback(c.Context())

	var sessionID string
	if in.ShuttlecockUsed != nil {
		var oldUsed int
		_ = tx.QueryRow(c.Context(),
			`SELECT COALESCE(shuttlecock_used, 0) FROM matches WHERE id = $1`, matchID).Scan(&oldUsed)
		if err := tx.QueryRow(c.Context(),
			`UPDATE matches SET shuttlecock_used = $2 WHERE id = $1 RETURNING session_id::text`,
			matchID, *in.ShuttlecockUsed).Scan(&sessionID); err != nil {
			return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Matches not found.")
		}
		if *in.ShuttlecockUsed == 0 {
			// keep the USAGE ledger in sync with the edited match figure
			if _, err := tx.Exec(c.Context(),
				`DELETE FROM shuttlecock_transactions WHERE match_id = $1`, matchID); err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update usage.")
			}
		} else {
			sess, ferr := s.fetch(c.Context(), sessionID)
			sessionType := ""
			if ferr == nil {
				sessionType = sess.Type
			}
			if productID, perr := pickUsageProduct(c.Context(), tx, sessionType); perr == nil {
				// Rewrite the ledger only when the bucket covers the new
				// figure (counting the replaced figure back). Otherwise the
				// old rows stay until an explicit shuttle allocation.
				covered := true
				if name, stock, serr := productStock(c.Context(), tx, productID); serr == nil {
					if err := checkStockFit(name, stock, int64(oldUsed), int64(*in.ShuttlecockUsed)); err != nil {
						covered = false
					}
				}
				if covered {
					if _, err := tx.Exec(c.Context(),
						`DELETE FROM shuttlecock_transactions WHERE match_id = $1`, matchID); err != nil {
						return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update usage.")
					}
					_, _ = tx.Exec(c.Context(), `
						INSERT INTO shuttlecock_transactions (id, product_id, type, units, session_id, match_id)
						VALUES (gen_random_uuid(), $1, 'USAGE', $2, $3, $4)`,
						productID, *in.ShuttlecockUsed, sessionID, matchID)
				}
			}
		}
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update match.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"id": matchID})
}

func (s *Service) DeleteMatch(c *fiber.Ctx) error {
	matchID := c.Params("id")
	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete match.")
	}
	defer tx.Rollback(c.Context())
	if _, err := tx.Exec(c.Context(), `DELETE FROM shuttlecock_transactions WHERE match_id = $1`, matchID); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete match.")
	}
	tag, err := tx.Exec(c.Context(), `DELETE FROM matches WHERE id = $1`, matchID)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Matches not found.")
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete match.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) AddMatchPlayer(c *fiber.Ctx) error {
	matchID := c.Params("id")
	var in struct {
		PlayerID string `json:"player_id"`
		Team     *int   `json:"team"`
	}
	if err := httpx.Decode(c, &in); err != nil || in.PlayerID == "" {
		return httpx.Err(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "A player must be selected.")
	}
	if _, err := uuid.Parse(in.PlayerID); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
	}
	_, err := s.db.Exec(c.Context(),
		`INSERT INTO match_players (match_id, player_id, team) VALUES ($1, $2, $3)
		 ON CONFLICT (match_id, player_id) DO UPDATE SET team = EXCLUDED.team`,
		matchID, in.PlayerID, in.Team)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not add player.")
	}
	return httpx.OK(c, http.StatusCreated, map[string]any{"ok": true})
}

func (s *Service) RemoveMatchPlayer(c *fiber.Ctx) error {
	matchID := c.Params("id")
	playerID := c.Params("playerId")
	tag, err := s.db.Exec(c.Context(),
		`DELETE FROM match_players WHERE match_id = $1 AND player_id = $2`, matchID, playerID)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player is not in this match.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

// Summary answers "is this session profitable?" in one call. PRD §37.
func (s *Service) Summary(c *fiber.Ctx) error {
	id := c.Params("id")
	sess, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	var revenue, otherExpense int64
	var unitsUsed int
	var billedPaid int64
	_ = s.db.QueryRow(c.Context(), `
		SELECT
			COALESCE((SELECT SUM(amount) FROM revenues WHERE session_id = $1), 0),
			COALESCE((SELECT SUM(amount) FROM expenses WHERE session_id = $1 AND category <> 'VENUE' AND category <> 'SHUTTLECOCK_PURCHASE'), 0),
			COALESCE((SELECT SUM(units) FROM shuttlecock_transactions WHERE session_id = $1 AND type = 'USAGE'), 0),
			COALESCE((SELECT SUM(total) FROM player_bills WHERE session_id = $1 AND payment_status = 'PAID'), 0)`, id).
		Scan(&revenue, &otherExpense, &unitsUsed, &billedPaid)
	revenue += billedPaid

	var avgPerUnit int64
	_ = s.db.QueryRow(c.Context(), `
		SELECT COALESCE(SUM(t.unit_price * t.units / NULLIF(pr.units_per_pack, 0)) / NULLIF(SUM(t.units), 0), 0)
		FROM shuttlecock_transactions t
		JOIN shuttlecock_products pr ON pr.id = t.product_id
		WHERE t.type = 'PURCHASE'`).Scan(&avgPerUnit)

	var shuttleCost int64
	if avgPerUnit == 0 && sess.ShuttleUnitsPerPack > 0 {
		// No purchase recorded yet: estimate exact from the session pack price.
		shuttleCost = finance.RoundHalfUp(finance.ShuttlecockCost(sess.ShuttlePackPrice, int64(sess.ShuttleUnitsPerPack), int64(unitsUsed)))
	} else {
		shuttleCost = finance.RoundHalfUp(finance.ShuttlecockCost(avgPerUnit, 1, int64(unitsUsed)))
	}
	operatingCost := sess.CourtCost + shuttleCost + otherExpense
	// Upfront member money (commitment fees) pre-funds the courts: spread
	// what the period has actually collected across its planned sessions so
	// one session doesn't look like a loss for courts already paid for.
	var prepaidCourts int64
	if sess.PeriodID != nil {
		var prepaidTotal int64
		var planSessions int
		_ = s.db.QueryRow(c.Context(),
			`SELECT COALESCE(SUM(amount), 0) FROM revenues WHERE period_id = $1 AND source = 'COMMITMENT_FEE'`,
			*sess.PeriodID).Scan(&prepaidTotal)
		_ = s.db.QueryRow(c.Context(),
			`SELECT COALESCE(number_of_sessions, 0) FROM membership_periods WHERE id = $1`,
			*sess.PeriodID).Scan(&planSessions)
		if planSessions <= 0 {
			_ = s.db.QueryRow(c.Context(),
				`SELECT COUNT(*) FROM mabar_sessions WHERE period_id = $1`, *sess.PeriodID).Scan(&planSessions)
		}
		if planSessions > 0 {
			prepaidCourts = prepaidTotal / int64(planSessions)
		}
	}
	profit := revenue + prepaidCourts - operatingCost

	var listed, present, noShow int
	_ = s.db.QueryRow(c.Context(), `
		SELECT COUNT(*) FILTER (WHERE status <> 'NOT_LISTED'),
		       COUNT(*) FILTER (WHERE status = 'PRESENT'),
		       COUNT(*) FILTER (WHERE status = 'NO_SHOW')
		FROM attendances WHERE session_id = $1`, id).Scan(&listed, &present, &noShow)

	return httpx.OK(c, http.StatusOK, map[string]any{
		"session":          sess,
		"revenue":          revenue,
		"billed_paid":      billedPaid,
		"court_cost":       sess.CourtCost,
		"prepaid_courts":   prepaidCourts,
		"shuttlecock_used": unitsUsed,
		"shuttlecock_cost": shuttleCost,
		"other_expense":    otherExpense,
		"operating_cost":   operatingCost,
		"profit":           profit,
		"status":           finance.FinancialStatus(revenue+prepaidCourts, operatingCost),
		"players_present":  present,
		"players_listed":   listed,
		"no_show":          noShow,
	})
}

// GenerateBilling computes one bill per present player. For DAILY_EVENT the
// court cost splits equally and each player pays per shuttlecock they played
// when pricing is FIXED_PER_SHUTTLECOCK. PRD §26/§27. For PERIOD sessions
// each present player is billed at period rates instead: members pay the
// per-visit contribution, guests pay the non-member fee.
func (s *Service) GenerateBilling(c *fiber.Ctx) error {
	id := c.Params("id")
	sess, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	type presentPlayer struct {
		id       string
		isMember bool
	}

	rows, err := s.db.Query(c.Context(), `
		SELECT a.player_id::text, a.is_member FROM attendances a
		WHERE a.session_id = $1 AND a.status = 'PRESENT' ORDER BY a.listed_at`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate bills.")
	}
	present := []presentPlayer{}
	for rows.Next() {
		var p presentPlayer
		if err := rows.Scan(&p.id, &p.isMember); err == nil {
			present = append(present, p)
		}
	}
	rows.Close()

	// Badminton needs at least 4 players on court: refuse to calculate
	// bills for a session that cannot actually be played.
	if len(present) < 4 {
		return httpx.WriteAppError(c, httpx.Unprocessable("An open play session needs at least 4 present players to calculate bills."))
	}

	var memberRate, guestRate int64
	if sess.Type == "PERIOD" {
		if sess.PeriodID == nil {
			return httpx.WriteAppError(c, httpx.Unprocessable("PERIOD sessions must belong to a period."))
		}
		if err := s.db.QueryRow(c.Context(),
			`SELECT member_contribution, non_member_fee FROM membership_periods WHERE id = $1`,
			*sess.PeriodID).Scan(&memberRate, &guestRate); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load period rates.")
		}
	}

	scockRows, err := s.db.Query(c.Context(), `
		SELECT mp.player_id::text, SUM(m.shuttlecock_used)
		FROM match_players mp
		JOIN matches m ON m.id = mp.match_id
		WHERE m.session_id = $1 GROUP BY mp.player_id`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate bills.")
	}
	playerShuttle := map[string]int64{}
	for scockRows.Next() {
		var pid string
		var n int64
		if err := scockRows.Scan(&pid, &n); err == nil {
			playerShuttle[pid] = n
		}
	}
	scockRows.Close()

	// Simple recap also contributes kok per player, so daily billing works
	// without composing 2v2 matches. Both sources are summed to support mixed use.
	simpleRows, err := s.db.Query(c.Context(), `
		SELECT player_id::text, shuttlecock_used
		FROM session_player_stats WHERE session_id = $1`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate bills.")
	}
	for simpleRows.Next() {
		var pid string
		var n int64
		if err := simpleRows.Scan(&pid, &n); err == nil {
			playerShuttle[pid] += n
		}
	}
	simpleRows.Close()

	shares := finance.CourtShares(sess.CourtCost, len(present))
	bills := make([]map[string]any, 0, len(present))
	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate bills.")
	}
	defer tx.Rollback(c.Context())

	for i, p := range present {
		var share, count, contribution, total int64
		if sess.Type == "PERIOD" {
			if p.isMember {
				total = memberRate
			} else {
				total = guestRate
			}
		} else {
			count = playerShuttle[p.id]
			contribution = count * sess.ShuttlecockPrice
			share = shares[i]
			total = finance.DailyPlayerBill(share, count, sess.ShuttlecockPrice, 0)
		}
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO player_bills (id, session_id, player_id, court_share, shuttlecock_count,
			                          shuttlecock_contribution, other_charge, total)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 0, $6)
			ON CONFLICT (session_id, player_id) DO UPDATE SET
				court_share = EXCLUDED.court_share,
				shuttlecock_count = EXCLUDED.shuttlecock_count,
				shuttlecock_contribution = EXCLUDED.shuttlecock_contribution,
				total = EXCLUDED.total`,
			id, p.id, share, count, contribution, total); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate bills.")
		}
		bills = append(bills, map[string]any{
			"player_id": p.id, "court_share": share,
			"shuttlecock_count": count, "shuttlecock_contribution": contribution, "total": total,
		})
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not generate bills.")
	}
	return httpx.OK(c, http.StatusCreated, bills)
}

func (s *Service) ListBilling(c *fiber.Ctx) error {
	id := c.Params("id")
	rows, err := s.db.Query(c.Context(), `
		SELECT b.player_id::text, pl.name, b.court_share, b.shuttlecock_count,
		       b.shuttlecock_contribution, b.other_charge, b.total, b.payment_status, b.payment_method
		FROM player_bills b
		JOIN players pl ON pl.id = b.player_id
		WHERE b.session_id = $1 ORDER BY pl.name`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load bills.")
	}
	defer rows.Close()
	bills := []map[string]any{}
	for rows.Next() {
		var pid, name, status, method string
		var court, contribution, other, total int64
		var count int
		if err := rows.Scan(&pid, &name, &court, &count, &contribution, &other, &total, &status, &method); err == nil {
			bills = append(bills, map[string]any{
				"player_id": pid, "player_name": name, "court_share": court,
				"shuttlecock_count": count, "shuttlecock_contribution": contribution,
				"other_charge": other, "total": total, "payment_status": status,
				"payment_method": method,
			})
		}
	}
	return httpx.OK(c, http.StatusOK, bills)
}

var billStatuses = map[string]bool{
	"UNPAID": true, "PAID": true,
}

var billMethods = map[string]bool{
	"CASH": true, "QRIS": true, "BCA": true,
}

// UpdateBillingStatus updates one player's payment status and/or method
// without recalculating amounts.
func (s *Service) UpdateBillingStatus(c *fiber.Ctx) error {
	id := c.Params("id")
	playerID := c.Params("playerId")
	var in struct {
		PaymentStatus *string `json:"payment_status"`
		PaymentMethod *string `json:"payment_method"`
	}
	if err := httpx.Decode(c, &in); err != nil || (in.PaymentStatus == nil && in.PaymentMethod == nil) {
		return httpx.WriteAppError(c, httpx.Unprocessable("Provide payment_status and/or payment_method."))
	}
	if in.PaymentStatus != nil && !billStatuses[*in.PaymentStatus] {
		return httpx.WriteAppError(c, httpx.Unprocessable("Payment status must be UNPAID or PAID."))
	}
	if in.PaymentMethod != nil && !billMethods[*in.PaymentMethod] {
		return httpx.WriteAppError(c, httpx.Unprocessable("Payment method must be CASH, QRIS, or BCA."))
	}
	set, args := []string{}, []any{id, playerID}
	if in.PaymentStatus != nil {
		args = append(args, *in.PaymentStatus)
		set = append(set, "payment_status = $"+strconv.Itoa(len(args)))
	}
	if in.PaymentMethod != nil {
		args = append(args, *in.PaymentMethod)
		set = append(set, "payment_method = $"+strconv.Itoa(len(args)))
	}
	tag, err := s.db.Exec(c.Context(),
		`UPDATE player_bills SET `+strings.Join(set, ", ")+` WHERE session_id = $1 AND player_id = $2`, args...)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Bill not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
