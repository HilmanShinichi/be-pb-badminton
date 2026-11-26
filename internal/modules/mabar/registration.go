package mabar

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type RegistrationSessionInfo struct {
	ID                         string  `json:"id"`
	Type                       string  `json:"type"`
	PeriodID                   *string `json:"period_id"`
	PeriodName                 *string `json:"period_name"`
	VenueName                  *string `json:"venue_name"`
	VenueDescription           *string `json:"venue_description"`
	Date                       string  `json:"date"`
	StartTime                  *string `json:"start_time"`
	EndTime                    *string `json:"end_time"`
	Description                *string `json:"description"`
	RegistrationOpensAt        *string `json:"registration_opens_at"`
	MemberRegistrationClosesAt *string `json:"member_registration_closes_at"`
	RegistrationClosesAt       *string `json:"registration_closes_at"`
	MaxMembers                 int     `json:"max_members"`
	MaxNonMembers              int     `json:"max_non_members"`
	MaxWaitingList             int     `json:"max_waiting_list"`
	AdminNote                  *string `json:"admin_note"`
	ServerTime                 string  `json:"server_time"`
	Status                     string  `json:"status"` // UPCOMING, MEMBER_ONLY, OPEN, CLOSED, NOT_CONFIGURED
	MemberRegistrationOpen     bool    `json:"member_registration_open"`
	NonMemberRegistrationOpen  bool    `json:"non_member_registration_open"`
}

type RegistrationQuotas struct {
	MaxMembers              int `json:"max_members"`
	MaxNonMembers           int `json:"max_non_members"`
	EffectiveMaxNonMembers  int `json:"effective_max_non_members"`
	BonusNonMemberSlots     int `json:"bonus_non_member_slots"`
	MaxWaitingList          int `json:"max_waiting_list"`
	MemberCount             int `json:"member_count"`
	NonMemberCount          int `json:"non_member_count"`
	WaitingCount            int `json:"waiting_count"`
	AvailableMemberSlots    int `json:"available_member_slots"`
	AvailableNonMemberSlots int `json:"available_non_member_slots"`
	AvailableWaitingSlots   int `json:"available_waiting_slots"`
}

type RegistrationPeriodMember struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Phone            *string `json:"phone"`
	AttendanceStatus string  `json:"attendance_status"`
	IsRegistered     bool    `json:"is_registered"`
	CommitmentFee    int64   `json:"commitment_fee"`
}

type RegistrationNonMemberPlayer struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IsRegistered bool   `json:"is_registered"`
}

type ParticipantItem struct {
	AttendanceID string `json:"attendance_id"`
	PlayerID     string `json:"player_id"`
	PlayerName   string `json:"player_name"`
	IsMember     bool   `json:"is_member"`
	Status       string `json:"status"`
	ListedAt     string `json:"listed_at"`
}

type RegistrationParticipants struct {
	Members     []ParticipantItem `json:"members"`
	NonMembers  []ParticipantItem `json:"non_members"`
	WaitingList []ParticipantItem `json:"waiting_list"`
}

type RegistrationResponse struct {
	Session          RegistrationSessionInfo       `json:"session"`
	Quotas           RegistrationQuotas            `json:"quotas"`
	PeriodMembers    []RegistrationPeriodMember    `json:"period_members"`
	NonMemberPlayers []RegistrationNonMemberPlayer `json:"non_member_players"`
	Participants     RegistrationParticipants      `json:"participants"`
}

func computeRegistrationStatus(opensAt, memberClosesAt, closesAt *time.Time, now time.Time) string {
	if opensAt == nil || closesAt == nil {
		return "NOT_CONFIGURED"
	}
	if now.Before(*opensAt) {
		return "UPCOMING"
	}
	if now.After(*closesAt) {
		return "CLOSED"
	}
	if memberClosesAt != nil && now.Before(*memberClosesAt) {
		return "MEMBER_ONLY"
	}
	return "OPEN"
}

