package finance

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/gofiber/fiber/v2"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

var revenueSources = map[string]bool{
	"COMMITMENT_FEE": true, "ATTENDANCE_CONTRIBUTION": true, "NON_MEMBER_FEE": true,
	"DAILY_EVENT_CONTRIBUTION": true, "OTHER": true,
}

var expenseCategories = map[string]bool{
	"VENUE": true, "SHUTTLECOCK_PURCHASE": true, "EQUIPMENT": true, "REFUND": true, "OTHER": true,
}

func idPtr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Summary gives the finance page its numbers for an optional period or
// session scope. Revenue here counts recorded income, not billed amounts.
func (s *Service) Summary(c *fiber.Ctx) error {
	periodID := c.Query("period_id")
	sessionID := c.Query("session_id")

	revWhere, expWhere := "TRUE", "TRUE"
	args := []any{}
	if periodID != "" {
		if _, err := uuid.Parse(periodID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid period ID."))
		}
		args = append(args, periodID)
		revWhere = "period_id = $" + strconv.Itoa(len(args))
		expWhere = "period_id = $" + strconv.Itoa(len(args))
	} else if sessionID != "" {
		if _, err := uuid.Parse(sessionID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
		}
		args = append(args, sessionID)
		revWhere = "session_id = $" + strconv.Itoa(len(args))
		expWhere = "session_id = $" + strconv.Itoa(len(args))
	}

	var totalRevenue int64
	if err := s.db.QueryRow(c.Context(),
		`SELECT COALESCE(SUM(amount), 0) FROM revenues WHERE `+revWhere, args...).Scan(&totalRevenue); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not compute revenue.")
	}

	rows, err := s.db.Query(c.Context(),
		`SELECT category, SUM(amount) FROM expenses WHERE `+expWhere+` GROUP BY category ORDER BY category`, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not compute expenses.")
	}
	defer rows.Close()
	byCategory := map[string]int64{}
	var totalExpense int64
	for rows.Next() {
		var cat string
		var amt int64
		if err := rows.Scan(&cat, &amt); err == nil {
			byCategory[cat] = amt
			totalExpense += amt
		}
	}

	var unitsUsed int64
	var avgPerUnit int64
	if err := s.db.QueryRow(c.Context(), `
		SELECT COALESCE(SUM(units), 0) FROM shuttlecock_transactions WHERE type = 'USAGE'`).Scan(&unitsUsed); err == nil {
		_ = s.db.QueryRow(c.Context(), `
			SELECT COALESCE(SUM(t.unit_price * t.units / NULLIF(pr.units_per_pack, 0)) / NULLIF(SUM(t.units), 0), 0)
			FROM shuttlecock_transactions t
			JOIN shuttlecock_products pr ON pr.id = t.product_id
			WHERE t.type = 'PURCHASE'`).Scan(&avgPerUnit)
	}
	shuttleUsageCost := finance.RoundHalfUp(finance.ShuttlecockCost(avgPerUnit, 1, unitsUsed))

	venue := byCategory["VENUE"]
	operatingCost := venue + shuttleUsageCost
	profit := totalRevenue - operatingCost

	return httpx.OK(c, http.StatusOK, map[string]any{
		"total_revenue":          totalRevenue,
		"total_expense":          totalExpense,
		"expense_by_category":    byCategory,
		"venue_cost":             venue,
		"shuttlecock_usage_cost": shuttleUsageCost,
		"operating_cost":         operatingCost,
		"operating_profit":       profit,
		"status":                 finance.FinancialStatus(totalRevenue, operatingCost),
		"cash_in":                totalRevenue,
		"cash_out":               totalExpense,
		"cash_flow":              totalRevenue - totalExpense,
		"shuttlecock_used":       unitsUsed,
	})
}

type txRow struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Category   string  `json:"category"`
	PlayerID   *string `json:"player_id"`
	PlayerName *string `json:"player_name"`
	Amount     int64   `json:"amount"`
	Note       *string `json:"note"`
	SessionID  *string `json:"session_id"`
	OccurredAt string  `json:"occurred_at"`
}

func (s *Service) Transactions(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(), `
		SELECT rv.id::text, 'REVENUE', rv.source, rv.player_id::text, pl.name, rv.amount, rv.note,
		       rv.session_id::text, rv.occurred_at::text
		FROM revenues rv LEFT JOIN players pl ON pl.id = rv.player_id
		UNION ALL
		SELECT ex.id::text, 'EXPENSE', ex.category, NULL, NULL, ex.amount, ex.note,
		       ex.session_id::text, ex.occurred_at::text
		FROM expenses ex
		ORDER BY occurred_at DESC LIMIT 200`)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load transactions.")
	}
	defer rows.Close()
	list := []txRow{}
	for rows.Next() {
		var t txRow
		if err := rows.Scan(&t.ID, &t.Kind, &t.Category, &t.PlayerID, &t.PlayerName, &t.Amount,
			&t.Note, &t.SessionID, &t.OccurredAt); err == nil {
			list = append(list, t)
		}
	}
	return httpx.OK(c, http.StatusOK, list)
}

type revenueInput struct {
	SessionID string `json:"session_id"`
	PeriodID  string `json:"period_id"`
	Source    string `json:"source"`
	PlayerID  string `json:"player_id"`
	Amount    int64  `json:"amount"`
	Note      string `json:"note"`
}

func (s *Service) CreateRevenue(c *fiber.Ctx) error {
	var in revenueInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if !revenueSources[in.Source] {
		return httpx.WriteAppError(c, httpx.Unprocessable("Invalid revenue source."))
	}
	if in.Amount <= 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Amount must be greater than 0."))
	}
	if in.SessionID != "" {
		if _, err := uuid.Parse(in.SessionID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
		}
	}
	var id string
	err := s.db.QueryRow(c.Context(), `
		INSERT INTO revenues (id, session_id, period_id, source, player_id, amount, note)
		VALUES (gen_random_uuid(), $1, $2, $3, NULLIF($4, '')::uuid, $5, NULLIF($6, ''))
		RETURNING id::text`,
		idPtr(in.SessionID), idPtr(in.PeriodID), in.Source, in.PlayerID, in.Amount, in.Note).Scan(&id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save revenue.")
	}
	return httpx.OK(c, http.StatusCreated, map[string]any{"id": id})
}

type expenseInput struct {
	SessionID string `json:"session_id"`
	PeriodID  string `json:"period_id"`
	Category  string `json:"category"`
	Amount    int64  `json:"amount"`
	Note      string `json:"note"`
}

func (s *Service) CreateExpense(c *fiber.Ctx) error {
	var in expenseInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if !expenseCategories[in.Category] {
		return httpx.WriteAppError(c, httpx.Unprocessable("Invalid expense category."))
	}
	if in.Amount <= 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Amount must be greater than 0."))
	}
	if in.SessionID != "" {
		if _, err := uuid.Parse(in.SessionID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
		}
	}
	var id string
	err := s.db.QueryRow(c.Context(), `
		INSERT INTO expenses (id, session_id, period_id, category, amount, note)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NULLIF($5, ''))
		RETURNING id::text`,
		idPtr(in.SessionID), idPtr(in.PeriodID), in.Category, in.Amount, in.Note).Scan(&id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save expense.")
	}
	return httpx.OK(c, http.StatusCreated, map[string]any{"id": id})
}
