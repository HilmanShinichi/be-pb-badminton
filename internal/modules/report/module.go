package report

import (
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

// wantCSV lets every report double as a CSV download. PRD §67.
func wantCSV(r *http.Request) bool { return r.URL.Query().Get("format") == "csv" }

func sendCSV(w http.ResponseWriter, r *http.Request, name string, header []string, rows [][]string) {
	httpx.CSV(w, name, header, rows)
}

// Attendance: per-player listed/present/cancelled/no-show with rate. PRD §17/§40.
func (s *Service) Attendance(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT pl.name,
		       COUNT(*) FILTER (WHERE a.status <> 'NOT_LISTED'),
		       COUNT(*) FILTER (WHERE a.status = 'PRESENT'),
		       COUNT(*) FILTER (WHERE a.status = 'CANCELLED'),
		       COUNT(*) FILTER (WHERE a.status = 'NO_SHOW')
		FROM attendances a JOIN players pl ON pl.id = a.player_id
		GROUP BY pl.name ORDER BY pl.name`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load attendance report.")
		return
	}
	defer rows.Close()

	type row struct {
		Name      string `json:"player"`
		Listed    int    `json:"listed"`
		Present   int    `json:"present"`
		Cancelled int    `json:"cancelled"`
		NoShow    int    `json:"no_show"`
		RateBP    int64  `json:"no_show_rate_bp"`
	}
	list := []row{}
	csvRows := [][]string{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.Name, &t.Listed, &t.Present, &t.Cancelled, &t.NoShow); err == nil {
			t.RateBP = finance.NoShowRate(int64(t.NoShow), int64(t.Listed))
			list = append(list, t)
			csvRows = append(csvRows, []string{t.Name, it(t.Listed), it(t.Present), it(t.Cancelled), it(t.NoShow), fmt.Sprintf("%.1f%%", float64(t.RateBP)/100)})
		}
	}
	if wantCSV(r) {
		sendCSV(w, r, "attendance-report.csv",
			[]string{"Player", "Listed", "Present", "Cancelled", "No-show", "No-show rate"}, csvRows)
		return
	}
	httpx.OK(w, http.StatusOK, list)
}

// NoShow ranks players by no-show count, highest first. PRD §40.
func (s *Service) NoShow(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT pl.name,
		       COUNT(*) FILTER (WHERE a.status <> 'NOT_LISTED'),
		       COUNT(*) FILTER (WHERE a.status = 'PRESENT'),
		       COUNT(*) FILTER (WHERE a.status = 'NO_SHOW')
		FROM attendances a JOIN players pl ON pl.id = a.player_id
		GROUP BY pl.name
		HAVING COUNT(*) FILTER (WHERE a.status = 'NO_SHOW') > 0
		ORDER BY COUNT(*) FILTER (WHERE a.status = 'NO_SHOW') DESC, pl.name`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load no-show report.")
		return
	}
	defer rows.Close()

	type row struct {
		Name    string `json:"player"`
		Listed  int    `json:"listed"`
		Present int    `json:"present"`
		NoShow  int    `json:"no_show"`
		RateBP  int64  `json:"no_show_rate_bp"`
	}
	list := []row{}
	csvRows := [][]string{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.Name, &t.Listed, &t.Present, &t.NoShow); err == nil {
			t.RateBP = finance.NoShowRate(int64(t.NoShow), int64(t.Listed))
			list = append(list, t)
			csvRows = append(csvRows, []string{t.Name, it(t.Listed), it(t.Present), it(t.NoShow), fmt.Sprintf("%.1f%%", float64(t.RateBP)/100)})
		}
	}
	if wantCSV(r) {
		sendCSV(w, r, "no-show-report.csv",
			[]string{"Player", "Listed", "Present", "No-show", "No-show rate"}, csvRows)
		return
	}
	httpx.OK(w, http.StatusOK, list)
}

// Shuttlecock: player contribution ranking across all sessions. PRD §24.
func (s *Service) Shuttlecock(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT pl.name,
		       COUNT(DISTINCT m.id),
		       COALESCE(SUM(m.shuttlecock_used), 0)
		FROM match_players mp
		JOIN matches m ON m.id = mp.match_id
		JOIN players pl ON pl.id = mp.player_id
		GROUP BY pl.name
		ORDER BY COALESCE(SUM(m.shuttlecock_used), 0) DESC, pl.name`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load shuttlecock report.")
		return
	}
	defer rows.Close()

	type row struct {
		Name    string `json:"player"`
		Matches int    `json:"matches"`
		Usage   int64  `json:"shuttlecock_usage"`
	}
	list := []row{}
	csvRows := [][]string{}
	rank := 1
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.Name, &t.Matches, &t.Usage); err == nil {
			list = append(list, t)
			csvRows = append(csvRows, []string{it(rank), t.Name, it(t.Matches), it(int(t.Usage))})
			rank++
		}
	}
	if wantCSV(r) {
		sendCSV(w, r, "shuttlecock-report.csv",
			[]string{"Rank", "Player", "Matches", "Player shuttlecocks"}, csvRows)
		return
	}
	httpx.OK(w, http.StatusOK, list)
}

// PlayerUsage: attendance, matches and shuttlecock contribution per player
// in one table. PRD §41.
func (s *Service) PlayerUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT pl.name,
		       COALESCE(att.present, 0),
		       COALESCE(mm.matches, 0),
		       COALESCE(us.usage, 0)
		FROM players pl
		LEFT JOIN (
			SELECT player_id, COUNT(*) AS present FROM attendances WHERE status = 'PRESENT' GROUP BY player_id
		) att ON att.player_id = pl.id
		LEFT JOIN (
			SELECT mp.player_id, COUNT(*) AS matches FROM match_players mp GROUP BY mp.player_id
		) mm ON mm.player_id = pl.id
		LEFT JOIN (
			SELECT mp.player_id, SUM(m.shuttlecock_used) AS usage
			FROM match_players mp JOIN matches m ON m.id = mp.match_id GROUP BY mp.player_id
		) us ON us.player_id = pl.id
		WHERE pl.status <> 'ARCHIVED'
		ORDER BY COALESCE(us.usage, 0) DESC, pl.name`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load player report.")
		return
	}
	defer rows.Close()

	type row struct {
		Name    string `json:"player"`
		Present int    `json:"present"`
		Matches int    `json:"matches"`
		Usage   int64  `json:"shuttlecock_usage"`
	}
	list := []row{}
	csvRows := [][]string{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.Name, &t.Present, &t.Matches, &t.Usage); err == nil {
			list = append(list, t)
			csvRows = append(csvRows, []string{t.Name, it(t.Present), it(t.Matches), it(int(t.Usage))})
		}
	}
	if wantCSV(r) {
		sendCSV(w, r, "player-report.csv",
			[]string{"Player", "Present", "Matches", "Player shuttlecocks"}, csvRows)
		return
	}
	httpx.OK(w, http.StatusOK, list)
}