func isMemberDeadlinePassed(memberClosesAt *time.Time, now time.Time) bool {
	return memberClosesAt != nil && !now.Before(*memberClosesAt)
}

func computeRegistrationQuotas(
	maxMembers, maxNonMembers, maxWaitingList int,
	memberCount, nonMemberCount, waitingCount int,
	status string,
	memberClosesAt *time.Time,
	now time.Time,
) RegistrationQuotas {
	isWindowOpen := status == "MEMBER_ONLY" || status == "OPEN"
	memberClosed := isMemberDeadlinePassed(memberClosesAt, now)

	bonusNonMemberSlots := 0
	if memberClosed {
		remainingMembers := maxMembers - memberCount
		if remainingMembers > 0 {
			bonusNonMemberSlots = remainingMembers
		}
	}
	effectiveMaxNonMembers := maxNonMembers + bonusNonMemberSlots

	availMembers := maxMembers - memberCount
	if availMembers < 0 {
		availMembers = 0
	}
	if !isWindowOpen || memberClosed {
		availMembers = 0
	}

	availNonMember := effectiveMaxNonMembers - nonMemberCount
	if availNonMember < 0 {
		availNonMember = 0
	}
	if !isWindowOpen {
		availNonMember = 0
	}

	availWaiting := maxWaitingList - waitingCount
	if availWaiting < 0 {
		availWaiting = 0
	}
	if !isWindowOpen {
		availWaiting = 0
	}

	return RegistrationQuotas{
		MaxMembers:              maxMembers,
		MaxNonMembers:           maxNonMembers,
		EffectiveMaxNonMembers:  effectiveMaxNonMembers,
		BonusNonMemberSlots:     bonusNonMemberSlots,
		MaxWaitingList:          maxWaitingList,
		MemberCount:             memberCount,
		NonMemberCount:          nonMemberCount,
		WaitingCount:            waitingCount,
		AvailableMemberSlots:    availMembers,
		AvailableNonMemberSlots: availNonMember,
		AvailableWaitingSlots:   availWaiting,
	}
}

