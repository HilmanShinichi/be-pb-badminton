package mabar

import (
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type SessionExpense struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Category   string `json:"category"`
	Amount     int64  `json:"amount"`
	Note       string `json:"note"`
	OccurredAt string `json:"occurred_at"`
}

type addExpenseInput struct {
	Description string `json:"description"`
	Amount      int64  `json:"amount"`
	Category    string `json:"category"`
}

func (s *Service) ListExpenses(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	if _, err := uuid.Parse(sessionID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
	}

	rows, err := s.db.Query(c.Context(), `
		SELECT id::text, session_id::text, category, amount, COALESCE(note, ''), occurred_at::text
		FROM expenses
		WHERE session_id = $1 AND category <> 'SHUTTLECOCK_PURCHASE'
		ORDER BY occurred_at ASC`, sessionID)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load session expenses.")
	}
	defer rows.Close()

	items := []SessionExpense{}
	for rows.Next() {
		var item SessionExpense
		if err := rows.Scan(&item.ID, &item.SessionID, &item.Category, &item.Amount, &item.Note, &item.OccurredAt); err == nil {
			items = append(items, item)
		}
	}
	return httpx.OK(c, http.StatusOK, items)
}

func (s *Service) AddExpense(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	sess, err := s.fetch(c.Context(), sessionID)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	var in addExpenseInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}

	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Expense description is required."))
	}
	if in.Amount <= 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Expense amount must be greater than 0."))
	}

	cat := strings.ToUpper(strings.TrimSpace(in.Category))
	if cat == "" {
		cat = "OTHER"
	}

	var item SessionExpense
	err = s.db.QueryRow(c.Context(), `
		INSERT INTO expenses (id, session_id, period_id, category, amount, note, occurred_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, now())
		RETURNING id::text, session_id::text, category, amount, COALESCE(note, ''), occurred_at::text`,
		sess.ID, sess.PeriodID, cat, in.Amount, desc).
		Scan(&item.ID, &item.SessionID, &item.Category, &item.Amount, &item.Note, &item.OccurredAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save expense.")
	}

	return httpx.OK(c, http.StatusCreated, item)
}

func (s *Service) DeleteExpense(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	expenseID := c.Params("expenseId")

	if _, err := uuid.Parse(sessionID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
	}
	if _, err := uuid.Parse(expenseID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid expense ID."))
	}

	tag, err := s.db.Exec(c.Context(), `
		DELETE FROM expenses WHERE id = $1 AND session_id = $2`,
		expenseID, sessionID)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete expense.")
	}
	if tag.RowsAffected() == 0 {
		return httpx.WriteAppError(c, httpx.NotFound("Expense not found."))
	}

	return httpx.OK(c, http.StatusOK, map[string]bool{"deleted": true})
}
