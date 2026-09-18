package mabar

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type SimpleStat struct {
	SessionID      string `json:"session_id"`
	PlayerID       string `json:"player_id"`
	PlayerName     string `json:"player_name"`
	PlayCount      int    `json:"play_count"`
	ShuttlecockUsed int   `json:"shuttlecock_used"`
}

func (s *Service) ListSimpleStats(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rows, err := s.db.Query(r.Context(), `
		SELECT s.session_id::text, s.player_id::text, pl.name, s.play_count, s.shuttlecock_used
		FROM session_player_stats s
		JOIN players pl ON pl.id = s.player_id
		WHERE s.session_id = $1 ORDER BY pl.name`, id)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load simple recap.")
		return
	}
	defer rows.Close()
	list := []SimpleStat{}
	for rows.Next() {
		var st SimpleStat
		if err := rows.Scan(&st.SessionID, &st.PlayerID, &st.PlayerName, &st.PlayCount, &st.ShuttlecockUsed); err == nil {
			list = append(list, st)
		}
	}
	httpx.OK(w, http.StatusOK, list)
}

type simpleStatInput struct {
	PlayerID       string `json:"player_id"`
	PlayCount      *int   `json:"play_count"`
	ShuttlecockUsed *int  `json:"shuttlecock_used"`
}

type saveSimpleInput struct {
	Rows []simpleStatInput `json:"rows"`
}

// SaveSimpleStats upserts the simple recap: per-player play count + kok.
// It also syncs one session-level USAGE transaction (match_id IS NULL) so
// stock and session totals move even without per-match 2v2 input.
func (s *Service) SaveSimpleStats(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.fetch(r, id)
	if err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	var in saveSimpleInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	for _, row := range in.Rows {
		if _, err := uuid.Parse(row.PlayerID); err != nil {
			httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid player ID."))
			return
		}
		plays := 0
		if row.PlayCount != nil {
			plays = *row.PlayCount
		}
		cocks := 0
		if row.ShuttlecockUsed != nil {
			cocks = *row.ShuttlecockUsed
		}
		if plays < 0 || cocks < 0 {
			httpx.WriteAppError(w, httpx.Unprocessable("Play count and shuttlecock must not be negative."))
			return
		}
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save recap.")
		return
	}
	defer tx.Rollback(r.Context())

	for _, row := range in.Rows {
		plays := 0
		if row.PlayCount != nil {
			plays = *row.PlayCount
		}
		cocks := 0
		if row.ShuttlecockUsed != nil {
			cocks = *row.ShuttlecockUsed
		}
		if plays == 0 && cocks == 0 {
			if _, err := tx.Exec(r.Context(),
				`DELETE FROM session_player_stats WHERE session_id = $1 AND player_id = $2`,
				id, row.PlayerID); err != nil {
				httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save recap.")
				return
			}
			continue
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO session_player_stats (session_id, player_id, play_count, shuttlecock_used, updated_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (session_id, player_id) DO UPDATE SET
				play_count = EXCLUDED.play_count,
				shuttlecock_used = EXCLUDED.shuttlecock_used,
				updated_at = now()`,
			id, row.PlayerID, plays, cocks); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save recap.")
			return
		}
	}

	var totalSimple int
	if err := tx.QueryRow(r.Context(),
		`SELECT COALESCE(SUM(shuttlecock_used), 0) FROM session_player_stats WHERE session_id = $1`,
		id).Scan(&totalSimple); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save recap.")
		return
	}

	// Keep stock in sync: one session-level USAGE row, match_id NULL.
	// Per-match USAGE rows (match_id NOT NULL) are untouched.
	// Auto-write only when a suitable bucket covers the new total (counting
	// the replaced figure back). Otherwise nothing is written here and the
	// session must be allocated explicitly via shuttle allocation.
	covered := false
	coverProduct := ""
	if totalSimple > 0 {
		if productID, err := pickUsageProduct(r.Context(), tx, sess.Type); err == nil {
			var freed int64
			_ = tx.QueryRow(r.Context(),
				`SELECT COALESCE(SUM(units), 0) FROM shuttlecock_transactions
				 WHERE session_id = $1 AND match_id IS NULL AND type = 'USAGE' AND product_id = $2`,
				id, productID).Scan(&freed)
			if name, stock, serr := productStock(r.Context(), tx, productID); serr != nil {
				covered = true
				coverProduct = productID
			} else if err := checkStockFit(name, stock, freed, int64(totalSimple)); err == nil {
				covered = true
				coverProduct = productID
			}
		}
	}
	if covered {
		if _, err := tx.Exec(r.Context(),
			`DELETE FROM shuttlecock_transactions WHERE session_id = $1 AND match_id IS NULL AND type = 'USAGE'`,
			id); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save shuttlecock usage.")
			return
		}
	}
	if totalSimple > 0 && covered {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO shuttlecock_transactions (id, product_id, type, units, session_id, match_id, note)
			VALUES (gen_random_uuid(), $1, 'USAGE', $2, $3, NULL, 'simple-recap')`,
			coverProduct, totalSimple, id); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save shuttlecock usage.")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save recap.")
		return
	}
	s.ListSimpleStats(w, r)
}