func (s *Service) GetRegistrationInfo(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}

	var sess RegistrationSessionInfo
	var opensAtStr, memberClosesAtStr, closesAtStr *string
	var opensAt, memberClosesAt, closesAt *time.Time

	err := s.db.QueryRow(c.Context(), `
		SELECT s.id::text, s.type, s.period_id::text, p.name, v.name,
		       s.venue_description, s.date::text, s.start_time::text, s.end_time::text,
		       s.description, s.admin_note, s.registration_opens_at, s.member_registration_closes_at,
		       s.registration_closes_at, s.max_members, s.max_non_members, s.max_waiting_list,
		       s.registration_opens_at::text, s.member_registration_closes_at::text, s.registration_closes_at::text
		FROM mabar_sessions s
		LEFT JOIN membership_periods p ON p.id = s.period_id
		LEFT JOIN venues v ON v.id = s.venue_id
		WHERE s.id = $1`, id).Scan(
		&sess.ID, &sess.Type, &sess.PeriodID, &sess.PeriodName, &sess.VenueName,
		&sess.VenueDescription, &sess.Date, &sess.StartTime, &sess.EndTime,
		&sess.Description, &sess.AdminNote, &opensAt, &memberClosesAt, &closesAt,
		&sess.MaxMembers, &sess.MaxNonMembers, &sess.MaxWaitingList,
		&opensAtStr, &memberClosesAtStr, &closesAtStr,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return httpx.NotFound("Session not found.")
		}
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load session.")
	}

	now := time.Now().UTC()
	sess.ServerTime = now.Format(time.RFC3339)
	sess.RegistrationOpensAt = opensAtStr
	sess.MemberRegistrationClosesAt = memberClosesAtStr
	sess.RegistrationClosesAt = closesAtStr
	sess.Status = computeRegistrationStatus(opensAt, memberClosesAt, closesAt, now)
	isWindowOpen := sess.Status == "MEMBER_ONLY" || sess.Status == "OPEN"
	sess.MemberRegistrationOpen = isWindowOpen && !isMemberDeadlinePassed(memberClosesAt, now)
	sess.NonMemberRegistrationOpen = isWindowOpen

	// Counts
	var memberCount, nonMemberCount, waitingCount int
	_ = s.db.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM attendances
		WHERE session_id = $1 AND is_member = true AND status IN ('LISTED', 'CONFIRMED', 'PRESENT')`, id).Scan(&memberCount)

	_ = s.db.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM attendances
		WHERE session_id = $1 AND is_member = false AND status IN ('LISTED', 'CONFIRMED', 'PRESENT')`, id).Scan(&nonMemberCount)

	_ = s.db.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM attendances
		WHERE session_id = $1 AND status = 'WAITING'`, id).Scan(&waitingCount)

	quotas := computeRegistrationQuotas(
		sess.MaxMembers,
		sess.MaxNonMembers,
		sess.MaxWaitingList,
		memberCount,
		nonMemberCount,
		waitingCount,
		sess.Status,
		memberClosesAt,
		now,
	)

	// Period members
	periodMembers := []RegistrationPeriodMember{}
	if sess.PeriodID != nil {
		rows, err := s.db.Query(c.Context(), `
			SELECT pl.id::text, pl.name, pl.phone, COALESCE(a.status, 'NOT_LISTED'), 
			       m.commitment_fee
			FROM memberships m
			JOIN players pl ON pl.id = m.player_id
			LEFT JOIN attendances a ON a.session_id = $1 AND a.player_id = pl.id
			WHERE m.period_id = $2 AND m.status <> 'WITHDRAWN' AND pl.status <> 'ARCHIVED'
			ORDER BY pl.name ASC`, id, *sess.PeriodID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var pm RegistrationPeriodMember
				if err := rows.Scan(&pm.ID, &pm.Name, &pm.Phone, &pm.AttendanceStatus, &pm.CommitmentFee); err == nil {
					pm.IsRegistered = pm.AttendanceStatus == "LISTED" || pm.AttendanceStatus == "CONFIRMED" || pm.AttendanceStatus == "PRESENT" || pm.AttendanceStatus == "WAITING"
					periodMembers = append(periodMembers, pm)
				}
			}
		}
	}

	// Non-member players (all active players not holding an active commitment-fee membership in this period)
	nonMemberPlayers := []RegistrationNonMemberPlayer{}
	var nmRows pgx.Rows
	var nmErr error
	if sess.PeriodID != nil {
		nmRows, nmErr = s.db.Query(c.Context(), `
			SELECT pl.id::text, pl.name, COALESCE(a.status, 'NOT_LISTED')
			FROM players pl
			LEFT JOIN attendances a ON a.session_id = $1 AND a.player_id = pl.id
			WHERE pl.status <> 'ARCHIVED'
			  AND NOT EXISTS (
			      SELECT 1 FROM memberships m
			      WHERE m.period_id = $2
			        AND m.player_id = pl.id
			        AND m.status <> 'WITHDRAWN'
			        AND m.commitment_fee > 0
			  )
			ORDER BY pl.name ASC`, id, *sess.PeriodID)
	} else {
		nmRows, nmErr = s.db.Query(c.Context(), `
			SELECT pl.id::text, pl.name, COALESCE(a.status, 'NOT_LISTED')
			FROM players pl
			LEFT JOIN attendances a ON a.session_id = $1 AND a.player_id = pl.id
			WHERE pl.status <> 'ARCHIVED'
			ORDER BY pl.name ASC`, id)
	}
	if nmErr == nil {
		defer nmRows.Close()
		for nmRows.Next() {
			var nm RegistrationNonMemberPlayer
			var attStatus string
			if err := nmRows.Scan(&nm.ID, &nm.Name, &attStatus); err == nil {
				nm.IsRegistered = attStatus == "LISTED" || attStatus == "CONFIRMED" || attStatus == "PRESENT" || attStatus == "WAITING"
				nonMemberPlayers = append(nonMemberPlayers, nm)
			}
		}
	}

	// Participants
	participants := RegistrationParticipants{
		Members:     []ParticipantItem{},
		NonMembers:  []ParticipantItem{},
		WaitingList: []ParticipantItem{},
	}

	pRows, err := s.db.Query(c.Context(), `
		SELECT a.id::text, a.player_id::text, pl.name, a.is_member, a.status, a.listed_at::text
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		WHERE a.session_id = $1 AND a.status IN ('LISTED', 'CONFIRMED', 'PRESENT', 'WAITING')
		ORDER BY a.listed_at ASC`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load participants.")
	}
	defer pRows.Close()
	for pRows.Next() {
		var p ParticipantItem
		if err := pRows.Scan(&p.AttendanceID, &p.PlayerID, &p.PlayerName, &p.IsMember, &p.Status, &p.ListedAt); err == nil {
			if p.Status == "WAITING" {
				participants.WaitingList = append(participants.WaitingList, p)
			} else if p.IsMember {
				participants.Members = append(participants.Members, p)
			} else {
				participants.NonMembers = append(participants.NonMembers, p)
			}
		}
	}
	if err := pRows.Err(); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not read participants.")
	}

	return httpx.OK(c, http.StatusOK, RegistrationResponse{
		Session:          sess,
		Quotas:           quotas,
		PeriodMembers:    periodMembers,
		NonMemberPlayers: nonMemberPlayers,
		Participants:     participants,
	})
}

type openPlayRegisterInput struct {
	ParticipantType string  `json:"participant_type"` // "MEMBER" or "NON_MEMBER"
	PlayerID        *string `json:"player_id"`
	PlayerName      *string `json:"player_name"`
	Phone           *string `json:"phone"`
}

type openPlayRegisterResult struct {
	Status   string `json:"status"` // "PRESENT" or "WAITING"
	IsMember bool   `json:"is_member"`
	Message  string `json:"message"`
}

func (s *Service) RegisterOpenPlay(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}

	var in openPlayRegisterInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid request body.")
	}

	in.ParticipantType = strings.ToUpper(strings.TrimSpace(in.ParticipantType))
	if in.ParticipantType != "MEMBER" && in.ParticipantType != "NON_MEMBER" {
		return httpx.Unprocessable("Participant type must be MEMBER or NON_MEMBER.")
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not begin transaction.")
	}
	defer tx.Rollback(c.Context())

	var periodID *string
	var opensAt, memberClosesAt, closesAt *time.Time
	var maxMembers, maxNonMembers, maxWaitingList int

	err = tx.QueryRow(c.Context(), `
		SELECT period_id::text, registration_opens_at, member_registration_closes_at,
		       registration_closes_at, max_members, max_non_members, max_waiting_list
		FROM mabar_sessions
		WHERE id = $1 FOR UPDATE`, id).Scan(
		&periodID, &opensAt, &memberClosesAt, &closesAt,
		&maxMembers, &maxNonMembers, &maxWaitingList,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return httpx.NotFound("Session not found.")
		}
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load session.")
	}

	now := time.Now().UTC()
	status := computeRegistrationStatus(opensAt, memberClosesAt, closesAt, now)
	switch status {
	case "NOT_CONFIGURED":
		return httpx.Unprocessable("Pendaftaran belum dibuka untuk sesi ini.")
	case "UPCOMING":
		return httpx.Unprocessable("Pendaftaran belum dibuka.")
	case "CLOSED":
		return httpx.Unprocessable("Pendaftaran sudah ditutup.")
	}
	if in.ParticipantType == "MEMBER" && isMemberDeadlinePassed(memberClosesAt, now) {
		return httpx.Unprocessable("Batas waktu pendaftaran member telah berakhir. Sisa slot sekarang dibuka untuk non-member.")
	}

	var targetPlayerID string
	var isMember bool

	if in.ParticipantType == "MEMBER" {
		if in.PlayerID == nil || strings.TrimSpace(*in.PlayerID) == "" {
			return httpx.Unprocessable("Pilih pemain member.")
		}
		targetPlayerID = strings.TrimSpace(*in.PlayerID)
		if periodID == nil {
			return httpx.Unprocessable("Sesi ini bukan sesi periode.")
		}

		// Verify member is in period
		var isPeriodMember bool
		_ = tx.QueryRow(c.Context(), `
			SELECT EXISTS(
				SELECT 1 FROM memberships
				WHERE period_id = $1 AND player_id = $2 AND status <> 'WITHDRAWN'
			)`, *periodID, targetPlayerID).Scan(&isPeriodMember)
		if !isPeriodMember {
			return httpx.Unprocessable("Pemain tidak terdaftar sebagai member periode ini.")
		}
		isMember = true
	} else {
		isMember = false
		if in.PlayerID != nil && strings.TrimSpace(*in.PlayerID) != "" {
			targetPlayerID = strings.TrimSpace(*in.PlayerID)
			if _, err := uuid.Parse(targetPlayerID); err != nil {
				return httpx.Unprocessable("ID pemain tidak valid.")
			}
			var playerExists bool
			_ = tx.QueryRow(c.Context(), `
				SELECT EXISTS(SELECT 1 FROM players WHERE id = $1 AND status <> 'ARCHIVED')`,
				targetPlayerID).Scan(&playerExists)
			if !playerExists {
				return httpx.Unprocessable("Pemain tidak ditemukan.")
			}
		} else {
			if in.PlayerName == nil || strings.TrimSpace(*in.PlayerName) == "" {
				return httpx.Unprocessable("Nama pemain non-member harus diisi.")
			}
			name := strings.TrimSpace(*in.PlayerName)
			phone := ""
			if in.Phone != nil {
				phone = strings.TrimSpace(*in.Phone)
			}

			// Find existing active player or create one
			err = tx.QueryRow(c.Context(), `
				SELECT id::text FROM players
				WHERE LOWER(name) = LOWER($1) AND status <> 'ARCHIVED'
				LIMIT 1`, name).Scan(&targetPlayerID)
			if err == pgx.ErrNoRows {
				err = tx.QueryRow(c.Context(), `
					INSERT INTO players (id, name, phone, status)
					VALUES (gen_random_uuid(), $1, NULLIF($2, ''), 'ACTIVE')
					RETURNING id::text`, name, phone).Scan(&targetPlayerID)
				if err != nil {
					return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create player.")
				}
			} else if err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not lookup player.")
			}
		}

		if periodID != nil {
			var isPaidPeriodMember bool
			_ = tx.QueryRow(c.Context(), `
				SELECT EXISTS(
					SELECT 1 FROM memberships
					WHERE period_id = $1 AND player_id = $2 AND status <> 'WITHDRAWN' AND commitment_fee > 0
				)`, *periodID, targetPlayerID).Scan(&isPaidPeriodMember)
			if isPaidPeriodMember {
				return httpx.Unprocessable("Pemain terdaftar sebagai member periode ini. Silakan daftar melalui pilihan Member.")
			}
		}
	}

	// Check if already registered or in waiting list
	var currentStatus string
	var existingAttendanceID string
	err = tx.QueryRow(c.Context(), `
		SELECT id::text, status FROM attendances
		WHERE session_id = $1 AND player_id = $2`, id, targetPlayerID).Scan(&existingAttendanceID, &currentStatus)
	if err == nil {
		if currentStatus == "LISTED" || currentStatus == "CONFIRMED" || currentStatus == "PRESENT" {
			return httpx.Conflict("ALREADY_REGISTERED", "Pemain sudah terdaftar dalam sesi ini.")
		}
		if currentStatus == "WAITING" {
			return httpx.Conflict("ALREADY_WAITING", "Pemain sudah terdaftar di waiting list.")
		}
	}

	// Calculate slot availability
	var memberCount, nonMemberCount, waitingCount int
	_ = tx.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM attendances
		WHERE session_id = $1 AND is_member = true AND status IN ('LISTED', 'CONFIRMED', 'PRESENT')`, id).Scan(&memberCount)
	_ = tx.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM attendances
		WHERE session_id = $1 AND is_member = false AND status IN ('LISTED', 'CONFIRMED', 'PRESENT')`, id).Scan(&nonMemberCount)
	_ = tx.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM attendances
		WHERE session_id = $1 AND status = 'WAITING'`, id).Scan(&waitingCount)

	quotas := computeRegistrationQuotas(
		maxMembers,
		maxNonMembers,
		maxWaitingList,
		memberCount,
		nonMemberCount,
		waitingCount,
		status,
		memberClosesAt,
		now,
	)

	var assignedStatus string
	var responseMsg string

	if isMember {
		if quotas.AvailableMemberSlots > 0 {
			assignedStatus = "PRESENT"
			responseMsg = "Pendaftaran berhasil sebagai Member."
		} else {
			// Member quota full -> check waiting list
			if quotas.AvailableWaitingSlots > 0 {
				assignedStatus = "WAITING"
				responseMsg = fmt.Sprintf("Slot member sudah penuh (%d). Anda berhasil masuk ke Waiting List.", maxMembers)
			} else {
				return httpx.Unprocessable("Slot pendaftaran dan waiting list sudah penuh.")
			}
		}
	} else {
		// Non-member uses max_non_members before member cutoff, plus any unfilled member slots after cutoff
		if quotas.AvailableNonMemberSlots > 0 {
			assignedStatus = "PRESENT"
			responseMsg = "Pendaftaran berhasil sebagai Non-Member."
		} else {
			if quotas.AvailableWaitingSlots > 0 {
				assignedStatus = "WAITING"
				responseMsg = fmt.Sprintf("Slot non-member sudah penuh (%d). Anda berhasil masuk ke Waiting List.", quotas.EffectiveMaxNonMembers)
			} else {
				return httpx.Unprocessable("Slot pendaftaran dan waiting list sudah penuh.")
			}
		}
	}

	// Update or insert attendance
	if existingAttendanceID != "" {
		_, err = tx.Exec(c.Context(), `
			UPDATE attendances SET
			  status = $1, is_member = $2, listed_at = now(), cancelled_at = NULL
			WHERE id = $3`, assignedStatus, isMember, existingAttendanceID)
	} else {
		_, err = tx.Exec(c.Context(), `
			INSERT INTO attendances (id, session_id, player_id, status, is_member, listed_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, now())`,
			id, targetPlayerID, assignedStatus, isMember)
	}
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not register attendance.")
	}

	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not commit registration.")
	}

	return httpx.OK(c, http.StatusOK, openPlayRegisterResult{
		Status:   assignedStatus,
		IsMember: isMember,
		Message:  responseMsg,
	})
}