// Financial: revenue by source, expense by category, profit and cash flow.
func (s *Service) Financial(w http.ResponseWriter, r *http.Request) {
	var revenueBySource = map[string]int64{}
	rows, err := s.db.Query(r.Context(),
		`SELECT source, SUM(amount) FROM revenues GROUP BY source ORDER BY source`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load financial report.")
		return
	}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err == nil {
			revenueBySource[k] = v
		}
	}
	rows.Close()

	var expenseByCategory = map[string]int64{}
	rows, err = s.db.Query(r.Context(),
		`SELECT category, SUM(amount) FROM expenses GROUP BY category ORDER BY category`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load financial report.")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err == nil {
			expenseByCategory[k] = v
		}
	}

	var totalRevenue, totalExpense int64
	csvRows := [][]string{}
	for _, k := range []string{"COMMITMENT_FEE", "ATTENDANCE_CONTRIBUTION", "NON_MEMBER_FEE", "DAILY_EVENT_CONTRIBUTION", "OTHER"} {
		if revenueBySource[k] == 0 {
			continue
		}
		totalRevenue += revenueBySource[k]
		csvRows = append(csvRows, []string{"REVENUE", k, it(int(revenueBySource[k]))})
	}
	for _, k := range []string{"VENUE", "SHUTTLECOCK_PURCHASE", "EQUIPMENT", "REFUND", "OTHER"} {
		if expenseByCategory[k] == 0 {
			continue
		}
		totalExpense += expenseByCategory[k]
		csvRows = append(csvRows, []string{"EXPENSE", k, it(int(expenseByCategory[k]))})
	}
	csvRows = append(csvRows, []string{"TOTAL", "Revenue", it(int(totalRevenue))})
	csvRows = append(csvRows, []string{"TOTAL", "Cash expenses", it(int(totalExpense))})
	csvRows = append(csvRows, []string{"TOTAL", "Cash flow", it(int(totalRevenue - totalExpense))})

	if wantCSV(r) {
		sendCSV(w, r, "financial-report.csv", []string{"Type", "Category", "Amount"}, csvRows)
		return
	}
	httpx.OK(w, http.StatusOK, map[string]any{
		"revenue_by_source":   revenueBySource,
		"expense_by_category": expenseByCategory,
		"total_revenue":       totalRevenue,
		"total_expense":       totalExpense,
		"cash_flow":           totalRevenue - totalExpense,
	})
}

// InactiveMembers flags players with no PRESENT attendance for the
// configured threshold. PRD §39.
func (s *Service) InactiveMembers(w http.ResponseWriter, r *http.Request) {
	months := 6
	if v := r.URL.Query().Get("months"); len(v) > 0 && (v[0] >= '1' && v[0] <= '9') {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 && n < 120 {
			months = n
		}
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT pl.name,
		       COALESCE(TO_CHAR(MAX(a.present_at), 'YYYY-MM-DD'), '-'),
		       COALESCE(cnt.total, 0),
		       COALESCE((EXTRACT(EPOCH FROM now() - MAX(a.present_at)) / 86400)::int, 0)
		FROM players pl
		LEFT JOIN (
			SELECT player_id, listed_at AS present_at FROM attendances WHERE status = 'PRESENT'
		) a ON a.player_id = pl.id
		LEFT JOIN (
			SELECT player_id, COUNT(*) AS total FROM attendances WHERE status = 'PRESENT' GROUP BY player_id
		) cnt ON cnt.player_id = pl.id
		WHERE pl.status = 'ACTIVE'
		GROUP BY pl.name, cnt.total
		HAVING COALESCE(MAX(a.present_at), 'epoch') < now() - make_interval(months => $1)
		ORDER BY MAX(a.present_at) NULLS FIRST`, months)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load inactive members.")
		return
	}
	defer rows.Close()

	type row struct {
		Name         string `json:"player"`
		LastPresent  string `json:"last_present"`
		Total        int    `json:"total_present"`
		DaysInactive int    `json:"days_inactive"`
	}
	list := []row{}
	for rows.Next() {
		var t row
		if err := rows.Scan(&t.Name, &t.LastPresent, &t.Total, &t.DaysInactive); err == nil {
			list = append(list, t)
		}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"threshold_months": months, "players": list})
}

func it(n int) string { return fmt.Sprintf("%d", n) }
