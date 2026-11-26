package openplay

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService(db *pgxpool.Pool) *Service {
	return &Service{db: db}
}

// CreateSession creates a new open play session
func (s *Service) CreateSession(c *fiber.Ctx) error {
	var req CreateSessionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	if err := validateCreateSessionRequest(&req); err != nil {
		return httpx.WriteAppError(c, err)
	}

	sessionID := uuid.New().String()
	userID := c.Locals("user_id").(string)

	query := `
		INSERT INTO open_play_sessions (
			id, name, description, schedule_date, start_time, end_time,
			venue_id, max_members, max_non_members, max_waiting_list,
			cancellation_window_hours, min_non_member_fee, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, name, description, schedule_date, start_time, end_time,
			venue_id, max_members, max_non_members, max_waiting_list,
			cancellation_window_hours, min_non_member_fee, status, created_by,
			created_at, updated_at
	`

	var session OpenPlaySession
	err := s.db.QueryRow(
		context.Background(),
		query,
		sessionID, req.Name, req.Description, req.ScheduleDate, req.StartTime, req.EndTime,
		req.VenueID, req.MaxMembers, req.MaxNonMembers, req.MaxWaitingList,
		req.CancellationWindowHours, req.MinNonMemberFee, userID,
	).Scan(
		&session.ID, &session.Name, &session.Description, &session.ScheduleDate,
		&session.StartTime, &session.EndTime, &session.VenueID, &session.MaxMembers,
		&session.MaxNonMembers, &session.MaxWaitingList, &session.CancellationWindowHours,
		&session.MinNonMemberFee, &session.Status, &session.CreatedBy, &session.CreatedAt,
		&session.UpdatedAt,
	)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to create session"))
	}

	return c.Status(http.StatusCreated).JSON(session)
}

// ListSessions lists all open play sessions with filters
func (s *Service) ListSessions(c *fiber.Ctx) error {
	status := c.Query("status", "OPEN")
	fromDate := c.Query("from_date")
	toDate := c.Query("to_date")

	query := `
		SELECT id, name, description, schedule_date, start_time, end_time,
			venue_id, max_members, max_non_members, max_waiting_list,
			cancellation_window_hours, min_non_member_fee, status, created_by,
			created_at, updated_at
		FROM open_play_sessions
		WHERE 1=1
	`

	args := []interface{}{}
	argCount := 1

	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", argCount)
		args = append(args, status)
		argCount++
	}

	if fromDate != "" {
		query += fmt.Sprintf(" AND schedule_date >= $%d", argCount)
		args = append(args, fromDate)
		argCount++
	}

	if toDate != "" {
		query += fmt.Sprintf(" AND schedule_date <= $%d", argCount)
		args = append(args, toDate)
		argCount++
	}

	query += " ORDER BY schedule_date DESC, start_time DESC"

	rows, err := s.db.Query(context.Background(), query, args...)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch sessions"))
	}
	defer rows.Close()

	var sessions []*OpenPlaySession
	for rows.Next() {
		var session OpenPlaySession
		err := rows.Scan(
			&session.ID, &session.Name, &session.Description, &session.ScheduleDate,
			&session.StartTime, &session.EndTime, &session.VenueID, &session.MaxMembers,
			&session.MaxNonMembers, &session.MaxWaitingList, &session.CancellationWindowHours,
			&session.MinNonMemberFee, &session.Status, &session.CreatedBy, &session.CreatedAt,
			&session.UpdatedAt,
		)
		if err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to scan session"))
		}
		sessions = append(sessions, &session)
	}

	if sessions == nil {
		sessions = []*OpenPlaySession{}
	}

	return c.JSON(sessions)
}