type submitCancellationInput struct {
	PlayerID string `json:"player_id"`
	Reason   string `json:"reason"`
}

func (s *Service) SubmitCancellation(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}

	var in submitCancellationInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid request body.")
	}

	in.PlayerID = strings.TrimSpace(in.PlayerID)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.PlayerID == "" {
		return httpx.Unprocessable("Player ID is required.")
	}
	if in.Reason == "" {
		return httpx.Unprocessable("Alasan pembatalan harus diisi.")
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not begin transaction.")
	}
	defer tx.Rollback(c.Context())

	var attendanceID, currentStatus string
	var isMember bool
	err = tx.QueryRow(c.Context(), `
		SELECT id::text, status, is_member FROM attendances
		WHERE session_id = $1 AND player_id = $2`, id, in.PlayerID).Scan(&attendanceID, &currentStatus, &isMember)
	if err != nil {
		if err == pgx.ErrNoRows {
			return httpx.NotFound("Pemain tidak terdaftar dalam sesi ini.")
		}
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not find attendance.")
	}

	if currentStatus != "LISTED" && currentStatus != "CONFIRMED" && currentStatus != "PRESENT" && currentStatus != "WAITING" {
		return httpx.Unprocessable("Pemain tidak dalam status terdaftar aktif atau waiting list.")
	}

	// If player is WAITING, cancellation can be approved immediately since it doesn't leave an active slot vacant
	if currentStatus == "WAITING" {
		newStatus := "CANCELLED"
		if isMember {
			newStatus = "NOT_LISTED"
		}
		_, err = tx.Exec(c.Context(), `
			UPDATE attendances SET status = $1, cancelled_at = now()
			WHERE id = $2`, newStatus, attendanceID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not cancel waiting reservation.")
		}

		_, _ = tx.Exec(c.Context(), `
			INSERT INTO mabar_cancellations (id, session_id, player_id, reason, status, admin_notes, reviewed_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 'APPROVED', 'Auto-approved waiting list cancellation', now())`,
			id, in.PlayerID, in.Reason)

		if err := tx.Commit(c.Context()); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not commit.")
		}
		return httpx.OK(c, http.StatusOK, fiber.Map{
			"message": "Pembatalan antrean waiting list berhasil.",
		})
	}

	// For active players (LISTED, CONFIRMED, PRESENT), check for pending cancellation
	var pendingExists bool
	_ = tx.QueryRow(c.Context(), `
		SELECT EXISTS(
			SELECT 1 FROM mabar_cancellations
			WHERE session_id = $1 AND player_id = $2 AND status = 'PENDING'
		)`, id, in.PlayerID).Scan(&pendingExists)
	if pendingExists {
		return httpx.Conflict("ALREADY_PENDING", "Permintaan pembatalan sudah dikirim dan sedang menunggu persetujuan admin.")
	}

	_, err = tx.Exec(c.Context(), `
		INSERT INTO mabar_cancellations (id, session_id, player_id, reason, status)
		VALUES (gen_random_uuid(), $1, $2, $3, 'PENDING')`,
		id, in.PlayerID, in.Reason)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not submit cancellation.")
	}

	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not commit.")
	}

	return httpx.OK(c, http.StatusOK, fiber.Map{
		"message": "Pengajuan pembatalan berhasil dikirim. Menunggu verifikasi admin.",
	})
}

