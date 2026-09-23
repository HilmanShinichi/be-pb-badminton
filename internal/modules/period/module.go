package period

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/domain/finance"
	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

type Period struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	StartDate          string  `json:"start_date"`
	EndDate            string  `json:"end_date"`
	NumberOfSessions   int     `json:"number_of_sessions"`
	MaxMembers         *int    `json:"max_members"`
	CommitmentFee      int64   `json:"commitment_fee"`
	MemberContribution int64   `json:"member_contribution"`
	NonMemberFee       int64   `json:"non_member_fee"`
	VenueCostTotal     int64   `json:"venue_cost_total"`
	ShuttlePackPrice   int64   `json:"shuttle_pack_price"`
	ShuttleUnitsPerPack int    `json:"shuttle_units_per_pack"`
	ShuttlePerSession  int     `json:"shuttle_per_session"`
	Status             string  `json:"status"`
	SessionWeekdays    []int32 `json:"session_weekdays"`
	DefaultStartTime   *string `json:"default_start_time"`
	DefaultEndTime     *string `json:"default_end_time"`
	CreatedAt          string  `json:"created_at"`
}

type Membership struct {
	ID              string `json:"id"`
	PeriodID        string `json:"period_id"`
	PlayerID        string `json:"player_id"`
	PlayerName      string `json:"player_name"`
	CommitmentFee   int64  `json:"commitment_fee"`
	JoinedAt        string `json:"joined_at"`
	Status          string `json:"status"`
	AttendanceCount int    `json:"attendance_count"`
	TotalBill       int64  `json:"total_bill"`
	NonMemberCost   int64  `json:"non_member_cost"`
	Benefit         int64  `json:"benefit"`
	// PaidBySource sums finance revenues recorded for this player in this
	// period, keyed by revenue source (COMMITMENT_FEE,
	// ATTENDANCE_CONTRIBUTION, ...). It is the single source of truth for
	// "has this member paid", so no extra payment columns are needed.
	PaidBySource map[string]int64 `json:"paid"`
}

type periodInput struct {
	Name               *string  `json:"name"`
	StartDate          *string  `json:"start_date"`
	EndDate            *string  `json:"end_date"`
	NumberOfSessions   *int     `json:"number_of_sessions"`
	MaxMembers         *int     `json:"max_members"`
	CommitmentFee      *int64   `json:"commitment_fee"`
	MemberContribution *int64   `json:"member_contribution"`
	NonMemberFee       *int64   `json:"non_member_fee"`
	VenueCostTotal     *int64   `json:"venue_cost_total"`
	ShuttlePackPrice   *int64   `json:"shuttle_pack_price"`
	ShuttleUnitsPerPack *int    `json:"shuttle_units_per_pack"`
	ShuttlePerSession  *int     `json:"shuttle_per_session"`
	Status             *string  `json:"status"`
	SessionWeekdays    *[]int32 `json:"session_weekdays"`
	DefaultStartTime   *string  `json:"default_start_time"`
	DefaultEndTime     *string  `json:"default_end_time"`
}

const periodCols = `id::text, name, start_date::text, end_date::text, number_of_sessions,
	max_members, commitment_fee, member_contribution, non_member_fee,
	venue_cost_total, shuttle_pack_price, shuttle_units_per_pack, shuttle_per_session,
	status, session_weekdays, default_start_time::text, default_end_time::text, created_at::text`

func scanPeriod(row pgx.Row) (Period, error) {
	var p Period
	err := row.Scan(&p.ID, &p.Name, &p.StartDate, &p.EndDate, &p.NumberOfSessions,
		&p.MaxMembers, &p.CommitmentFee, &p.MemberContribution, &p.NonMemberFee,
		&p.VenueCostTotal, &p.ShuttlePackPrice, &p.ShuttleUnitsPerPack, &p.ShuttlePerSession,
		&p.Status, &p.SessionWeekdays, &p.DefaultStartTime, &p.DefaultEndTime, &p.CreatedAt)
	return p, err
}

func (s *Service) List(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(),
		`SELECT `+periodCols+` FROM membership_periods ORDER BY start_date DESC`)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load periods.")
	}
	defer rows.Close()
	periods := []Period{}
	for rows.Next() {
		if p, err := scanPeriod(rows); err == nil {
			periods = append(periods, p)
		}
	}
	return httpx.OK(c, http.StatusOK, periods)
}

func (s *Service) Get(c *fiber.Ctx) error {
	p, err := s.fetch(c.Context(), c.Params("id"))
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	return httpx.OK(c, http.StatusOK, p)
}

func (s *Service) fetch(ctx context.Context, id string) (Period, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Period{}, httpx.BadRequest("BAD_REQUEST", "Invalid period ID.")
	}
	p, err := scanPeriod(s.db.QueryRow(ctx,
		`SELECT `+periodCols+` FROM membership_periods WHERE id = $1`, id))
	if err != nil {
		return Period{}, httpx.NotFound("Period not found.")
	}
	return p, nil
}