// GetSession retrieves a specific open play session with participant counts
func (s *Service) GetSession(c *fiber.Ctx) error {
	sessionID := c.Params("id")

	sessionQuery := `
		SELECT id, name, description, schedule_date, start_time, end_time,
			venue_id, max_members, max_non_members, max_waiting_list,
			cancellation_window_hours, min_non_member_fee, status, created_by,
			created_at, updated_at
		FROM open_play_sessions
		WHERE id = $1
	`

	var session OpenPlaySession
	err := s.db.QueryRow(context.Background(), sessionQuery, sessionID).Scan(
		&session.ID, &session.Name, &session.Description, &session.ScheduleDate,
		&session.StartTime, &session.EndTime, &session.VenueID, &session.MaxMembers,
		&session.MaxNonMembers, &session.MaxWaitingList, &session.CancellationWindowHours,
		&session.MinNonMemberFee, &session.Status, &session.CreatedBy, &session.CreatedAt,
		&session.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return httpx.WriteAppError(c, httpx.NotFound("Session not found"))
	}
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch session"))
	}

	// Get participant counts
	countQuery := `
		SELECT
			COALESCE(SUM(CASE WHEN participant_type = 'MEMBER' AND participant_status = 'REGISTERED' THEN 1 ELSE 0 END), 0) as member_count,
			COALESCE(SUM(CASE WHEN participant_type = 'NON_MEMBER' AND participant_status = 'REGISTERED' THEN 1 ELSE 0 END), 0) as non_member_count,
			COALESCE(SUM(CASE WHEN participant_status = 'WAITING' THEN 1 ELSE 0 END), 0) as waiting_count
		FROM open_play_participants
		WHERE session_id = $1
	`

	err = s.db.QueryRow(context.Background(), countQuery, sessionID).Scan(
		&session.RegisteredMemberCount,
		&session.RegisteredNonMemberCount,
		&session.WaitingListCount,
	)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch participant counts"))
	}

	return c.JSON(session)
}

// JoinSession adds a player to an open play session
func (s *Service) JoinSession(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	var req JoinSessionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	if req.ParticipantType != ParticipantTypeMember && req.ParticipantType != ParticipantTypeNonMember {
		return httpx.WriteAppError(c, httpx.Unprocessable("Invalid participant_type"))
	}

	// Start transaction
	tx, err := s.db.Begin(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to start transaction"))
	}
	defer tx.Rollback(context.Background())

	// Check session exists and is open
	var maxMembers, maxNonMembers, maxWaiting int
	var status string
	sessionQuery := `SELECT max_members, max_non_members, max_waiting_list, status FROM open_play_sessions WHERE id = $1`
	err = tx.QueryRow(context.Background(), sessionQuery, sessionID).Scan(&maxMembers, &maxNonMembers, &maxWaiting, &status)
	if err == pgx.ErrNoRows {
		return httpx.WriteAppError(c, httpx.NotFound("Session not found"))
	}
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch session"))
	}

	if status != "OPEN" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Session is not open for registration"))
	}

	// Check if player already joined
	var existingID string
	checkQuery := `SELECT id FROM open_play_participants WHERE session_id = $1 AND player_id = $2 AND participant_status != 'CANCELLED'`
	err = tx.QueryRow(context.Background(), checkQuery, sessionID, req.PlayerID).Scan(&existingID)
	if err == nil {
		return httpx.WriteAppError(c, httpx.Conflict("ALREADY_JOINED", "Player already joined this session"))
	}
	if err != pgx.ErrNoRows {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to check existing participation"))
	}

	// Get current counts
	countQuery := `
		SELECT
			COALESCE(SUM(CASE WHEN participant_type = 'MEMBER' AND participant_status = 'REGISTERED' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN participant_type = 'NON_MEMBER' AND participant_status = 'REGISTERED' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN participant_status = 'WAITING' THEN 1 ELSE 0 END), 0)
		FROM open_play_participants
		WHERE session_id = $1
	`
	var memberCount, nonMemberCount, waitingCount int
	err = tx.QueryRow(context.Background(), countQuery, sessionID).Scan(&memberCount, &nonMemberCount, &waitingCount)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch participant counts"))
	}

	// Determine participant status
	var participantStatus ParticipantStatus
	if req.ParticipantType == ParticipantTypeMember {
		if memberCount < maxMembers {
			participantStatus = ParticipantStatusRegistered
		} else if waitingCount < maxWaiting {
			participantStatus = ParticipantStatusWaiting
		} else {
			return httpx.WriteAppError(c, httpx.Unprocessable("No slots available (members full and waiting list full)"))
		}
	} else {
		if nonMemberCount < maxNonMembers {
			participantStatus = ParticipantStatusRegistered
		} else if waitingCount < maxWaiting {
			participantStatus = ParticipantStatusWaiting
		} else {
			return httpx.WriteAppError(c, httpx.Unprocessable("No slots available (non-members full and waiting list full)"))
		}
	}

	// Insert participant
	participantID := uuid.New().String()
	insertQuery := `
		INSERT INTO open_play_participants (
			id, session_id, player_id, participant_type, participant_status
		) VALUES ($1, $2, $3, $4, $5)
	`
	_, err = tx.Exec(context.Background(), insertQuery, participantID, sessionID, req.PlayerID, req.ParticipantType, participantStatus)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to add participant"))
	}

	err = tx.Commit(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to commit transaction"))
	}

	return c.Status(http.StatusCreated).JSON(fiber.Map{
		"message": "Successfully joined session",
		"status":  participantStatus,
	})
}

