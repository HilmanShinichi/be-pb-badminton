package report

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

// wantCSV lets every report double as a CSV download. PRD §67.
func wantCSV(c *fiber.Ctx) bool { return c.Query("format") == "csv" }

func sendCSV(c *fiber.Ctx, name string, header []string, rows [][]string) error {
	return httpx.CSV(c, name, header, rows)
}

type ScopeFilter struct {
	Scope     string // "ALL", "PERIOD", "DAILY_EVENT"
	PeriodID  string
	SessionID string
}

func parseScopeFilter(c *fiber.Ctx) ScopeFilter {
	f := ScopeFilter{
		Scope:     c.Query("scope"),
		PeriodID:  c.Query("period_id"),
		SessionID: c.Query("session_id"),
	}
	if f.PeriodID != "" {
		if _, err := uuid.Parse(f.PeriodID); err != nil {
			f.PeriodID = ""
		} else {
			f.Scope = "PERIOD"
		}
	}
	if f.SessionID != "" {
		if _, err := uuid.Parse(f.SessionID); err != nil {
			f.SessionID = ""
		} else {
			f.Scope = "DAILY_EVENT"
		}
	}
	return f
}

func sessionWhere(f ScopeFilter, alias string) (string, []any) {
	if f.PeriodID != "" {
		return fmt.Sprintf("%s.period_id = $1", alias), []any{f.PeriodID}
	}
	if f.SessionID != "" {
		return fmt.Sprintf("%s.id = $1", alias), []any{f.SessionID}
	}
	if f.Scope == "PERIOD" {
		return fmt.Sprintf("%s.type = 'PERIOD'", alias), nil
	}
	if f.Scope == "DAILY_EVENT" {
		return fmt.Sprintf("%s.type = 'DAILY_EVENT'", alias), nil
	}
	return "TRUE", nil
}