func (in periodInput) validate() error {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return httpx.Unprocessable("Period name is required.")
	}
	for label, d := range map[string]*string{"Start date": in.StartDate, "End date": in.EndDate} {
		if d == nil {
			return httpx.Unprocessable(label + " is required.")
		}
		if _, err := time.Parse("2006-01-02", *d); err != nil {
			return httpx.Unprocessable(label + " must use YYYY-MM-DD format.")
		}
	}
	if *in.StartDate > *in.EndDate {
		return httpx.Unprocessable("Start date must be before end date.")
	}
	if in.SessionWeekdays != nil {
		seen := map[int32]bool{}
		for _, d := range *in.SessionWeekdays {
			if d < 0 || d > 6 {
				return httpx.Unprocessable("Weekdays must be 0 (Sunday) through 6 (Saturday).")
			}
			if seen[d] {
				return httpx.Unprocessable("Each weekday can only be picked once.")
			}
			seen[d] = true
		}
	}
	for label, t := range map[string]*string{"Default start time": in.DefaultStartTime, "Default end time": in.DefaultEndTime} {
		if t == nil || *t == "" {
			continue
		}
		if _, err := time.Parse("15:04", *t); err != nil {
			return httpx.Unprocessable(label + " must use HH:MM format.")
		}
	}
	for label, v := range map[string]*int64{"Commitment fee": in.CommitmentFee, "Per-session contribution": in.MemberContribution, "Non-member rate": in.NonMemberFee, "Venue cost total": in.VenueCostTotal, "Shuttle pack price": in.ShuttlePackPrice} {
		if v != nil && *v < 0 {
			return httpx.Unprocessable(label + " must not be negative.")
		}
	}
	if in.ShuttleUnitsPerPack != nil && *in.ShuttleUnitsPerPack <= 0 {
		return httpx.Unprocessable("Shuttle units per pack must be greater than 0.")
	}
	if in.ShuttlePerSession != nil && *in.ShuttlePerSession < 0 {
		return httpx.Unprocessable("Shuttle per session must not be negative.")
	}
	if in.Status != nil && !map[string]bool{"DRAFT": true, "ACTIVE": true, "COMPLETED": true, "CANCELLED": true}[*in.Status] {
		return httpx.Unprocessable("Invalid period status.")
	}
	return nil
}

func (s *Service) Create(c *fiber.Ctx) error {
	var in periodInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if err := in.validate(); err != nil {
		return httpx.WriteAppError(c, err)
	}
	weekdays := weekdaysOr(in.SessionWeekdays)
	// Session count is derived from the date range and picked weekdays, so a
	// 4-week and a 5-week period both work without manual entry.
	sessions := countSessionDays(*in.StartDate, *in.EndDate, weekdays)
	if sessions == 0 && in.NumberOfSessions != nil && *in.NumberOfSessions > 0 {
		// Old periods without weekdays keep their manual session count.
		sessions = *in.NumberOfSessions
	}
	p, err := scanPeriod(s.db.QueryRow(c.Context(),
		`INSERT INTO membership_periods
		 (id, name, start_date, end_date, number_of_sessions, max_members,
		  commitment_fee, member_contribution, non_member_fee,
		  venue_cost_total, shuttle_pack_price, shuttle_units_per_pack, shuttle_per_session,
		  status, session_weekdays, default_start_time, default_end_time)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'DRAFT', $13, $14, $15)
		 RETURNING `+periodCols,
		strings.TrimSpace(*in.Name), *in.StartDate, *in.EndDate, sessions, in.MaxMembers,
		val64(in.CommitmentFee), val64(in.MemberContribution), val64(in.NonMemberFee),
		val64(in.VenueCostTotal), valDef64(in.ShuttlePackPrice, 125000), valDefInt(in.ShuttleUnitsPerPack, 12), valDefInt(in.ShuttlePerSession, 24),
		weekdays, in.DefaultStartTime, in.DefaultEndTime))
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save period.")
	}
	return httpx.OK(c, http.StatusCreated, p)
}

// guardImmutable rejects edits on COMPLETED periods: history becomes
// read-only and changes must go through an explicit reopen. PRD §50.
func (s *Service) guardImmutable(ctx context.Context, id string) error {
	p, err := s.fetch(ctx, id)
	if err != nil {
		return err
	}
	if p.Status == "COMPLETED" {
		return httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed.")
	}
	return nil
}

func (s *Service) Update(c *fiber.Ctx) error {
	id := c.Params("id")
	if err := s.guardImmutable(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}
	var in periodInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if err := in.validate(); err != nil {
		return httpx.WriteAppError(c, err)
	}
	weekdays := weekdaysOr(in.SessionWeekdays)
	if in.SessionWeekdays == nil {
		// Partial edit (e.g. cost settings): keep existing weekdays, times,
		// quota and status instead of wiping them.
		cur, _ := s.fetch(c.Context(), id)
		weekdays = cur.SessionWeekdays
		if in.DefaultStartTime == nil {
			in.DefaultStartTime = cur.DefaultStartTime
		}
		if in.DefaultEndTime == nil {
			in.DefaultEndTime = cur.DefaultEndTime
		}
		if in.MaxMembers == nil {
			in.MaxMembers = cur.MaxMembers
		}
		if in.Status == nil {
			st := cur.Status
			in.Status = &st
		}
		sessions := countSessionDays(*in.StartDate, *in.EndDate, weekdays)
		if sessions == 0 && in.NumberOfSessions != nil && *in.NumberOfSessions > 0 {
			sessions = *in.NumberOfSessions
		}
		status := *in.Status
		// Keep existing cost settings when the edit form omits them.
		p, err := scanPeriod(s.db.QueryRow(c.Context(),
			`UPDATE membership_periods SET
			 name = $2, start_date = $3, end_date = $4, number_of_sessions = $5,
			 max_members = $6, commitment_fee = $7, member_contribution = $8,
			 non_member_fee = $9, venue_cost_total = $10,
			 shuttle_pack_price = $11, shuttle_units_per_pack = $12, shuttle_per_session = $13,
			 status = $14, session_weekdays = $15,
			 default_start_time = $16, default_end_time = $17, updated_at = now()
			 WHERE id = $1 RETURNING `+periodCols,
			id, strings.TrimSpace(*in.Name), *in.StartDate, *in.EndDate, sessions, in.MaxMembers,
			coalesce64(in.CommitmentFee, cur.CommitmentFee),
			coalesce64(in.MemberContribution, cur.MemberContribution),
			coalesce64(in.NonMemberFee, cur.NonMemberFee),
			coalesce64(in.VenueCostTotal, cur.VenueCostTotal),
			coalesce64(in.ShuttlePackPrice, cur.ShuttlePackPrice),
			coalesceInt(in.ShuttleUnitsPerPack, cur.ShuttleUnitsPerPack),
			coalesceInt(in.ShuttlePerSession, cur.ShuttlePerSession),
			status,
			weekdays, in.DefaultStartTime, in.DefaultEndTime))
		if err != nil {
			return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Period not found.")
		}
		return httpx.OK(c, http.StatusOK, p)
	}
	sessions := countSessionDays(*in.StartDate, *in.EndDate, weekdays)
	if sessions == 0 && in.NumberOfSessions != nil && *in.NumberOfSessions > 0 {
		sessions = *in.NumberOfSessions
	}
	status := "DRAFT"
	if in.Status != nil {
		status = *in.Status
	}
	// Keep existing cost settings when the edit form omits them.
	cur, _ := s.fetch(c.Context(), id)
	p, err := scanPeriod(s.db.QueryRow(c.Context(),
		`UPDATE membership_periods SET
		 name = $2, start_date = $3, end_date = $4, number_of_sessions = $5,
		 max_members = $6, commitment_fee = $7, member_contribution = $8,
		 non_member_fee = $9, venue_cost_total = $10,
		 shuttle_pack_price = $11, shuttle_units_per_pack = $12, shuttle_per_session = $13,
		 status = $14, session_weekdays = $15,
		 default_start_time = $16, default_end_time = $17, updated_at = now()
		 WHERE id = $1 RETURNING `+periodCols,
		id, strings.TrimSpace(*in.Name), *in.StartDate, *in.EndDate, sessions, in.MaxMembers,
		val64(in.CommitmentFee), val64(in.MemberContribution), val64(in.NonMemberFee),
		coalesce64(in.VenueCostTotal, cur.VenueCostTotal),
		coalesce64(in.ShuttlePackPrice, cur.ShuttlePackPrice),
		coalesceInt(in.ShuttleUnitsPerPack, cur.ShuttleUnitsPerPack),
		coalesceInt(in.ShuttlePerSession, cur.ShuttlePerSession),
		status,
		weekdays, in.DefaultStartTime, in.DefaultEndTime))
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Period not found.")
	}
	return httpx.OK(c, http.StatusOK, p)
}