// GetParticipants retrieves all participants for a session
func (s *Service) GetParticipants(c *fiber.Ctx) error {
	sessionID := c.Params("id")

	query := `
		SELECT
			opp.id, opp.session_id, opp.player_id, p.name, opp.participant_type,
			opp.participant_status, opp.joined_at, opp.filled_from_waiting_at
		FROM open_play_participants opp
		JOIN players p ON opp.player_id = p.id
		WHERE opp.session_id = $1 AND opp.participant_status != 'CANCELLED'
		ORDER BY
			CASE opp.participant_status
				WHEN 'REGISTERED' THEN 1
				WHEN 'AUTO_FILLED' THEN 2
				WHEN 'WAITING' THEN 3
				ELSE 4
			END,
			opp.joined_at ASC
	`

	rows, err := s.db.Query(context.Background(), query, sessionID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch participants"))
	}
	defer rows.Close()

	var members, nonMembers, waiting []*Participant

	for rows.Next() {
		var p Participant
		err := rows.Scan(
			&p.ID, &p.SessionID, &p.PlayerID, &p.PlayerName, &p.ParticipantType,
			&p.ParticipantStatus, &p.JoinedAt, &p.FilledFromWaitingAt,
		)
		if err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to scan participant"))
		}

		if p.ParticipantStatus == ParticipantStatusWaiting {
			waiting = append(waiting, &p)
		} else if p.ParticipantType == ParticipantTypeMember {
			members = append(members, &p)
		} else {
			nonMembers = append(nonMembers, &p)
		}
	}

	resp := ParticipantListResponse{
		Members:     members,
		NonMembers:  nonMembers,
		WaitingList: waiting,
	}

	if resp.Members == nil {
		resp.Members = []*Participant{}
	}
	if resp.NonMembers == nil {
		resp.NonMembers = []*Participant{}
	}
	if resp.WaitingList == nil {
		resp.WaitingList = []*Participant{}
	}

	return c.JSON(resp)
}

// CancelParticipation creates a cancellation request
func (s *Service) CancelParticipation(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	participantID := c.Params("participant_id")

	var req CancelParticipationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	if req.Reason == "" {
		return httpx.WriteAppError(c, httpx.Unprocessable("Reason is required"))
	}

	tx, err := s.db.Begin(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to start transaction"))
	}
	defer tx.Rollback(context.Background())

	// Verify participant exists
	var playerID string
	checkQuery := `
		SELECT player_id FROM open_play_participants
		WHERE id = $1 AND session_id = $2 AND participant_status != 'CANCELLED'
	`
	err = tx.QueryRow(context.Background(), checkQuery, participantID, sessionID).Scan(&playerID)
	if err == pgx.ErrNoRows {
		return httpx.WriteAppError(c, httpx.NotFound("Participant not found or already cancelled"))
	}
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to verify participant"))
	}

	// Create cancellation request
	cancellationID := uuid.New().String()
	insertQuery := `
		INSERT INTO open_play_cancellations (
			id, participant_id, session_id, reason
		) VALUES ($1, $2, $3, $4)
		RETURNING id, participant_id, session_id, reason, request_status, requested_at, created_at
	`

	var cancellation CancellationRequest
	err = tx.QueryRow(context.Background(), insertQuery, cancellationID, participantID, sessionID, req.Reason).Scan(
		&cancellation.ID, &cancellation.ParticipantID, &cancellation.SessionID, &cancellation.Reason,
		&cancellation.Status, &cancellation.RequestedAt, &cancellation.CreatedAt,
	)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to create cancellation request"))
	}

	err = tx.Commit(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to commit transaction"))
	}

	return c.Status(http.StatusCreated).JSON(cancellation)
}

// Validation helpers

func validateCreateSessionRequest(req *CreateSessionRequest) error {
	if req.Name == "" {
		return httpx.Unprocessable("Name is required")
	}
	if req.ScheduleDate == "" {
		return httpx.Unprocessable("Schedule date is required")
	}
	if req.StartTime == "" {
		return httpx.Unprocessable("Start time is required")
	}
	if req.MaxMembers <= 0 {
		return httpx.Unprocessable("Max members must be greater than 0")
	}
	if req.MaxNonMembers <= 0 {
		return httpx.Unprocessable("Max non-members must be greater than 0")
	}
	if req.MaxWaitingList < 0 {
		return httpx.Unprocessable("Max waiting list cannot be negative")
	}
	if req.CancellationWindowHours <= 0 {
		return httpx.Unprocessable("Cancellation window hours must be greater than 0")
	}
	return nil
}
