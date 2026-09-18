package inventory

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

type Product struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UnitName      string `json:"unit_name"`
	UnitsPerPack  int    `json:"units_per_pack"`
	PurchasePrice int64  `json:"purchase_price"`
	Purpose       string `json:"purpose"`
	Active        bool   `json:"active"`
	Stock         int64  `json:"stock"`
}

var productPurposes = map[string]bool{"GENERAL": true, "DAILY": true, "PERIOD": true}

func (s *Service) ListProducts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT p.id::text, p.name, p.unit_name, p.units_per_pack, p.purchase_price, p.purpose, p.active,
		       COALESCE(stock.total, 0)
		FROM shuttlecock_products p
		LEFT JOIN (
			SELECT product_id,
			       COALESCE(SUM(units) FILTER (WHERE type = 'PURCHASE'), 0)
			       - COALESCE(SUM(units) FILTER (WHERE type = 'USAGE'), 0)
			       + COALESCE(SUM(units) FILTER (WHERE type = 'ADJUSTMENT'), 0) AS total
			FROM shuttlecock_transactions GROUP BY product_id
		) stock ON stock.product_id = p.id
		ORDER BY p.active DESC, p.created_at`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load products.")
		return
	}
	defer rows.Close()
	products := []Product{}
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitName, &p.UnitsPerPack, &p.PurchasePrice, &p.Purpose, &p.Active, &p.Stock); err == nil {
			products = append(products, p)
		}
	}
	httpx.OK(w, http.StatusOK, products)
}

func (s *Service) CreateProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string `json:"name"`
		UnitName      string `json:"unit_name"`
		UnitsPerPack  int    `json:"units_per_pack"`
		PurchasePrice int64  `json:"purchase_price"`
		Purpose       string `json:"purpose"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if in.Name == "" {
		httpx.WriteAppError(w, httpx.Unprocessable("Nama produk is required."))
		return
	}
	if in.UnitsPerPack <= 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Units per pack must be greater than 0."))
		return
	}
	if in.PurchasePrice < 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Purchase price must not be negative."))
		return
	}
	if in.UnitName == "" {
		in.UnitName = "pc"
	}
	if !productPurposes[in.Purpose] {
		in.Purpose = "GENERAL"
	}
	var p Product
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO shuttlecock_products (id, name, unit_name, units_per_pack, purchase_price, purpose)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
		RETURNING id::text, name, unit_name, units_per_pack, purchase_price, purpose, active, 0::bigint`,
		in.Name, in.UnitName, in.UnitsPerPack, in.PurchasePrice, in.Purpose,
	).Scan(&p.ID, &p.Name, &p.UnitName, &p.UnitsPerPack, &p.PurchasePrice, &p.Purpose, &p.Active, &p.Stock)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save product.")
		return
	}
	httpx.OK(w, http.StatusCreated, p)
}

// UpdateProduct fixes wrong input (name, pack size, price, purpose) without
// touching stock history, which stays derived from transactions.
func (s *Service) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid product ID."))
		return
	}
	var in struct {
		Name          *string `json:"name"`
		UnitName      *string `json:"unit_name"`
		UnitsPerPack  *int    `json:"units_per_pack"`
		PurchasePrice *int64  `json:"purchase_price"`
		Purpose       *string `json:"purpose"`
		Active        *bool   `json:"active"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if in.Name != nil && *in.Name == "" {
		httpx.WriteAppError(w, httpx.Unprocessable("Product name is required."))
		return
	}
	if in.UnitsPerPack != nil && *in.UnitsPerPack <= 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Units per pack must be greater than 0."))
		return
	}
	if in.PurchasePrice != nil && *in.PurchasePrice < 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Purchase price must not be negative."))
		return
	}
	if in.Purpose != nil && !productPurposes[*in.Purpose] {
		httpx.WriteAppError(w, httpx.Unprocessable("Purpose must be GENERAL, DAILY, or PERIOD."))
		return
	}
	var cur Product
	if err := s.db.QueryRow(r.Context(), `
		SELECT id::text, name, unit_name, units_per_pack, purchase_price, purpose, active, 0::bigint
		FROM shuttlecock_products WHERE id = $1`, id).Scan(
		&cur.ID, &cur.Name, &cur.UnitName, &cur.UnitsPerPack, &cur.PurchasePrice, &cur.Purpose, &cur.Active, &cur.Stock); err != nil {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Product not found.")
		return
	}
	if in.Name != nil {
		cur.Name = *in.Name
	}
	if in.UnitName != nil && *in.UnitName != "" {
		cur.UnitName = *in.UnitName
	}
	if in.UnitsPerPack != nil {
		cur.UnitsPerPack = *in.UnitsPerPack
	}
	if in.PurchasePrice != nil {
		cur.PurchasePrice = *in.PurchasePrice
	}
	if in.Purpose != nil {
		cur.Purpose = *in.Purpose
	}
	if in.Active != nil {
		cur.Active = *in.Active
	}
	tag, err := s.db.Exec(r.Context(), `
		UPDATE shuttlecock_products SET name = $2, unit_name = $3, units_per_pack = $4,
		       purchase_price = $5, purpose = $6, active = $7 WHERE id = $1`,
		id, cur.Name, cur.UnitName, cur.UnitsPerPack, cur.PurchasePrice, cur.Purpose, cur.Active)
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Product not found.")
		return
	}
	httpx.OK(w, http.StatusOK, cur)
}