// Attendance: per-player listed/present/cancelled/no-show with rate. PRD §17/§40.
func (s *Service) Attendance(c *fiber.Ctx) error {
	f := parseScopeFilter(c)
	cond, args := sessionWhere(f, "ms")

	query := fmt.Sprintf(`
		SELECT pl.name,
		       COUNT(*) FILTER (WHERE a.status <> 'NOT_LISTED'),
		       COUNT(*) FILTER (WHERE a.status = 'PRESENT'),
		       COUNT(*) FILTER (WHERE a.status = 'CANCELLED'),
		       COUNT(*) FILTER (WHERE a.status = 'NO_SHOW')
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		JOIN mabar_sessions ms ON ms.id = a.session_id
		WHERE %s
		GROUP BY pl.name
		HAVING COUNT(*) FILTER (WHERE a.status <> 'NOT_LISTED') > 0
		ORDER BY pl.name`, cond)

	rows, err := s.db.Query(c.Context(), query, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load attendance report.")
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
	if wantCSV(c) {
		return sendCSV(c, "attendance-report.csv",
			[]string{"Player", "Listed", "Present", "Cancelled", "No-show", "No-show rate"}, csvRows)
	}
	return httpx.OK(c, http.StatusOK, list)
}

// NoShow ranks players by no-show count, highest first. PRD §40.
func (s *Service) NoShow(c *fiber.Ctx) error {
	f := parseScopeFilter(c)
	cond, args := sessionWhere(f, "ms")

	query := fmt.Sprintf(`
		SELECT pl.name,
		       COUNT(*) FILTER (WHERE a.status <> 'NOT_LISTED'),
		       COUNT(*) FILTER (WHERE a.status = 'PRESENT'),
		       COUNT(*) FILTER (WHERE a.status = 'NO_SHOW')
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		JOIN mabar_sessions ms ON ms.id = a.session_id
		WHERE %s
		GROUP BY pl.name
		HAVING COUNT(*) FILTER (WHERE a.status = 'NO_SHOW') > 0
		ORDER BY COUNT(*) FILTER (WHERE a.status = 'NO_SHOW') DESC, pl.name`, cond)

	rows, err := s.db.Query(c.Context(), query, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load no-show report.")
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
	if wantCSV(c) {
		return sendCSV(c, "no-show-report.csv",
			[]string{"Player", "Listed", "Present", "No-show", "No-show rate"}, csvRows)
	}
	return httpx.OK(c, http.StatusOK, list)
}

// NoShowTracker returns comprehensive no-show and cancellation records with
// filtering by months (e.g. 3, 6, 9, 12 months) and scope (period, daily, or all).
func (s *Service) NoShowTracker(c *fiber.Ctx) error {
	months := 0
	if mStr := c.Query("months"); mStr != "" {
		n := 0
		for _, c := range mStr {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 && n <= 120 {
			months = n
		}
	}

	f := parseScopeFilter(c)
	cond, args := sessionWhere(f, "ms")

	monthCond := "TRUE"
	if months > 0 {
		monthCond = fmt.Sprintf("ms.date >= CURRENT_DATE - make_interval(months => %d)", months)
	}

	statusFilter := c.Query("status")
	statusCond := "a.status IN ('NO_SHOW', 'CANCELLED')"
	if statusFilter == "NO_SHOW" {
		statusCond = "a.status = 'NO_SHOW'"
	} else if statusFilter == "CANCELLED" {
		statusCond = "a.status = 'CANCELLED'"
	}

	// 1. Fetch individual incident records
	query := fmt.Sprintf(`
		SELECT a.id::text,
		       ms.id::text,
		       TO_CHAR(ms.date, 'YYYY-MM-DD'),
		       ms.type,
		       COALESCE(p.name, ''),
		       COALESCE(v.name, 'GOR'),
		       pl.id::text,
		       pl.name,
		       a.status,
		       COALESCE(a.no_show_reason, ''),
		       COALESCE(TO_CHAR(a.listed_at, 'YYYY-MM-DD HH24:MI'), '')
		FROM attendances a
		JOIN mabar_sessions ms ON ms.id = a.session_id
		JOIN players pl ON pl.id = a.player_id
		LEFT JOIN membership_periods p ON p.id = ms.period_id
		LEFT JOIN venues v ON v.id = ms.venue_id
		WHERE %s AND %s AND %s
		ORDER BY ms.date DESC, pl.name`, statusCond, cond, monthCond)

	rows, err := s.db.Query(c.Context(), query, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load no-show tracker records.")
	}
	defer rows.Close()

	type incidentRow struct {
		AttendanceID string `json:"attendance_id"`
		SessionID    string `json:"session_id"`
		SessionDate  string `json:"session_date"`
		SessionType  string `json:"session_type"`
		PeriodName   string `json:"period_name"`
		VenueName    string `json:"venue_name"`
		PlayerID     string `json:"player_id"`
		PlayerName   string `json:"player_name"`
		Status       string `json:"status"`
		Reason       string `json:"reason"`
		ListedAt     string `json:"listed_at"`
	}

	incidents := []incidentRow{}
	for rows.Next() {
		var it incidentRow
		if err := rows.Scan(&it.AttendanceID, &it.SessionID, &it.SessionDate, &it.SessionType,
			&it.PeriodName, &it.VenueName, &it.PlayerID, &it.PlayerName,
			&it.Status, &it.Reason, &it.ListedAt); err == nil {
			incidents = append(incidents, it)
		}
	}

	// 2. Fetch total listed per player to compute accurate rates
	listedQuery := fmt.Sprintf(`
		SELECT a.player_id::text, COUNT(*) FILTER (WHERE a.status <> 'NOT_LISTED')
		FROM attendances a
		JOIN mabar_sessions ms ON ms.id = a.session_id
		WHERE %s AND %s
		GROUP BY a.player_id`, cond, monthCond)

	listedMap := map[string]int{}
	if lRows, err := s.db.Query(c.Context(), listedQuery, args...); err == nil {
		defer lRows.Close()
		for lRows.Next() {
			var pid string
			var cnt int
			if err := lRows.Scan(&pid, &cnt); err == nil {
				listedMap[pid] = cnt
			}
		}
	}

	// 3. Aggregate per player
	type playerSummary struct {
		PlayerID       string        `json:"player_id"`
		PlayerName     string        `json:"player_name"`
		NoShowCount    int           `json:"no_show_count"`
		CancelledCount int           `json:"cancelled_count"`
		TotalIncidents int           `json:"total_incidents"`
		TotalListed    int           `json:"total_listed"`
		RateBP         int64         `json:"rate_bp"`
		LastIncident   string        `json:"last_incident"`
		Incidents      []incidentRow `json:"incidents"`
	}

	playerMap := map[string]*playerSummary{}
	playerOrder := []string{}

	for _, inc := range incidents {
		ps, ok := playerMap[inc.PlayerID]
		if !ok {
			ps = &playerSummary{
				PlayerID:     inc.PlayerID,
				PlayerName:   inc.PlayerName,
				LastIncident: inc.SessionDate,
				Incidents:    []incidentRow{},
			}
			playerMap[inc.PlayerID] = ps
			playerOrder = append(playerOrder, inc.PlayerID)
		}
		if inc.Status == "NO_SHOW" {
			ps.NoShowCount++
		} else if inc.Status == "CANCELLED" {
			ps.CancelledCount++
		}
		ps.TotalIncidents++
		ps.Incidents = append(ps.Incidents, inc)
	}

	playerList := []playerSummary{}
	for _, pid := range playerOrder {
		ps := playerMap[pid]
		ps.TotalListed = listedMap[pid]
		if ps.TotalListed < ps.TotalIncidents {
			ps.TotalListed = ps.TotalIncidents
		}
		ps.RateBP = finance.NoShowRate(int64(ps.NoShowCount), int64(ps.TotalListed))
		playerList = append(playerList, *ps)
	}

	sort.Slice(playerList, func(i, j int) bool {
		if playerList[i].TotalIncidents != playerList[j].TotalIncidents {
			return playerList[i].TotalIncidents > playerList[j].TotalIncidents
		}
		if playerList[i].NoShowCount != playerList[j].NoShowCount {
			return playerList[i].NoShowCount > playerList[j].NoShowCount
		}
		return playerList[i].PlayerName < playerList[j].PlayerName
	})

	if wantCSV(c) {
		csvRows := [][]string{}
		for _, it := range incidents {
			csvRows = append(csvRows, []string{
				it.SessionDate,
				it.SessionType,
				it.PeriodName,
				it.VenueName,
				it.PlayerName,
				it.Status,
				it.Reason,
			})
		}
		return sendCSV(c, "no-show-tracker.csv",
			[]string{"Date", "Type", "Period", "Venue", "Player", "Status", "Reason"}, csvRows)
	}

	return httpx.OK(c, http.StatusOK, map[string]any{
		"months":          months,
		"scope":           f.Scope,
		"total_incidents": len(incidents),
		"players":         playerList,
		"incidents":       incidents,
	})
}

// DeleteNoShowIncident removes or restores an attendance record marked as NO_SHOW or CANCELLED
// (e.g. when an admin mistakenly recorded a player as no-show / wrong input).
func (s *Service) DeleteNoShowIncident(c *fiber.Ctx) error {
	attendanceID := c.Params("attendanceId")
	if _, err := uuid.Parse(attendanceID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "ID absensi tidak valid."))
	}

	var sessionID, playerID, status string
	err := s.db.QueryRow(c.Context(),
		`SELECT session_id::text, player_id::text, status
		 FROM attendances
		 WHERE id = $1`, attendanceID).Scan(&sessionID, &playerID, &status)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Catatan absensi tidak ditemukan.")
	}

	if status != "NO_SHOW" && status != "CANCELLED" {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_STATUS", "Hanya catatan NO_SHOW atau CANCELLED yang dapat dihapus dari tracker."))
	}

	restorePresent := c.Query("restore_present") == "true"

	// Check if player has bills in this session
	var bills int
	_ = s.db.QueryRow(c.Context(),
		`SELECT COUNT(*) FROM player_bills WHERE session_id = $1 AND player_id = $2`,
		sessionID, playerID).Scan(&bills)

	if restorePresent || bills > 0 {
		_, err = s.db.Exec(c.Context(),
			`UPDATE attendances
			 SET status = 'PRESENT', no_show_reason = NULL, cancelled_at = NULL
			 WHERE id = $1`, attendanceID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal memperbarui status absensi.")
		}
		return httpx.OK(c, http.StatusOK, map[string]any{"ok": true, "action": "restored_present"})
	}

	tag, err := s.db.Exec(c.Context(),
		`DELETE FROM attendances WHERE id = $1`, attendanceID)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menghapus catatan absensi.")
	}

	deletedPlayer := false
	var history int
	_ = s.db.QueryRow(c.Context(), `
		SELECT (SELECT COUNT(*) FROM attendances WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM match_players WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM player_bills WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM payments WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM revenues WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM memberships WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM session_player_stats WHERE player_id = $1)`,
		playerID).Scan(&history)
	if history == 0 {
		tag, err := s.db.Exec(c.Context(), `DELETE FROM players WHERE id = $1`, playerID)
		if err == nil && tag.RowsAffected() > 0 {
			deletedPlayer = true
		}
	}

	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true, "action": "deleted", "player_deleted": deletedPlayer})
}