type CancellationItem struct {
	ID               string  `json:"id"`
	SessionID        string  `json:"session_id"`
	PlayerID         string  `json:"player_id"`
	PlayerName       string  `json:"player_name"`
	Phone            *string `json:"phone"`
	IsMember         bool    `json:"is_member"`
	AttendanceStatus string  `json:"attendance_status"`
	Reason           string  `json:"reason"`
	Status           string  `json:"status"` // PENDING, APPROVED, REJECTED
	AdminNotes       *string `json:"admin_notes"`
	ReviewedAt       *string `json:"reviewed_at"`
	CreatedAt        string  `json:"created_at"`
}

func (s *Service) ListCancellations(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}

	rows, err := s.db.Query(c.Context(), `
		SELECT c.id::text, c.session_id::text, c.player_id::text, pl.name, pl.phone,
		       COALESCE(a.is_member, false), COALESCE(a.status, 'UNKNOWN'),
		       c.reason, c.status, c.admin_notes, c.reviewed_at::text, c.created_at::text
		FROM mabar_cancellations c
		JOIN players pl ON pl.id = c.player_id
		LEFT JOIN attendances a ON a.session_id = c.session_id AND a.player_id = c.player_id
		WHERE c.session_id = $1
		ORDER BY c.created_at DESC`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load cancellations.")
	}
	defer rows.Close()

	list := []CancellationItem{}
	for rows.Next() {
		var item CancellationItem
		if err := rows.Scan(&item.ID, &item.SessionID, &item.PlayerID, &item.PlayerName, &item.Phone,
			&item.IsMember, &item.AttendanceStatus, &item.Reason, &item.Status,
			&item.AdminNotes, &item.ReviewedAt, &item.CreatedAt); err == nil {
			list = append(list, item)
		}
	}

	return httpx.OK(c, http.StatusOK, list)
}