// DeleteProduct removes a product. With history it refuses unless
// ?force=true, which hard-deletes its transactions too (cash expenses stay
// as books history).
func (s *Service) DeleteProduct(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid product ID."))
		return
	}
	var txs int
	_ = s.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM shuttlecock_transactions WHERE product_id = $1`, id).Scan(&txs)
	if txs > 0 && r.URL.Query().Get("force") != "true" {
		httpx.WriteAppError(w, httpx.Conflict("PRODUCT_HAS_HISTORY",
			"Product already has stock transactions. Fix it with edit, or delete with force to wipe its transactions."))
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete product.")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM shuttlecock_transactions WHERE product_id = $1`, id); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete product.")
		return
	}
	// Purchases also write a linked expense row; force means wiping those too,
	// otherwise the expenses FK would block the product delete below.
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM expenses WHERE product_id = $1`, id); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete product.")
		return
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM shuttlecock_products WHERE id = $1`, id)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete product.")
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Product not found.")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete product.")
		return
	}
	httpx.OK(w, http.StatusOK, map[string]any{"ok": true})
}

// Purchase records incoming stock and the cash-out expense in one
// transaction so inventory and finance never drift apart. Stock can arrive
// either by full packs ({packs, unit_price per pack}) or as loose pieces
// ({pcs, pcs_total}: total rupiah paid). Loose prices are normalized to a
// per-pack-equivalent unit_price so average-cost math stays exact.
func (s *Service) Purchase(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID string  `json:"product_id"`
		Packs     int     `json:"packs"`
		UnitPrice int64   `json:"unit_price"`
		Pcs       int     `json:"pcs"`
		PcsTotal  int64   `json:"pcs_total"`
		SessionID *string `json:"session_id"`
		Note      *string `json:"note"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if _, err := uuid.Parse(in.ProductID); err != nil {
		httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid product ID."))
		return
	}
	loose := in.Pcs > 0
	if loose && in.Packs > 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Use either packs or loose pcs, not both."))
		return
	}
	if !loose && in.Packs <= 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Pack count must be greater than 0."))
		return
	}
	if loose && in.PcsTotal < 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Total price must not be negative."))
		return
	}
	if !loose && in.UnitPrice < 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Price per pack must not be negative."))
		return
	}

	var unitsPerPack int
	var periodID *string
	if err := s.db.QueryRow(r.Context(),
		`SELECT units_per_pack FROM shuttlecock_products WHERE id = $1`, in.ProductID).Scan(&unitsPerPack); err != nil {
		httpx.WriteAppError(w, httpx.NotFound("Product not found."))
		return
	}
	if in.SessionID != nil {
		if _, err := uuid.Parse(*in.SessionID); err != nil {
			httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
			return
		}
		err := s.db.QueryRow(r.Context(),
			`SELECT period_id::text FROM mabar_sessions WHERE id = $1`, *in.SessionID).Scan(&periodID)
		if err != nil {
			httpx.WriteAppError(w, httpx.NotFound("Session not found."))
			return
		}
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not record purchase.")
		return
	}
	defer tx.Rollback(r.Context())

	units := int64(in.Packs * unitsPerPack)
	total := int64(in.Packs) * in.UnitPrice
	storedPrice := in.UnitPrice
	if loose {
		units = int64(in.Pcs)
		total = in.PcsTotal
		if unitsPerPack > 0 {
			storedPrice = total * int64(unitsPerPack) / int64(in.Pcs)
		} else {
			storedPrice = total
		}
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO shuttlecock_transactions (id, product_id, type, units, unit_price, session_id, note)
		VALUES (gen_random_uuid(), $1, 'PURCHASE', $2, $3, $4, $5)`,
		in.ProductID, units, storedPrice, in.SessionID, in.Note); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not record purchase.")
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO expenses (id, session_id, period_id, product_id, category, amount, note)
		VALUES (gen_random_uuid(), $1, $2, $4, 'SHUTTLECOCK_PURCHASE', $3, $5)`,
		in.SessionID, periodID, total, in.ProductID, in.Note); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not record purchase.")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not record purchase.")
		return
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"units": units, "total": total})
}

// Adjust handles stock corrections (broken tubes, count fixes) without
// touching cash: an adjustment changes units, not money.
func (s *Service) Adjust(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID string  `json:"product_id"`
		Units     int     `json:"units"`
		Note      *string `json:"note"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if _, err := uuid.Parse(in.ProductID); err != nil {
		httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid product ID."))
		return
	}
	if in.Units == 0 {
		httpx.WriteAppError(w, httpx.Unprocessable("Adjustment amount must not be 0."))
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO shuttlecock_transactions (id, product_id, type, units, note)
		VALUES (gen_random_uuid(), $1, 'ADJUSTMENT', $2, $3)`, in.ProductID, in.Units, in.Note); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not record adjustment.")
		return
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"units": in.Units})
}

func (s *Service) Transactions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT t.id::text, t.product_id::text, p.name, t.type, t.units, t.unit_price, t.session_id::text,
		       t.match_id::text, t.note, t.occurred_at::text
		FROM shuttlecock_transactions t
		JOIN shuttlecock_products p ON p.id = t.product_id
		ORDER BY t.occurred_at DESC LIMIT 200`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load transactions.")
		return
	}
	defer rows.Close()
	type tx struct {
		ID        string  `json:"id"`
		ProductID string  `json:"product_id"`
		Product   string  `json:"product"`
		Type      string  `json:"type"`
		Units     int     `json:"units"`
		UnitPrice *int64  `json:"unit_price"`
		SessionID *string `json:"session_id"`
		MatchID   *string `json:"match_id"`
		Note      *string `json:"note"`
		At        string  `json:"occurred_at"`
	}
	list := []tx{}
	for rows.Next() {
		var t tx
		if err := rows.Scan(&t.ID, &t.ProductID, &t.Product, &t.Type, &t.Units, &t.UnitPrice, &t.SessionID,
			&t.MatchID, &t.Note, &t.At); err == nil {
			list = append(list, t)
		}
	}
	httpx.OK(w, http.StatusOK, list)
}
