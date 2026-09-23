package mabar

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pb-kecebong/backend/internal/httpx"
)

// rowQuerier covers *pgxpool.Pool and pgx.Tx, both of which can run the
// product lookup below.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pickUsageProduct selects which stock bucket a session consumes: an active
// product whose purpose matches the session type (DAILY/PERIOD), falling back
// to shared GENERAL stock only. Buckets never cross: a DAILY product cannot
// be consumed by a period session and vice versa.
func pickUsageProduct(ctx context.Context, q rowQuerier, sessionType string) (string, error) {
	purpose := "GENERAL"
	if sessionType == "DAILY_EVENT" {
		purpose = "DAILY"
	} else if sessionType == "PERIOD" {
		purpose = "PERIOD"
	}
	var id string
	if err := q.QueryRow(ctx, `
		SELECT id::text FROM shuttlecock_products
		WHERE active AND purpose = $1 ORDER BY created_at LIMIT 1`, purpose).Scan(&id); err == nil {
		return id, nil
	}
	if purpose != "GENERAL" {
		if err := q.QueryRow(ctx, `
			SELECT id::text FROM shuttlecock_products
			WHERE active AND purpose = 'GENERAL' ORDER BY created_at LIMIT 1`).Scan(&id); err == nil {
			return id, nil
		}
	}
	return "", fmt.Errorf("no active shuttlecock product for %s sessions (not even shared stock)", purpose)
}

// suitablePurpose reports whether a product bucket may be consumed by a
// session type. GENERAL buckets are shared; DAILY and PERIOD never cross.
func suitablePurpose(productPurpose, sessionType string) bool {
	if productPurpose == "GENERAL" {
		return true
	}
	if sessionType == "DAILY_EVENT" {
		return productPurpose == "DAILY"
	}
	if sessionType == "PERIOD" {
		return productPurpose == "PERIOD"
	}
	return false
}

// productStock returns the name and current ledger stock
// (purchases − usage + adjustments) of one product.
func productStock(ctx context.Context, q rowQuerier, productID string) (string, int64, error) {
	var name string
	var stock int64
	err := q.QueryRow(ctx, `
		SELECT p.name,
		       COALESCE(SUM(t.units) FILTER (WHERE t.type = 'PURCHASE'), 0)
		       - COALESCE(SUM(t.units) FILTER (WHERE t.type = 'USAGE'), 0)
		       + COALESCE(SUM(t.units) FILTER (WHERE t.type = 'ADJUSTMENT'), 0)
		FROM shuttlecock_products p
		LEFT JOIN shuttlecock_transactions t ON t.product_id = p.id
		WHERE p.id = $1
		GROUP BY p.name`, productID).Scan(&name, &stock)
	return name, stock, err
}

// checkStockFit refuses usage that would drive a product negative, so a
// session can never consume shuttles nobody owns. Freed units (records of
// the same session being replaced) count back as available.
func checkStockFit(name string, stock, freed, need int64) error {
	if need <= stock+freed {
		return nil
	}
	return httpx.Unprocessable(fmt.Sprintf(
		"Not enough '%s' stock: need %d pcs, only %d available. Record a purchase first.",
		name, need, stock+freed))
}

type allocationItem struct {
	ProductID string `json:"product_id"`
	Units     int    `json:"units"`
}

// SaveAllocation replaces a whole session's USAGE ledger with an explicit
// per-product split (e.g. 12 pcs from one tube brand, 1 pc from another).
// The items must add up to the full session usage recorded in matches and
// simple recap, and every bucket must actually hold its share.
func (s *Service) SaveAllocation(c *fiber.Ctx) error {
	id := c.Params("id")
	sess, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	var in struct {
		Items []allocationItem `json:"items"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	seen := map[string]bool{}
	total := 0
	for _, it := range in.Items {
		if _, err := uuid.Parse(it.ProductID); err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid product ID."))
		}
		if it.Units <= 0 {
			return httpx.WriteAppError(c, httpx.Unprocessable("Allocation units must be greater than 0."))
		}
		if seen[it.ProductID] {
			return httpx.WriteAppError(c, httpx.Unprocessable("Each product can only appear once in an allocation."))
		}
		seen[it.ProductID] = true
		var active bool
		var pname, purpose string
		if err := s.db.QueryRow(c.Context(),
			`SELECT active, name, purpose FROM shuttlecock_products WHERE id = $1`, it.ProductID).Scan(&active, &pname, &purpose); err != nil || !active {
			return httpx.WriteAppError(c, httpx.Unprocessable("Shuttlecock product not found or inactive."))
		}
		if !suitablePurpose(purpose, sess.Type) {
			want := "daily"
			if sess.Type == "PERIOD" {
				want = "period"
			}
			return httpx.WriteAppError(c, httpx.Unprocessable(fmt.Sprintf(
				"'%s' is for %s sessions and cannot be used in this %s session.", pname, strings.ToLower(purpose), want)))
		}
		total += it.Units
	}

	var usage int
	_ = s.db.QueryRow(c.Context(), `
		SELECT COALESCE((SELECT SUM(shuttlecock_used) FROM matches WHERE session_id = $1), 0)
		     + COALESCE((SELECT SUM(shuttlecock_used) FROM session_player_stats WHERE session_id = $1), 0)`,
		id).Scan(&usage)
	if total != usage {
		return httpx.WriteAppError(c, httpx.Unprocessable(fmt.Sprintf(
			"Allocation (%d pcs) must cover the full session usage of %d pcs.", total, usage)))
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save allocation.")
	}
	defer tx.Rollback(c.Context())

	freed := map[string]int64{}
	frows, err := tx.Query(c.Context(), `
		SELECT product_id::text, COALESCE(SUM(units), 0)
		FROM shuttlecock_transactions
		WHERE session_id = $1 AND type = 'USAGE' GROUP BY product_id`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save allocation.")
	}
	for frows.Next() {
		var pid string
		var n int64
		if err := frows.Scan(&pid, &n); err == nil {
			freed[pid] = n
		}
	}
	frows.Close()

	names := map[string]string{}
	for _, it := range in.Items {
		name, stock, serr := productStock(c.Context(), tx, it.ProductID)
		if serr != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save allocation.")
		}
		if err := checkStockFit(name, stock, freed[it.ProductID], int64(it.Units)); err != nil {
			return httpx.WriteAppError(c, err)
		}
		names[it.ProductID] = name
	}

	if _, err := tx.Exec(c.Context(),
		`DELETE FROM shuttlecock_transactions WHERE session_id = $1 AND type = 'USAGE'`, id); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save allocation.")
	}
	out := make([]map[string]any, 0, len(in.Items))
	for _, it := range in.Items {
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO shuttlecock_transactions (id, product_id, type, units, session_id, match_id, note)
			VALUES (gen_random_uuid(), $1, 'USAGE', $2, $3, NULL, 'manual-allocation')`,
			it.ProductID, it.Units, id); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save allocation.")
		}
		out = append(out, map[string]any{
			"product_id": it.ProductID, "product_name": names[it.ProductID], "units": it.Units,
		})
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save allocation.")
	}
	return httpx.OK(c, http.StatusOK, out)
}