// Delete removes a period. With sessions, members or money it refuses unless
// ?force=true, which hard-deletes the whole period tree (sessions and their
// histories, memberships, period money records).
func (s *Service) Delete(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := s.fetch(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}
	var sessions, members, money int
	_ = s.db.QueryRow(c.Context(),
		`SELECT COUNT(*) FROM mabar_sessions WHERE period_id = $1`, id).Scan(&sessions)
	_ = s.db.QueryRow(c.Context(),
		`SELECT COUNT(*) FROM memberships WHERE period_id = $1 AND status <> 'WITHDRAWN'`, id).Scan(&members)
	_ = s.db.QueryRow(c.Context(),
		`SELECT (SELECT COUNT(*) FROM revenues WHERE period_id = $1)
		        + (SELECT COUNT(*) FROM expenses WHERE period_id = $1)`, id).Scan(&money)
	if (sessions > 0 || members > 0 || money > 0) && c.Query("force") != "true" {
		return httpx.WriteAppError(c, httpx.Conflict("PERIOD_HAS_HISTORY",
			"Period already has sessions, members, or money. Delete with force to wipe everything."))
	}
	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete period.")
	}
	defer tx.Rollback(c.Context())
	exec := func(q string) error {
		_, err := tx.Exec(c.Context(), q, id)
		return err
	}
	// Wipe every session of this period exactly like a forced session delete.
	for _, q := range []string{
		`DELETE FROM payments WHERE bill_id IN (SELECT b.id FROM player_bills b JOIN mabar_sessions ms ON ms.id = b.session_id WHERE ms.period_id = $1)`,
		`DELETE FROM player_bills WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM shuttlecock_transactions WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM matches WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM attendances WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM session_player_stats WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM revenues WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM expenses WHERE session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`,
		`DELETE FROM mabar_sessions WHERE period_id = $1`,
		`DELETE FROM memberships WHERE period_id = $1`,
		`DELETE FROM revenues WHERE period_id = $1`,
		`DELETE FROM expenses WHERE period_id = $1`,
	} {
		if err := exec(q); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete period.")
		}
	}
	tag, err := tx.Exec(c.Context(), `DELETE FROM membership_periods WHERE id = $1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Period not found.")
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete period.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) Complete(c *fiber.Ctx) error {	id := c.Params("id")
	if _, err := s.fetch(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}
	tag, err := s.db.Exec(c.Context(),
		`UPDATE membership_periods SET status = 'COMPLETED', updated_at = now() WHERE id = $1 AND status <> 'COMPLETED'`, id)
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed.")
		return nil
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"id": id, "status": "COMPLETED"})
}

func (s *Service) ListMembers(c *fiber.Ctx) error {
	id := c.Params("id")
	p, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	rows, err := s.db.Query(c.Context(), `
	SELECT m.id::text, m.period_id::text, m.player_id::text, pl.name, m.commitment_fee,
	       m.joined_at::text, m.status, COALESCE(a.cnt, 0)
		FROM memberships m
		JOIN players pl ON pl.id = m.player_id
		LEFT JOIN (
			SELECT att.player_id, COUNT(*) AS cnt
			FROM attendances att
			JOIN mabar_sessions ms ON ms.id = att.session_id
			WHERE ms.period_id = $1 AND att.status = 'PRESENT'
			GROUP BY att.player_id
		) a ON a.player_id = m.player_id
		WHERE m.period_id = $1 AND m.status <> 'WITHDRAWN'
		ORDER BY pl.name`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load members.")
	}
	defer rows.Close()

	members := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ID, &m.PeriodID, &m.PlayerID, &m.PlayerName, &m.CommitmentFee,
			&m.JoinedAt, &m.Status, &m.AttendanceCount); err == nil {
			m.TotalBill = finance.PeriodMemberBill(m.CommitmentFee, p.MemberContribution, int64(m.AttendanceCount))
			m.NonMemberCost = finance.NonMemberBill(p.NonMemberFee, int64(m.AttendanceCount))
			m.Benefit = m.NonMemberCost - m.TotalBill
			m.PaidBySource = map[string]int64{}
			members = append(members, m)
		}
	}
	// Attach per-member payment totals from finance revenues recorded with
	// this period and player. A separate query keeps the main scan stable.
	if prows, err := s.db.Query(c.Context(), `
		SELECT player_id::text, source, COALESCE(SUM(amount), 0)
		FROM revenues
		WHERE period_id = $1 AND player_id IS NOT NULL
		GROUP BY player_id, source`, id); err == nil {
		paid := map[string]map[string]int64{}
		for prows.Next() {
			var pid, src string
			var sum int64
			if err := prows.Scan(&pid, &src, &sum); err == nil {
				if paid[pid] == nil {
					paid[pid] = map[string]int64{}
				}
				paid[pid][src] = sum
			}
		}
		prows.Close()
		for i := range members {
			if sums, ok := paid[members[i].PlayerID]; ok {
				members[i].PaidBySource = sums
			}
		}
	}
	// Visit payments collected per session (paid period-session bills) count
	// toward the member balance under VISIT_BILLS.
	if brows, err := s.db.Query(c.Context(), `
		SELECT b.player_id::text, COALESCE(SUM(b.total), 0)
		FROM player_bills b
		JOIN mabar_sessions ms ON ms.id = b.session_id
		WHERE ms.period_id = $1 AND b.payment_status = 'PAID'
		GROUP BY b.player_id`, id); err == nil {
		for brows.Next() {
			var pid string
			var sum int64
			if err := brows.Scan(&pid, &sum); err == nil {
				for i := range members {
					if members[i].PlayerID == pid {
						members[i].PaidBySource["VISIT_BILLS"] += sum
					}
				}
			}
		}
		brows.Close()
	}
	return httpx.OK(c, http.StatusOK, members)
}

type memberInput struct {
	PlayerID string `json:"player_id"`
}

func (s *Service) AddMember(c *fiber.Ctx) error {
	id := c.Params("id")
	if err := s.guardImmutable(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}
	var in memberInput
	if err := httpx.Decode(c, &in); err != nil || in.PlayerID == "" {
		return httpx.Err(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "A player must be selected.")
	}
	if _, err := uuid.Parse(in.PlayerID); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
	}
	p, _ := s.fetch(c.Context(), id)
	if p.MaxMembers != nil {
		var count int
		_ = s.db.QueryRow(c.Context(),
			`SELECT COUNT(*) FROM memberships WHERE period_id = $1 AND status <> 'WITHDRAWN'`, id).Scan(&count)
		if count >= *p.MaxMembers {
			return httpx.WriteAppError(c, httpx.Conflict("MEMBERS_FULL", "Period member quota is full."))
		}
	}
	var mID, joinedAt string
	err := s.db.QueryRow(c.Context(),
		`INSERT INTO memberships (id, period_id, player_id, commitment_fee)
		 VALUES (gen_random_uuid(), $1, $2, $3)
		 ON CONFLICT (period_id, player_id) DO UPDATE SET status = 'ACTIVE', commitment_fee = EXCLUDED.commitment_fee
		 RETURNING id::text, joined_at::text`, id, in.PlayerID, p.CommitmentFee).Scan(&mID, &joinedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not add member.")
	}
	return httpx.OK(c, http.StatusCreated, Membership{ID: mID, PeriodID: id, PlayerID: in.PlayerID, CommitmentFee: p.CommitmentFee, JoinedAt: joinedAt, Status: "ACTIVE", PaidBySource: map[string]int64{}})
}

func (s *Service) RemoveMember(c *fiber.Ctx) error {
	id := c.Params("id")
	playerID := c.Params("playerId")
	if err := s.guardImmutable(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}
	// ?hard=true permanently deletes the membership AND every payment
	// recorded for this player in this period (revenues). Session history
	// (attendance, session bills) is left untouched. Default is a reversible
	// withdraw that keeps the money in finance.
	if c.Query("hard") == "true" {
		tx, err := s.db.Begin(c.Context())
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete member.")
		}
		defer tx.Rollback(c.Context())
		var dropped int64
		if err := tx.QueryRow(c.Context(),
			`WITH d AS (DELETE FROM revenues WHERE period_id = $1 AND player_id = $2 RETURNING 1)
			 SELECT COUNT(*) FROM d`, id, playerID).Scan(&dropped); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete member.")
		}
		tag, err := tx.Exec(c.Context(),
			`DELETE FROM memberships WHERE period_id = $1 AND player_id = $2`, id, playerID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete member.")
		}
		if tag.RowsAffected() == 0 {
			return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Membership not found.")
		}
		if err := tx.Commit(c.Context()); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete member.")
		}
		return httpx.OK(c, http.StatusOK, map[string]any{"ok": true, "payments_deleted": dropped})
	}
	tag, err := s.db.Exec(c.Context(),
		`UPDATE memberships SET status = 'WITHDRAWN' WHERE period_id = $1 AND player_id = $2 AND status <> 'WITHDRAWN'`,
		id, playerID)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Membership not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

// Summary is the period dashboard: profit, cash flow and shuttlecock totals
// per PRD §36. Court expense is real cash out; shuttlecock usage cost is an
// accrual at weighted average purchase price and never equals cash flow.
// Projection (from period settings + members + attendance) is always returned
// so a new period shows numbers before manual finance entries exist.
func (s *Service) Summary(c *fiber.Ctx) error {
	id := c.Params("id")
	p, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	var revenue, venueCost, purchaseCost int64
	var unitsUsed, unitsPurchased int64
	var sessions int
	_ = s.db.QueryRow(c.Context(), `
		SELECT
			COALESCE((SELECT SUM(amount) FROM revenues WHERE period_id = $1), 0),
			COALESCE((SELECT SUM(amount) FROM expenses WHERE period_id = $1 AND category = 'VENUE'), 0),
			COALESCE((SELECT SUM(amount) FROM expenses WHERE period_id = $1 AND category = 'SHUTTLECOCK_PURCHASE'), 0),
			COALESCE((SELECT SUM(units) FROM shuttlecock_transactions WHERE type = 'USAGE' AND session_id IN (SELECT id FROM mabar_sessions WHERE period_id = $1)), 0),
			COALESCE((SELECT SUM(units) FROM shuttlecock_transactions WHERE type = 'PURCHASE'), 0),
			(SELECT COUNT(*) FROM mabar_sessions WHERE period_id = $1)`, id).
		Scan(&revenue, &venueCost, &purchaseCost, &unitsUsed, &unitsPurchased, &sessions)

	// Per-unit avg: unit_price is stored per pack, so divide by pack size.
	var avgPerUnit int64
	_ = s.db.QueryRow(c.Context(), `
		SELECT COALESCE(SUM(t.unit_price * t.units / NULLIF(pr.units_per_pack, 0)) / NULLIF(SUM(t.units), 0), 0)
		FROM shuttlecock_transactions t
		JOIN shuttlecock_products pr ON pr.id = t.product_id
		WHERE t.type = 'PURCHASE'`).Scan(&avgPerUnit)

	var shuttleCost int64
	if avgPerUnit == 0 && p.ShuttleUnitsPerPack > 0 {
		shuttleCost = finance.RoundHalfUp(finance.ShuttlecockCost(p.ShuttlePackPrice, int64(p.ShuttleUnitsPerPack), unitsUsed))
	} else {
		shuttleCost = finance.RoundHalfUp(finance.ShuttlecockCost(avgPerUnit, 1, unitsUsed))
	}
	operatingCost := venueCost + shuttleCost
	profit := revenue - operatingCost

	// Billed so far from actual data: member commitment + hadir-based
	// contribution + non-member hadir. Absent members pay nothing.
	var memberCount int
	var commitmentTotal int64
	var memberPresent, nonMemberPresent int
	_ = s.db.QueryRow(c.Context(),
		`SELECT COUNT(*), COALESCE(SUM(commitment_fee), 0) FROM memberships WHERE period_id = $1 AND status <> 'WITHDRAWN'`,
		id).Scan(&memberCount, &commitmentTotal)
	_ = s.db.QueryRow(c.Context(), `
		SELECT
			COALESCE(COUNT(*) FILTER (WHERE a.status = 'PRESENT' AND a.is_member), 0),
			COALESCE(COUNT(*) FILTER (WHERE a.status = 'PRESENT' AND NOT a.is_member), 0)
		FROM attendances a
		JOIN mabar_sessions ms ON ms.id = a.session_id
		WHERE ms.period_id = $1`, id).Scan(&memberPresent, &nonMemberPresent)

	billedMemberContrib := int64(memberPresent) * p.MemberContribution
	billedNonMember := int64(nonMemberPresent) * p.NonMemberFee
	billedRevenue := commitmentTotal + billedMemberContrib + billedNonMember

	// Full projection for the whole period (all members attend all sessions).
	planSessions := p.NumberOfSessions
	if planSessions <= 0 {
		planSessions = sessions
	}
	projUnits := int64(p.ShuttlePerSession) * int64(planSessions)
	projShuttleCost := finance.RoundHalfUp(finance.ShuttlecockCost(p.ShuttlePackPrice, int64(maxInt(p.ShuttleUnitsPerPack, 1)), projUnits))
	projMemberContrib := int64(memberCount) * int64(planSessions) * p.MemberContribution
	projRevenueFull := commitmentTotal + projMemberContrib
	projOperating := p.VenueCostTotal + projShuttleCost
	// To-date projection uses setting venue prorated + actual billed.
	projOperatingToDate := p.VenueCostTotal + shuttleCost

	return httpx.OK(c, http.StatusOK, map[string]any{
		"period":                    p,
		"total_revenue":             revenue,
		"billed_revenue":            billedRevenue,
		"commitment_total":          commitmentTotal,
		"member_count":              memberCount,
		"member_present":            memberPresent,
		"non_member_present":        nonMemberPresent,
		"total_venue_cost":          venueCost,
		"shuttlecock_usage_cost":    shuttleCost,
		"shuttlecock_purchase_cost": purchaseCost,
		"operating_cost":            operatingCost,
		"operating_profit":          profit,
		"status":                    finance.FinancialStatus(revenue, operatingCost),
		"cash_in":                   revenue,
		"cash_out":                  venueCost + purchaseCost,
		"cash_flow":                 revenue - venueCost - purchaseCost,
		"shuttlecock_used":          unitsUsed,
		"shuttlecock_stock":         unitsPurchased - unitsUsed,
		"sessions":                  sessions,
		"avg_profit_per_session":    avg(int64(sessions), profit),
		"projection": map[string]any{
			"venue_total":            p.VenueCostTotal,
			"sessions":               planSessions,
			"shuttle_per_session":    p.ShuttlePerSession,
			"shuttle_units":          projUnits,
			"shuttle_cost":           projShuttleCost,
			"operating_cost":         projOperating,
			"operating_cost_to_date": projOperatingToDate,
			"revenue_full":           projRevenueFull,
			"revenue_billed":         billedRevenue,
			"profit_full":            projRevenueFull - projOperating,
			"profit_billed":          billedRevenue - projOperatingToDate,
			"status_full":            finance.FinancialStatus(projRevenueFull, projOperating),
		},
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ShuttlecockMatrix is the player × session usage table from PRD §57. It
// reads detailed 2v2 matches only: each player in a match carries the full
// match count (contribution metric, not inventory reduction, PRD §23).
// Simple recap numbers live in their own session panel and are deliberately
// kept out, so the two input modes never double-count each other.
func (s *Service) ShuttlecockMatrix(c *fiber.Ctx) error {
	id := c.Params("id")
	rows, err := s.db.Query(c.Context(), `
		SELECT pl.name, ms.date::text, SUM(m.shuttlecock_used::bigint)
		FROM match_players mp
		JOIN matches m ON m.id = mp.match_id
		JOIN mabar_sessions ms ON ms.id = m.session_id
		JOIN players pl ON pl.id = mp.player_id
		WHERE ms.period_id = $1
		GROUP BY pl.name, ms.date
		ORDER BY pl.name, ms.date`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load shuttlecock matrix.")
	}
	defer rows.Close()

	type cell struct {
		Name  string         `json:"name"`
		Cells map[string]int `json:"cells"`
		Total int            `json:"total"`
	}
	byName := map[string]*cell{}
	order := []string{}
	dates := map[string]bool{}
	for rows.Next() {
		var name, date string
		var n int
		if err := rows.Scan(&name, &date, &n); err != nil {
			continue
		}
		dates[date] = true
		row, ok := byName[name]
		if !ok {
			row = &cell{Name: name, Cells: map[string]int{}}
			byName[name] = row
			order = append(order, name)
		}
		row.Cells[date] = n
		row.Total += n
	}
	sessionDates := []string{}
	for d := range dates {
		sessionDates = append(sessionDates, d)
	}
	sort.Strings(sessionDates)

	matrix := make([]cell, 0, len(order))
	for _, name := range order {
		matrix = append(matrix, *byName[name])
	}

	totalUnits := 0
	for _, c := range matrix {
		totalUnits += c.Total
	}
	return httpx.OK(c, http.StatusOK, map[string]any{
		"dates":           sessionDates,
		"rows":            matrix,
		"total":           totalUnits,
		"avg_per_session": avg(int64(len(sessionDates)), int64(totalUnits)),
	})
}

// SessionBreakdown lists one row per session of the period with that
// night's own revenue, cost and profit. It keeps per-session results
// separate from the full-period totals: period-level income or costs that
// are not linked to any session only appear in Summary, never here.
func (s *Service) SessionBreakdown(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := s.fetch(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}

	type sess struct {
		ID       string
		Date     string
		Court    int64
		Pack     int64
		PerPack  int
		Present  int
		Revenue  int64
		Units    int
	}
	sessions := []sess{}
	srows, err := s.db.Query(c.Context(), `
		SELECT id::text, date::text, court_cost, shuttle_pack_price, shuttle_units_per_pack
		FROM mabar_sessions WHERE period_id = $1 ORDER BY date`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load sessions.")
	}
	for srows.Next() {
		var x sess
		if err := srows.Scan(&x.ID, &x.Date, &x.Court, &x.Pack, &x.PerPack); err == nil {
			sessions = append(sessions, x)
		}
	}
	srows.Close()

	byID := map[string]*sess{}
	for i := range sessions {
		byID[sessions[i].ID] = &sessions[i]
	}
	collect := func(ctx context.Context, q string, into func(*sess, int64)) {
		prows, err := s.db.Query(ctx, q, id)
		if err != nil {
			return
		}
		defer prows.Close()
		for prows.Next() {
			var sid string
			var n int64
			if err := prows.Scan(&sid, &n); err == nil {
				if x, ok := byID[sid]; ok {
					into(x, n)
				}
			}
		}
	}
	inPeriod := `IN (SELECT id FROM mabar_sessions WHERE period_id = $1)`
	collect(c.Context(), `SELECT session_id::text, COUNT(*) FROM attendances
		WHERE status = 'PRESENT' AND session_id `+inPeriod+` GROUP BY session_id`,
		func(x *sess, n int64) { x.Present = int(n) })
	collect(c.Context(), `SELECT session_id::text, COALESCE(SUM(amount), 0) FROM revenues
		WHERE session_id `+inPeriod+` GROUP BY session_id`,
		func(x *sess, n int64) { x.Revenue += n })
	collect(c.Context(), `SELECT session_id::text, COALESCE(SUM(total), 0) FROM player_bills
		WHERE payment_status = 'PAID' AND session_id `+inPeriod+` GROUP BY session_id`,
		func(x *sess, n int64) { x.Revenue += n })
	collect(c.Context(), `SELECT session_id::text, COALESCE(SUM(units), 0) FROM shuttlecock_transactions
		WHERE type = 'USAGE' AND session_id `+inPeriod+` GROUP BY session_id`,
		func(x *sess, n int64) { x.Units = int(n) })

	var avgPerUnit int64
	_ = s.db.QueryRow(c.Context(), `
		SELECT COALESCE(SUM(t.unit_price * t.units / NULLIF(pr.units_per_pack, 0)) / NULLIF(SUM(t.units), 0), 0)
		FROM shuttlecock_transactions t
		JOIN shuttlecock_products pr ON pr.id = t.product_id
		WHERE t.type = 'PURCHASE'`).Scan(&avgPerUnit)

	type outRow struct {
		SessionID      string `json:"session_id"`
		Date           string `json:"date"`
		PlayersPresent int    `json:"players_present"`
		Revenue        int64  `json:"revenue"`
		CourtCost      int64  `json:"court_cost"`
		ShuttlecockUsed int   `json:"shuttlecock_used"`
		ShuttlecockCost int64 `json:"shuttlecock_cost"`
		OperatingCost  int64  `json:"operating_cost"`
		Profit         int64  `json:"profit"`
		Status         string `json:"status"`
	}
	out := make([]outRow, 0, len(sessions))
	for _, x := range sessions {
		var shuttleCost int64
		if avgPerUnit == 0 && x.PerPack > 0 {
			shuttleCost = finance.RoundHalfUp(finance.ShuttlecockCost(x.Pack, int64(x.PerPack), int64(x.Units)))
		} else {
			shuttleCost = finance.RoundHalfUp(finance.ShuttlecockCost(avgPerUnit, 1, int64(x.Units)))
		}
		opCost := x.Court + shuttleCost
		out = append(out, outRow{
			SessionID: x.ID, Date: x.Date, PlayersPresent: x.Present,
			Revenue: x.Revenue, CourtCost: x.Court,
			ShuttlecockUsed: x.Units, ShuttlecockCost: shuttleCost,
			OperatingCost: opCost, Profit: x.Revenue - opCost,
			Status: finance.FinancialStatus(x.Revenue, opCost),
		})
	}
	return httpx.OK(c, http.StatusOK, out)
}

// GenerateSessions creates one PERIOD mabar session per picked weekday
// between start and end. It is idempotent: dates that already have a session
// for this period are skipped, so re-running after adding a weekday only
// fills the gaps. Default start/end times are copied onto new sessions.
func (s *Service) GenerateSessions(c *fiber.Ctx) error {
	id := c.Params("id")
	if err := s.guardImmutable(c.Context(), id); err != nil {
		return httpx.WriteAppError(c, err)
	}
	p, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	if len(p.SessionWeekdays) == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pick at least one weekday on the period first."))
	}
	start, err1 := time.Parse("2006-01-02", p.StartDate)
	end, err2 := time.Parse("2006-01-02", p.EndDate)
	if err1 != nil || err2 != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Period dates are invalid.")
	}

	existing := map[string]bool{}
	rows, err := s.db.Query(c.Context(),
		`SELECT date::text FROM mabar_sessions WHERE period_id = $1`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load existing sessions.")
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil {
			existing[d] = true
		}
	}
	rows.Close()

	wanted := map[int]bool{}
	for _, d := range p.SessionWeekdays {
		wanted[int(d)] = true
	}

	created := []string{}
	perSessionCourt := int64(0)
	if p.NumberOfSessions > 0 {
		perSessionCourt = p.VenueCostTotal / int64(p.NumberOfSessions)
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		if !wanted[int(d.Weekday())] || existing[date] {
			continue
		}
		var newID string
		if err := s.db.QueryRow(c.Context(), `
			INSERT INTO mabar_sessions (id, type, period_id, date, start_time, end_time, court_cost, shuttlecock_price, shuttle_pack_price, shuttle_units_per_pack)
			VALUES (gen_random_uuid(), 'PERIOD', $1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id::text`, id, date, p.DefaultStartTime, p.DefaultEndTime,
			perSessionCourt, perUnitPrice(p.ShuttlePackPrice, p.ShuttleUnitsPerPack),
			p.ShuttlePackPrice, maxInt(p.ShuttleUnitsPerPack, 1)).Scan(&newID); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create sessions.")
		}
		created = append(created, date)
	}
	return httpx.OK(c, http.StatusCreated, map[string]any{"created": created, "count": len(created)})
}

func perUnitPrice(packPrice int64, perPack int) int64 {
	if perPack <= 0 {
		return packPrice
	}
	return packPrice / int64(perPack)
}

// countSessionDays counts dates in [start, end] whose weekday is picked, so
// number_of_sessions always reflects the real calendar (4, 5, or more weeks).
func countSessionDays(start, end string, weekdays []int32) int {
	s, err1 := time.Parse("2006-01-02", start)
	e, err2 := time.Parse("2006-01-02", end)
	if err1 != nil || err2 != nil || len(weekdays) == 0 {
		return 0
	}
	wanted := map[int]bool{}
	for _, d := range weekdays {
		wanted[int(d)] = true
	}
	n := 0
	for d := s; !d.After(e); d = d.AddDate(0, 0, 1) {
		if wanted[int(d.Weekday())] {
			n++
		}
	}
	return n
}

func weekdaysOr(in *[]int32) []int32 {
	if in == nil {
		return []int32{}
	}
	return *in
}

func avg(n int64, total int64) int64 {
	if n == 0 {
		return 0
	}
	return total / n
}

func val64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func valDef64(p *int64, def int64) int64 {
	if p == nil {
		return def
	}
	return *p
}

func valDefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func coalesce64(p *int64, cur int64) int64 {
	if p == nil {
		return cur
	}
	return *p
}

func coalesceInt(p *int, cur int) int {
	if p == nil {
		return cur
	}
	return *p
}

type MatrixSession struct {
	ID           string `json:"id"`
	Date         string `json:"date"`
	Status       string `json:"status"`
	PresentCount int    `json:"present_count"`
	TotalKasKok  int64  `json:"total_kas_kok"`
}

type MatrixMemberRow struct {
	PlayerID             string            `json:"player_id"`
	PlayerName           string            `json:"player_name"`
	CommitmentFee        int64             `json:"commitment_fee"`
	CommitmentPaid       bool              `json:"commitment_paid"`
	CommitmentAmountPaid int64             `json:"commitment_amount_paid"`
	Attendances          map[string]string `json:"attendances"`
	PresentCount         int               `json:"present_count"`
}

type MatrixNonMemberRow struct {
	SessionID  string `json:"session_id"`
	PlayerID   string `json:"player_id"`
	PlayerName string `json:"player_name"`
	Fee        int64  `json:"fee"`
	Paid       bool   `json:"paid"`
	Note       string `json:"note"`
}

// AttendanceMatrix aggregates attendance and kas kok data across all sessions
// of a period for each active member, producing a matrix table matching the
// club's weekly member recap spreadsheet.
func (s *Service) AttendanceMatrix(c *fiber.Ctx) error {
	id := c.Params("id")
	p, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	// 1. Fetch all sessions for this period ordered by date
	srows, err := s.db.Query(c.Context(), `
		SELECT id::text, date::text, status
		FROM mabar_sessions
		WHERE period_id = $1
		ORDER BY date ASC, created_at ASC`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load period sessions.")
	}
	defer srows.Close()

	sessions := []MatrixSession{}
	sessionMap := map[string]int{}
	for srows.Next() {
		var ms MatrixSession
		if err := srows.Scan(&ms.ID, &ms.Date, &ms.Status); err == nil {
			sessionMap[ms.ID] = len(sessions)
			sessions = append(sessions, ms)
		}
	}
	srows.Close()

	// 2. Fetch active members for this period
	mrows, err := s.db.Query(c.Context(), `
		SELECT m.player_id::text, pl.name, m.commitment_fee
		FROM memberships m
		JOIN players pl ON pl.id = m.player_id
		WHERE m.period_id = $1 AND m.status <> 'WITHDRAWN'
		ORDER BY pl.name ASC`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load members.")
	}
	defer mrows.Close()

	rows := []MatrixMemberRow{}
	memberIndexMap := map[string]int{}
	for mrows.Next() {
		var mr MatrixMemberRow
		if err := mrows.Scan(&mr.PlayerID, &mr.PlayerName, &mr.CommitmentFee); err == nil {
			mr.Attendances = make(map[string]string)
			memberIndexMap[mr.PlayerID] = len(rows)
			rows = append(rows, mr)
		}
	}
	mrows.Close()

	// 3. Fetch commitment payments per member
	prows, err := s.db.Query(c.Context(), `
		SELECT player_id::text, COALESCE(SUM(amount), 0)
		FROM revenues
		WHERE period_id = $1 AND source = 'COMMITMENT_FEE' AND player_id IS NOT NULL
		GROUP BY player_id`, id)
	if err == nil {
		for prows.Next() {
			var pid string
			var sum int64
			if err := prows.Scan(&pid, &sum); err == nil {
				if idx, ok := memberIndexMap[pid]; ok {
					rows[idx].CommitmentAmountPaid = sum
				}
			}
		}
		prows.Close()
	}

	for i := range rows {
		requiredFee := rows[i].CommitmentFee
		if requiredFee <= 0 {
			requiredFee = p.CommitmentFee
		}
		if requiredFee <= 0 {
			rows[i].CommitmentPaid = true
		} else {
			rows[i].CommitmentPaid = rows[i].CommitmentAmountPaid >= requiredFee
		}
	}

	// 4. Fetch attendance records for sessions in this period
	arows, err := s.db.Query(c.Context(), `
		SELECT a.session_id::text, a.player_id::text, a.status
		FROM attendances a
		JOIN mabar_sessions ms ON ms.id = a.session_id
		WHERE ms.period_id = $1`, id)
	if err == nil {
		for arows.Next() {
			var sid, pid, status string
			if err := arows.Scan(&sid, &pid, &status); err == nil {
				if midx, ok := memberIndexMap[pid]; ok {
					rows[midx].Attendances[sid] = status
					if status == "PRESENT" {
						rows[midx].PresentCount++
						if sidx, ok := sessionMap[sid]; ok {
							sessions[sidx].PresentCount++
						}
					}
				}
			}
		}
		arows.Close()
	}

	// 5. Fetch non-members attending sessions in this period
	nonMembers := []MatrixNonMemberRow{}
	nonMemberFee := p.NonMemberFee
	if nonMemberFee <= 0 {
		nonMemberFee = 25000
	}
	nmRows, err := s.db.Query(c.Context(), `
		SELECT a.session_id::text, a.player_id::text, pl.name,
		       COALESCE(b.total, $2),
		       COALESCE(b.payment_status, 'PAID'),
		       COALESCE(b.payment_method, a.no_show_reason, '')
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		JOIN mabar_sessions ms ON ms.id = a.session_id
		LEFT JOIN player_bills b ON b.session_id = a.session_id AND b.player_id = a.player_id
		WHERE ms.period_id = $1 AND (a.is_member = false OR a.player_id NOT IN (
			SELECT player_id FROM memberships WHERE period_id = $1 AND status <> 'WITHDRAWN'
		)) AND a.status = 'PRESENT'
		ORDER BY ms.date ASC, a.listed_at ASC, pl.name ASC`, id, nonMemberFee)
	if err == nil {
		for nmRows.Next() {
			var nm MatrixNonMemberRow
			var payStatus, payMethod string
			if err := nmRows.Scan(&nm.SessionID, &nm.PlayerID, &nm.PlayerName, &nm.Fee, &payStatus, &payMethod); err == nil {
				nm.Paid = payStatus == "PAID"
				nm.Note = payMethod
				nonMembers = append(nonMembers, nm)
			}
		}
		nmRows.Close()
	}

	// 6. Calculate totals
	var totalLapanganPaid int64
	for _, m := range rows {
		totalLapanganPaid += m.CommitmentAmountPaid
	}

	var totalKasKok int64
	for i := range sessions {
		sessions[i].TotalKasKok = int64(sessions[i].PresentCount) * p.MemberContribution
		totalKasKok += sessions[i].TotalKasKok
	}

	return httpx.OK(c, http.StatusOK, map[string]any{
		"sessions":            sessions,
		"rows":                rows,
		"non_members":         nonMembers,
		"commitment_fee":      p.CommitmentFee,
		"member_contribution": p.MemberContribution,
		"non_member_fee":      p.NonMemberFee,
		"total_lapangan_paid": totalLapanganPaid,
		"total_kas_kok":       totalKasKok,
	})
}