func (s *Service) ApproveCancellation(c *fiber.Ctx) error {
	id := c.Params("id")
	cancellationID := c.Params("cancellationId")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}
	if _, err := uuid.Parse(cancellationID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid cancellation ID.")
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not begin transaction.")
	}
	defer tx.Rollback(c.Context())

	var playerID string
	var status string
	err = tx.QueryRow(c.Context(), `
		SELECT player_id::text, status FROM mabar_cancellations
		WHERE id = $1 AND session_id = $2 FOR UPDATE`, cancellationID, id).Scan(&playerID, &status)
	if err != nil {
		if err == pgx.ErrNoRows {
			return httpx.NotFound("Cancellation request not found.")
		}
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load cancellation.")
	}

	if status != "PENDING" {
		return httpx.Conflict("ALREADY_REVIEWED", "Permintaan pembatalan sudah pernah diproses.")
	}

	// Update cancellation to APPROVED
	_, err = tx.Exec(c.Context(), `
		UPDATE mabar_cancellations
		SET status = 'APPROVED', reviewed_at = now(), updated_at = now()
		WHERE id = $1`, cancellationID)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update cancellation.")
	}

	// Update attendance of cancelling player
	var isMember bool
	var attID string
	err = tx.QueryRow(c.Context(), `
		SELECT id::text, is_member FROM attendances
		WHERE session_id = $1 AND player_id = $2`, id, playerID).Scan(&attID, &isMember)
	if err == nil {
		newStatus := "CANCELLED"
		if isMember {
			newStatus = "NOT_LISTED"
		}
		_, _ = tx.Exec(c.Context(), `
			UPDATE attendances SET status = $1, cancelled_at = now()
			WHERE id = $2`, newStatus, attID)
	}

	// AUTO-FILL FROM WAITING LIST
	// Find earliest player in WAITING status for this session
	var waitingAttID, waitingPlayerID, waitingPlayerName string
	err = tx.QueryRow(c.Context(), `
		SELECT a.id::text, a.player_id::text, pl.name
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		WHERE a.session_id = $1 AND a.status = 'WAITING'
		ORDER BY a.listed_at ASC
		LIMIT 1
		FOR UPDATE OF a`, id).Scan(&waitingAttID, &waitingPlayerID, &waitingPlayerName)

	var promotedPlayer *string
	if err == nil && waitingAttID != "" {
		// Promote this player to PRESENT
		_, err = tx.Exec(c.Context(), `
			UPDATE attendances SET status = 'PRESENT', listed_at = now()
			WHERE id = $1`, waitingAttID)
		if err == nil {
			promotedPlayer = &waitingPlayerName
		}
	}

	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not commit approval.")
	}

	resp := fiber.Map{
		"message": "Pembatalan disetujui.",
	}
	if promotedPlayer != nil {
		resp["auto_filled_player"] = *promotedPlayer
		resp["message"] = "Pembatalan disetujui. Slot kosong otomatis diisi oleh: " + *promotedPlayer
	}

	return httpx.OK(c, http.StatusOK, resp)
}

type rejectCancellationInput struct {
	AdminNotes *string `json:"admin_notes"`
}

func (s *Service) RejectCancellation(c *fiber.Ctx) error {
	id := c.Params("id")
	cancellationID := c.Params("cancellationId")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}
	if _, err := uuid.Parse(cancellationID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid cancellation ID.")
	}

	var in rejectCancellationInput
	_ = httpx.Decode(c, &in)

	tag, err := s.db.Exec(c.Context(), `
		UPDATE mabar_cancellations
		SET status = 'REJECTED', admin_notes = $3, reviewed_at = now(), updated_at = now()
		WHERE id = $1 AND session_id = $2 AND status = 'PENDING'`,
		cancellationID, id, in.AdminNotes)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not reject cancellation.")
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("Cancellation request not found or already reviewed.")
	}

	return httpx.OK(c, http.StatusOK, fiber.Map{
		"message": "Pembatalan ditolak.",
	})
}