// Shuttlecock: player contribution ranking across all sessions. PRD §24.
func (s *Service) Shuttlecock(c *fiber.Ctx) error {
	f := parseScopeFilter(c)
	cond, args := sessionWhere(f, "ms")

	query := fmt.Sprintf(`
		SELECT pl.name,
		       COUNT(DISTINCT m.id),
		       COALESCE(SUM(m.shuttlecock_used), 0)
		FROM match_players mp
		JOIN matches m ON m.id = mp.match_id
		JOIN mabar_sessions ms ON ms.id = m.session_id
		JOIN players pl ON pl.id = mp.player_id
		WHERE %s
		GROUP BY pl.name
		ORDER BY COALESCE(SUM(m.shuttlecock_used), 0) DESC, pl.name`, cond)

	rows, err := s.db.Query(c.Context(), query, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load shuttlecock report.")
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
	if wantCSV(c) {
		return sendCSV(c, "shuttlecock-report.csv",
			[]string{"Rank", "Player", "Matches", "Player shuttlecocks"}, csvRows)
	}
	return httpx.OK(c, http.StatusOK, list)
}

// PlayerUsage: attendance, matches and shuttlecock contribution per player
// in one table. PRD §41.
func (s *Service) PlayerUsage(c *fiber.Ctx) error {
	f := parseScopeFilter(c)
	cond, args := sessionWhere(f, "ms")

	query := fmt.Sprintf(`
		SELECT pl.name,
		       COALESCE(att.present, 0),
		       COALESCE(mm.matches, 0),
		       COALESCE(us.usage, 0)
		FROM players pl
		LEFT JOIN (
			SELECT a.player_id, COUNT(*) AS present
			FROM attendances a
			JOIN mabar_sessions ms ON ms.id = a.session_id
			WHERE a.status = 'PRESENT' AND %s
			GROUP BY a.player_id
		) att ON att.player_id = pl.id
		LEFT JOIN (
			SELECT mp.player_id, COUNT(*) AS matches
			FROM match_players mp
			JOIN matches m ON m.id = mp.match_id
			JOIN mabar_sessions ms ON ms.id = m.session_id
			WHERE %s
			GROUP BY mp.player_id
		) mm ON mm.player_id = pl.id
		LEFT JOIN (
			SELECT mp.player_id, SUM(m.shuttlecock_used) AS usage
			FROM match_players mp
			JOIN matches m ON m.id = mp.match_id
			JOIN mabar_sessions ms ON ms.id = m.session_id
			WHERE %s
			GROUP BY mp.player_id
		) us ON us.player_id = pl.id
		WHERE pl.status <> 'ARCHIVED'
		  AND (COALESCE(att.present, 0) > 0 OR COALESCE(mm.matches, 0) > 0 OR COALESCE(us.usage, 0) > 0)
		ORDER BY COALESCE(us.usage, 0) DESC, pl.name`, cond, cond, cond)

	rows, err := s.db.Query(c.Context(), query, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load player report.")
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
	if wantCSV(c) {
		return sendCSV(c, "player-report.csv",
			[]string{"Player", "Present", "Matches", "Player shuttlecocks"}, csvRows)
	}
	return httpx.OK(c, http.StatusOK, list)
}

// Financial: revenue by source, expense by category, profit and cash flow.
func (s *Service) Financial(c *fiber.Ctx) error {
	f := parseScopeFilter(c)

	revCond := "TRUE"
	expCond := "TRUE"
	args := []any{}

	if f.PeriodID != "" {
		args = append(args, f.PeriodID)
		revCond = "(r.period_id = $1 OR s.period_id = $1)"
		expCond = "(e.period_id = $1 OR s.period_id = $1)"
	} else if f.SessionID != "" {
		args = append(args, f.SessionID)
		revCond = "r.session_id = $1"
		expCond = "e.session_id = $1"
	} else if f.Scope == "PERIOD" {
		revCond = "(s.type = 'PERIOD' OR r.period_id IS NOT NULL)"
		expCond = "(s.type = 'PERIOD' OR e.period_id IS NOT NULL)"
	} else if f.Scope == "DAILY_EVENT" {
		revCond = "s.type = 'DAILY_EVENT'"
		expCond = "s.type = 'DAILY_EVENT'"
	}

	var revenueBySource = map[string]int64{}
	rows, err := s.db.Query(c.Context(),
		fmt.Sprintf(`SELECT r.source, SUM(r.amount)
		             FROM revenues r
		             LEFT JOIN mabar_sessions s ON s.id = r.session_id
		             WHERE %s
		             GROUP BY r.source
		             ORDER BY r.source`, revCond), args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load financial report.")
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
	rows, err = s.db.Query(c.Context(),
		fmt.Sprintf(`SELECT e.category, SUM(e.amount)
		             FROM expenses e
		             LEFT JOIN mabar_sessions s ON s.id = e.session_id
		             WHERE %s
		             GROUP BY e.category
		             ORDER BY e.category`, expCond), args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load financial report.")
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

	if wantCSV(c) {
		return sendCSV(c, "financial-report.csv", []string{"Type", "Category", "Amount"}, csvRows)
	}
	return httpx.OK(c, http.StatusOK, map[string]any{
		"revenue_by_source":   revenueBySource,
		"expense_by_category": expenseByCategory,
		"total_revenue":       totalRevenue,
		"total_expense":       totalExpense,
		"cash_flow":           totalRevenue - totalExpense,
	})
}

// InactiveMembers flags players with no PRESENT attendance for the
// configured threshold. PRD §39.
func (s *Service) InactiveMembers(c *fiber.Ctx) error {
	months := 6
	if v := c.Query("months"); len(v) > 0 && (v[0] >= '1' && v[0] <= '9') {
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
	rows, err := s.db.Query(c.Context(), `
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
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load inactive members.")
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
	return httpx.OK(c, http.StatusOK, map[string]any{"threshold_months": months, "players": list})
}

func it(n int) string { return fmt.Sprintf("%d", n) }
