package dashboard

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

type sessionRow struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Date          string  `json:"date"`
	PeriodName    *string `json:"period_name"`
	VenueName     *string `json:"venue_name"`
	CourtCost     int64   `json:"court_cost"`
	Revenue       int64   `json:"revenue"`
	ShuttleUsed   int64   `json:"shuttlecock_used"`
	ShuttleCost   int64   `json:"shuttlecock_cost"`
	Profit        int64   `json:"profit"`
	Status        string  `json:"status"`
	BillsTotal    int     `json:"bills_total"`
	BillsPaid     int     `json:"bills_paid"`
	PaymentStatus string  `json:"payment_status"`
}

// Get is the landing overview: what is happening, what is next, is the club
// in the black. Kept to a handful of aggregate queries.
func (s *Service) Get(w http.ResponseWriter, r *http.Request) {
	var activePeriodID, activePeriodName *string
	var members int
	_ = s.db.QueryRow(r.Context(), `
		SELECT p.id::text, p.name, COALESCE((SELECT COUNT(*) FROM memberships m
			WHERE m.period_id = p.id AND m.status <> 'WITHDRAWN'), 0)
		FROM membership_periods p WHERE p.status = 'ACTIVE'
		ORDER BY p.start_date DESC LIMIT 1`).Scan(&activePeriodID, &activePeriodName, &members)

	upcoming := []sessionRow{}
	rows, err := s.db.Query(r.Context(), `
		SELECT s.id::text, s.type, s.date::text, p.name, v.name, s.court_cost, 0, 0, 0, 0, ''
		FROM mabar_sessions s
		LEFT JOIN membership_periods p ON p.id = s.period_id
		LEFT JOIN venues v ON v.id = s.venue_id
		LEFT JOIN (SELECT session_id, COUNT(*) AS total,
		                  COUNT(*) FILTER (WHERE payment_status = 'PAID') AS paid
		           FROM player_bills GROUP BY session_id) bl ON bl.session_id = s.id
		WHERE s.date >= CURRENT_DATE
		  AND NOT (s.type = 'DAILY_EVENT' AND COALESCE(bl.total, 0) > 0
		           AND COALESCE(bl.paid, 0) >= bl.total)
		ORDER BY s.date ASC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var row sessionRow
			if err := rows.Scan(&row.ID, &row.Type, &row.Date, &row.PeriodName, &row.VenueName,
				&row.CourtCost, &row.Revenue, &row.ShuttleUsed, &row.ShuttleCost, &row.Profit, &row.Status); err == nil {
				upcoming = append(upcoming, row)
			}
		}
		rows.Close()
	}

	recent := []sessionRow{}
	// Recent sessions support server-side pagination, type filter and
	// search so the table stays usable with many rows. Params:
	// recent_limit (default 5, max 50), recent_offset (default 0),
	// recent_type (PERIOD or DAILY_EVENT), recent_q (matches venue,
	// period, date, type, descriptions).
	recentLimit := 5
	if v, err := strconv.Atoi(r.URL.Query().Get("recent_limit")); err == nil && v > 0 && v <= 50 {
		recentLimit = v
	}
	recentOffset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("recent_offset")); err == nil && v >= 0 {
		recentOffset = v
	}
	recentType := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("recent_type")))
	if recentType == "ALL" {
		recentType = ""
	}
	if recentType != "" && recentType != "PERIOD" && recentType != "DAILY_EVENT" {
		recentType = ""
	}
	recentQ := strings.TrimSpace(r.URL.Query().Get("recent_q"))
	recentArgs := []any{}
	recentFilter := ""
	if recentType != "" {
		recentArgs = append(recentArgs, recentType)
		recentFilter += ` AND s.type = $` + strconv.Itoa(len(recentArgs))
	}
	if recentQ != "" {
		recentArgs = append(recentArgs, "%"+recentQ+"%")
		ph := `$` + strconv.Itoa(len(recentArgs))
		recentFilter += ` AND (p.name ILIKE ` + ph + ` OR v.name ILIKE ` + ph +
			` OR s.date::text ILIKE ` + ph + ` OR s.type ILIKE ` + ph +
			` OR COALESCE(s.venue_description, '') ILIKE ` + ph +
			` OR COALESCE(s.description, '') ILIKE ` + ph + `)`
	}
	var recentTotal int
	_ = s.db.QueryRow(r.Context(), `
		SELECT COUNT(*)
		FROM mabar_sessions s
		LEFT JOIN membership_periods p ON p.id = s.period_id
		LEFT JOIN venues v ON v.id = s.venue_id
		LEFT JOIN (SELECT session_id, COUNT(*) AS total,
		                  COUNT(*) FILTER (WHERE payment_status = 'PAID') AS paid
		           FROM player_bills GROUP BY session_id) bl ON bl.session_id = s.id
		WHERE (s.date < CURRENT_DATE
		   OR (s.date = CURRENT_DATE AND s.type = 'DAILY_EVENT'
		       AND COALESCE(bl.total, 0) > 0 AND COALESCE(bl.paid, 0) >= bl.total))`+recentFilter,
		recentArgs...).Scan(&recentTotal)
	recentArgs = append(recentArgs, recentLimit, recentOffset)
	limitPH := `$` + strconv.Itoa(len(recentArgs)-1)
	offsetPH := `$` + strconv.Itoa(len(recentArgs))
	rows, err = s.db.Query(r.Context(), `
		WITH avg_unit AS (
			SELECT COALESCE(SUM(t.unit_price * t.units / NULLIF(pr.units_per_pack, 0)) / NULLIF(SUM(t.units), 0), 0) AS price
			FROM shuttlecock_transactions t
			JOIN shuttlecock_products pr ON pr.id = t.product_id
			WHERE t.type = 'PURCHASE'
		)
		SELECT s.id::text, s.type, s.date::text, p.name, v.name, s.court_cost,
		       COALESCE(rv.total, 0) + COALESCE(bl.paid_total, 0),
		       COALESCE(sh_u.units, 0),
		       ROUND(COALESCE(sh_u.units, 0) * COALESCE(NULLIF(a.price, 0),
		           s.shuttle_pack_price / NULLIF(s.shuttle_units_per_pack, 0)::float)),
		       COALESCE(rv.total, 0) + COALESCE(bl.paid_total, 0) - s.court_cost
		           - ROUND(COALESCE(sh_u.units, 0) * COALESCE(NULLIF(a.price, 0),
		           s.shuttle_pack_price / NULLIF(s.shuttle_units_per_pack, 0)::float)),
		       '',
		       COALESCE(bl.total, 0), COALESCE(bl.paid, 0)
		FROM mabar_sessions s
		LEFT JOIN membership_periods p ON p.id = s.period_id
		LEFT JOIN venues v ON v.id = s.venue_id
		LEFT JOIN avg_unit a ON TRUE
		LEFT JOIN (SELECT session_id, SUM(amount) AS total FROM revenues GROUP BY session_id) rv ON rv.session_id = s.id
		LEFT JOIN (SELECT session_id, SUM(units) AS units FROM shuttlecock_transactions WHERE type = 'USAGE' GROUP BY session_id) sh_u ON sh_u.session_id = s.id
		LEFT JOIN (SELECT session_id, COUNT(*) AS total, COUNT(*) FILTER (WHERE payment_status = 'PAID') AS paid,
		                  SUM(total) FILTER (WHERE payment_status = 'PAID') AS paid_total
		           FROM player_bills GROUP BY session_id) bl ON bl.session_id = s.id
		WHERE (s.date < CURRENT_DATE
		   OR (s.date = CURRENT_DATE AND s.type = 'DAILY_EVENT'
		       AND COALESCE(bl.total, 0) > 0 AND COALESCE(bl.paid, 0) >= bl.total))`+recentFilter+`
		ORDER BY s.date DESC LIMIT `+limitPH+` OFFSET `+offsetPH, recentArgs...)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load dashboard.")
		return
	}
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.ID, &row.Type, &row.Date, &row.PeriodName, &row.VenueName,
			&row.CourtCost, &row.Revenue, &row.ShuttleUsed, &row.ShuttleCost, &row.Profit, &row.Status,
			&row.BillsTotal, &row.BillsPaid); err == nil {
			row.Status = finance.FinancialStatus(row.Revenue, row.CourtCost+row.ShuttleCost)
			row.PaymentStatus = paymentState(row.Type, row.BillsTotal, row.BillsPaid)
			recent = append(recent, row)
		}
	}
	rows.Close()

	var stock int64
	_ = s.db.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(units) FILTER (WHERE type = 'PURCHASE'), 0)
		       - COALESCE(SUM(units) FILTER (WHERE type = 'USAGE'), 0)
		       + COALESCE(SUM(units) FILTER (WHERE type = 'ADJUSTMENT'), 0)
		FROM shuttlecock_transactions`).Scan(&stock)

	stockByPurpose := map[string]int64{"DAILY": 0, "PERIOD": 0, "GENERAL": 0}
	prows, err := s.db.Query(r.Context(), `
		SELECT p.purpose,
		       COALESCE(SUM(t.units) FILTER (WHERE t.type = 'PURCHASE'), 0)
		       - COALESCE(SUM(t.units) FILTER (WHERE t.type = 'USAGE'), 0)
		       + COALESCE(SUM(t.units) FILTER (WHERE t.type = 'ADJUSTMENT'), 0)
		FROM shuttlecock_products p
		LEFT JOIN shuttlecock_transactions t ON t.product_id = p.id
		GROUP BY p.purpose`)
	if err == nil {
		defer prows.Close()
		for prows.Next() {
			var purpose string
			var total int64
			if err := prows.Scan(&purpose, &total); err == nil {
				stockByPurpose[purpose] += total
			}
		}
	}

	var monthRevenue, monthExpense int64
	_ = s.db.QueryRow(r.Context(), `
		SELECT COALESCE((SELECT SUM(amount) FROM revenues WHERE occurred_at >= date_trunc('month', CURRENT_DATE)), 0),
		       COALESCE((SELECT SUM(amount) FROM expenses WHERE occurred_at >= date_trunc('month', CURRENT_DATE)), 0)`).
		Scan(&monthRevenue, &monthExpense)

	// Monthly cash flow split by scope so Daily and Period money never mix.
	// Precedence per row: linked session type wins, then period link, then
	// the purchased product purpose (stock buys without a session), else umum.
	monthScope := map[string]map[string]int64{
		"daily":   {"revenue": 0, "expense": 0},
		"period":  {"revenue": 0, "expense": 0},
		"general": {"revenue": 0, "expense": 0},
	}
	var dRev, pRev, gRev, dExp, pExp, gExp int64
	_ = s.db.QueryRow(r.Context(), `
		SELECT
			COALESCE(SUM(r.amount) FILTER (WHERE s.type = 'DAILY_EVENT'), 0),
			COALESCE(SUM(r.amount) FILTER (WHERE s.type = 'PERIOD' OR (s.id IS NULL AND r.period_id IS NOT NULL)), 0),
			COALESCE(SUM(r.amount) FILTER (WHERE s.id IS NULL AND r.period_id IS NULL), 0)
		FROM revenues r LEFT JOIN mabar_sessions s ON s.id = r.session_id
		WHERE r.occurred_at >= date_trunc('month', CURRENT_DATE)`).
		Scan(&dRev, &pRev, &gRev)
	_ = s.db.QueryRow(r.Context(), `
		SELECT
			COALESCE(SUM(e.amount) FILTER (WHERE s.type = 'DAILY_EVENT' OR (s.id IS NULL AND e.period_id IS NULL AND p.purpose = 'DAILY')), 0),
			COALESCE(SUM(e.amount) FILTER (WHERE s.type = 'PERIOD' OR (s.id IS NULL AND e.period_id IS NOT NULL) OR (s.id IS NULL AND e.period_id IS NULL AND p.purpose = 'PERIOD')), 0),
			COALESCE(SUM(e.amount) FILTER (WHERE s.id IS NULL AND e.period_id IS NULL AND COALESCE(p.purpose, 'GENERAL') = 'GENERAL'), 0)
		FROM expenses e
		LEFT JOIN mabar_sessions s ON s.id = e.session_id
		LEFT JOIN shuttlecock_products p ON p.id = e.product_id
		WHERE e.occurred_at >= date_trunc('month', CURRENT_DATE)`).
		Scan(&dExp, &pExp, &gExp)
	monthScope["daily"]["revenue"], monthScope["period"]["revenue"], monthScope["general"]["revenue"] = dRev, pRev, gRev
	monthScope["daily"]["expense"], monthScope["period"]["expense"], monthScope["general"]["expense"] = dExp, pExp, gExp

	// Paid player bills are cash in, but marking a bill PAID never writes a
	// revenues row — so without this the month scope misses bill collections
	// while session cards (which add paid_total) show them. Same rule as the
	// session cards: billed revenue counts once paid.
	var dBilled, pBilled int64
	_ = s.db.QueryRow(r.Context(), `
		SELECT
			COALESCE(SUM(b.total) FILTER (WHERE s.type = 'DAILY_EVENT'), 0),
			COALESCE(SUM(b.total) FILTER (WHERE s.type = 'PERIOD'), 0)
		FROM player_bills b
		JOIN mabar_sessions s ON s.id = b.session_id
		WHERE b.payment_status = 'PAID'
		  AND s.date >= date_trunc('month', CURRENT_DATE)::date`).
		Scan(&dBilled, &pBilled)
	monthScope["daily"]["revenue"] += dBilled
	monthScope["period"]["revenue"] += pBilled
	monthRevenue += dBilled + pBilled

	httpx.OK(w, http.StatusOK, map[string]any{
		"active_period": map[string]any{
			"id": activePeriodID, "name": activePeriodName, "members": members,
		},
		"upcoming_sessions": upcoming,
		"recent_sessions":   recent,
		"recent_total":      recentTotal,
		"recent_limit":      recentLimit,
		"recent_offset":     recentOffset,
		"shuttlecock_stock": stock,
		"stock_by_purpose":  stockByPurpose,
		"month_revenue":     monthRevenue,
		"month_expense":     monthExpense,
		"month_cash_flow":   monthRevenue - monthExpense,
		"month_daily": map[string]any{
			"revenue": monthScope["daily"]["revenue"], "expense": monthScope["daily"]["expense"],
			"cash_flow": monthScope["daily"]["revenue"] - monthScope["daily"]["expense"],
		},
		"month_period": map[string]any{
			"revenue": monthScope["period"]["revenue"], "expense": monthScope["period"]["expense"],
			"cash_flow": monthScope["period"]["revenue"] - monthScope["period"]["expense"],
		},
		"month_general": map[string]any{
			"revenue": monthScope["general"]["revenue"], "expense": monthScope["general"]["expense"],
			"cash_flow": monthScope["general"]["revenue"] - monthScope["general"]["expense"],
		},
	})
}

// paymentState tells whether a daily session is fully paid off. Period
// sessions have no per-player bills, so they report no payment state.
func paymentState(sessionType string, total, paid int) string {
	if sessionType != "DAILY_EVENT" || total == 0 {
		return "NO_BILLS"
	}
	if paid >= total {
		return "SETTLED"
	}
	return "PENDING"
}
