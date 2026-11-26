package openplay

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"

	"github.com/pb-kecebong/backend/internal/httpx"
)

// UpdateSession updates an open play session (admin only)
func (s *Service) UpdateSession(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	var req UpdateSessionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	if req.Name == nil && req.Status == nil && req.MaxMembers == nil && req.AdminNote == nil {
		return httpx.WriteAppError(c, httpx.Unprocessable("No fields to update"))
	}

	query := `UPDATE open_play_sessions SET `
	args := []interface{}{}
	argCount := 1

	if req.Name != nil {
		query += fmt.Sprintf("name = $%d, ", argCount)
		args = append(args, *req.Name)
		argCount++
	}

	if req.Description != nil {
		query += fmt.Sprintf("description = $%d, ", argCount)
		args = append(args, *req.Description)
		argCount++
	}

	if req.AdminNote != nil {
		query += fmt.Sprintf("admin_note = $%d, ", argCount)
		args = append(args, *req.AdminNote)
		argCount++
	}

	if req.Status != nil {
		query += fmt.Sprintf("status = $%d, ", argCount)
		args = append(args, *req.Status)
		argCount++
	}

	if req.MaxMembers != nil && *req.MaxMembers > 0 {
		query += fmt.Sprintf("max_members = $%d, ", argCount)
		args = append(args, *req.MaxMembers)
		argCount++
	}

	if req.MaxNonMembers != nil && *req.MaxNonMembers > 0 {
		query += fmt.Sprintf("max_non_members = $%d, ", argCount)
		args = append(args, *req.MaxNonMembers)
		argCount++
	}

	if req.MaxWaitingList != nil && *req.MaxWaitingList >= 0 {
		query += fmt.Sprintf("max_waiting_list = $%d, ", argCount)
		args = append(args, *req.MaxWaitingList)
		argCount++
	}

	query += fmt.Sprintf("updated_at = NOW() WHERE id = $%d", argCount)
	args = append(args, sessionID)

	_, err := s.db.Exec(context.Background(), query, args...)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to update session"))
	}

	return c.JSON(fiber.Map{"message": "Session updated successfully"})
}

// DeleteSession marks a session as cancelled (soft delete)
func (s *Service) DeleteSession(c *fiber.Ctx) error {
	sessionID := c.Params("id")

	query := `UPDATE open_play_sessions SET status = 'CANCELLED', updated_at = NOW() WHERE id = $1`
	result, err := s.db.Exec(context.Background(), query, sessionID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to delete session"))
	}

	if result.RowsAffected() == 0 {
		return httpx.WriteAppError(c, httpx.NotFound("Session not found"))
	}

	return c.JSON(fiber.Map{"message": "Session deleted successfully"})
}

// RemoveParticipant removes a participant from a session
func (s *Service) RemoveParticipant(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	participantID := c.Params("participant_id")

	tx, err := s.db.Begin(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to start transaction"))
	}
	defer tx.Rollback(context.Background())

	// Mark participant as cancelled
	query := `
		UPDATE open_play_participants
		SET participant_status = 'CANCELLED', updated_at = NOW()
		WHERE id = $1 AND session_id = $2
	`
	result, err := tx.Exec(context.Background(), query, participantID, sessionID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to remove participant"))
	}

	if result.RowsAffected() == 0 {
		return httpx.WriteAppError(c, httpx.NotFound("Participant not found"))
	}

	// Auto-fill from waiting list
	err = autoFillFromWaitingList(tx, sessionID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to auto-fill waiting list"))
	}

	err = tx.Commit(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to commit transaction"))
	}

	return c.JSON(fiber.Map{"message": "Participant removed successfully"})
}

// GetPendingCancellations retrieves all pending cancellation requests
func (s *Service) GetPendingCancellations(c *fiber.Ctx) error {
	query := `
		SELECT
			opc.id, opc.participant_id, opc.session_id, opc.reason,
			opc.request_status, opc.admin_notes, opc.requested_at,
			opc.created_at, ops.name as session_name, p.name as player_name
		FROM open_play_cancellations opc
		JOIN open_play_sessions ops ON opc.session_id = ops.id
		JOIN open_play_participants opp ON opc.participant_id = opp.id
		JOIN players p ON opp.player_id = p.id
		WHERE opc.request_status = 'PENDING'
		ORDER BY opc.requested_at ASC
	`

	rows, err := s.db.Query(context.Background(), query)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch pending cancellations"))
	}
	defer rows.Close()

	var cancellations []*CancellationRequestDetail

	for rows.Next() {
		var cr CancellationRequestDetail
		err := rows.Scan(
			&cr.ID, &cr.ParticipantID, &cr.SessionID, &cr.Reason,
			&cr.Status, &cr.AdminNotes, &cr.RequestedAt, &cr.CreatedAt,
			&cr.SessionName, &cr.PlayerName,
		)
		if err != nil {
			return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to scan cancellation"))
		}
		cancellations = append(cancellations, &cr)
	}

	if cancellations == nil {
		cancellations = []*CancellationRequestDetail{}
	}

	return c.JSON(cancellations)
}

// ApproveCancellation approves a pending cancellation request
func (s *Service) ApproveCancellation(c *fiber.Ctx) error {
	cancellationID := c.Params("id")
	var req ApproveCancellationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	tx, err := s.db.Begin(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to start transaction"))
	}
	defer tx.Rollback(context.Background())

	// Get cancellation and participant info
	var participantID, sessionID string
	getCancelQuery := `
		SELECT participant_id, session_id FROM open_play_cancellations
		WHERE id = $1 AND request_status = 'PENDING'
	`
	err = tx.QueryRow(context.Background(), getCancelQuery, cancellationID).Scan(&participantID, &sessionID)
	if err == pgx.ErrNoRows {
		return httpx.WriteAppError(c, httpx.NotFound("Cancellation request not found or already processed"))
	}
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch cancellation"))
	}

	// Update cancellation status
	updateCancelQuery := `
		UPDATE open_play_cancellations
		SET request_status = 'APPROVED', admin_notes = $1, updated_at = NOW()
		WHERE id = $2
	`
	_, err = tx.Exec(context.Background(), updateCancelQuery, req.AdminNotes, cancellationID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to update cancellation status"))
	}

	// Mark participant as cancelled
	markCancelQuery := `
		UPDATE open_play_participants
		SET participant_status = 'CANCELLED', updated_at = NOW()
		WHERE id = $1
	`
	_, err = tx.Exec(context.Background(), markCancelQuery, participantID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to mark participant as cancelled"))
	}

	// Auto-fill from waiting list
	err = autoFillFromWaitingList(tx, sessionID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to auto-fill waiting list"))
	}

	err = tx.Commit(context.Background())
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to commit transaction"))
	}

	return c.JSON(fiber.Map{"message": "Cancellation approved successfully"})
}

// RejectCancellation rejects a pending cancellation request
func (s *Service) RejectCancellation(c *fiber.Ctx) error {
	cancellationID := c.Params("id")
	var req RejectCancellationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	query := `
		UPDATE open_play_cancellations
		SET request_status = 'REJECTED', admin_notes = $1, updated_at = NOW()
		WHERE id = $2 AND request_status = 'PENDING'
	`
	result, err := s.db.Exec(context.Background(), query, req.AdminNotes, cancellationID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to reject cancellation"))
	}

	if result.RowsAffected() == 0 {
		return httpx.WriteAppError(c, httpx.NotFound("Cancellation request not found or already processed"))
	}

	return c.JSON(fiber.Map{"message": "Cancellation rejected successfully"})
}

// autoFillFromWaitingList fills registered slots from waiting list (recursive)
func autoFillFromWaitingList(tx pgx.Tx, sessionID string) error {
	// Get first waiting participant
	getWaitingQuery := `
		SELECT id, participant_type FROM open_play_participants
		WHERE session_id = $1 AND participant_status = 'WAITING'
		ORDER BY joined_at ASC
		LIMIT 1
	`
	var waitingID, participantType string
	err := tx.QueryRow(context.Background(), getWaitingQuery, sessionID).Scan(&waitingID, &participantType)
	if err == pgx.ErrNoRows {
		// No more waiting participants
		return nil
	}
	if err != nil {
		return err
	}

	// Get session slot info
	getSessionQuery := `
		SELECT max_members, max_non_members FROM open_play_sessions WHERE id = $1
	`
	var maxMembers, maxNonMembers int
	err = tx.QueryRow(context.Background(), getSessionQuery, sessionID).Scan(&maxMembers, &maxNonMembers)
	if err != nil {
		return err
	}

	// Get current registered count for this type
	var countQuery string
	if participantType == "MEMBER" {
		countQuery = `
			SELECT COALESCE(COUNT(*), 0) FROM open_play_participants
			WHERE session_id = $1 AND participant_type = 'MEMBER' AND participant_status = 'REGISTERED'
		`
	} else {
		countQuery = `
			SELECT COALESCE(COUNT(*), 0) FROM open_play_participants
			WHERE session_id = $1 AND participant_type = 'NON_MEMBER' AND participant_status = 'REGISTERED'
		`
	}

	var registeredCount int
	err = tx.QueryRow(context.Background(), countQuery, sessionID).Scan(&registeredCount)
	if err != nil {
		return err
	}

	// Check if slot is available
	var slotAvailable bool
	if participantType == "MEMBER" {
		slotAvailable = registeredCount < maxMembers
	} else {
		slotAvailable = registeredCount < maxNonMembers
	}

	if !slotAvailable {
		// No slot available, return
		return nil
	}

	// Update waiting participant to REGISTERED (AUTO_FILLED)
	now := time.Now()
	updateQuery := `
		UPDATE open_play_participants
		SET participant_status = 'AUTO_FILLED', filled_from_waiting_at = $1, updated_at = NOW()
		WHERE id = $2
	`
	_, err = tx.Exec(context.Background(), updateQuery, now, waitingID)
	if err != nil {
		return err
	}

	// Recursively check if another waiting participant can be filled
	return autoFillFromWaitingList(tx, sessionID)
}

// GetSessionStats retrieves statistics for a session
func (s *Service) GetSessionStats(c *fiber.Ctx) error {
	sessionID := c.Params("id")

	query := `
		SELECT
			(SELECT COUNT(*) FROM open_play_participants WHERE session_id = $1 AND participant_type = 'MEMBER' AND participant_status = 'REGISTERED') as member_registered,
			(SELECT COUNT(*) FROM open_play_participants WHERE session_id = $1 AND participant_type = 'MEMBER' AND participant_status = 'AUTO_FILLED') as member_auto_filled,
			(SELECT COUNT(*) FROM open_play_participants WHERE session_id = $1 AND participant_type = 'NON_MEMBER' AND participant_status = 'REGISTERED') as non_member_registered,
			(SELECT COUNT(*) FROM open_play_participants WHERE session_id = $1 AND participant_type = 'NON_MEMBER' AND participant_status = 'AUTO_FILLED') as non_member_auto_filled,
			(SELECT COUNT(*) FROM open_play_participants WHERE session_id = $1 AND participant_status = 'WAITING') as waiting_count,
			(SELECT COUNT(*) FROM open_play_cancellations WHERE session_id = $1 AND request_status = 'PENDING') as pending_cancellations
	`

	var stats struct {
		MemberRegistered      int `json:"member_registered"`
		MemberAutoFilled      int `json:"member_auto_filled"`
		NonMemberRegistered   int `json:"non_member_registered"`
		NonMemberAutoFilled   int `json:"non_member_auto_filled"`
		WaitingCount          int `json:"waiting_count"`
		PendingCancellations  int `json:"pending_cancellations"`
	}

	err := s.db.QueryRow(context.Background(), query, sessionID).Scan(
		&stats.MemberRegistered, &stats.MemberAutoFilled,
		&stats.NonMemberRegistered, &stats.NonMemberAutoFilled,
		&stats.WaitingCount, &stats.PendingCancellations,
	)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch session stats"))
	}

	return c.JSON(stats)
}

// LinkMabarSession links a mabar session to an open play session and syncs attendances
func (s *Service) LinkMabarSession(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	var req struct {
		MabarSessionID string `json:"mabar_session_id"`
	}
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	if req.MabarSessionID == "" {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "mabar_session_id required"))
	}

	ctx := context.Background()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to start transaction"))
	}
	defer tx.Rollback(ctx)

	// Update mabar_sessions to link to open_play_session
	updateQuery := `
		UPDATE mabar_sessions 
		SET open_play_session_id = $1, updated_at = NOW()
		WHERE id = $2
	`
	_, err = tx.Exec(ctx, updateQuery, sessionID, req.MabarSessionID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to link mabar session"))
	}

	// Call sync function to migrate attendances to open_play_participants
	syncQuery := `
		SELECT * FROM sync_mabar_to_open_play($1, $2)
	`
	var syncedCount int
	var errorMsg *string
	err = tx.QueryRow(ctx, syncQuery, req.MabarSessionID, sessionID).Scan(&syncedCount, &errorMsg)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to sync attendances"))
	}

	if errorMsg != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("SYNC_ERROR", *errorMsg))
	}

	if err = tx.Commit(ctx); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to commit transaction"))
	}

	return c.JSON(fiber.Map{
		"message":       "Mabar session linked and synced successfully",
		"synced_count":  syncedCount,
		"session_id":    sessionID,
		"mabar_session_id": req.MabarSessionID,
	})
}

// UnlinkMabarSession unlinks a mabar session from open play (keeps participants)
func (s *Service) UnlinkMabarSession(c *fiber.Ctx) error {
	sessionID := c.Params("id")

	query := `
		UPDATE mabar_sessions 
		SET open_play_session_id = NULL, updated_at = NOW()
		WHERE open_play_session_id = $1
		RETURNING id
	`

	var mabarID string
	err := s.db.QueryRow(context.Background(), query, sessionID).Scan(&mabarID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to unlink mabar session"))
	}

	return c.JSON(fiber.Map{
		"message": "Mabar session unlinked successfully",
		"mabar_session_id": mabarID,
	})
}

// AddPlayerManually adds a new non-member player manually to a session (admin only)
func (s *Service) AddPlayerManually(c *fiber.Ctx) error {
	sessionID := c.Params("id")
	var req struct {
		PlayerName string `json:"player_name"`
		PhoneNumber *string `json:"phone_number"`
	}
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("INVALID_REQUEST", "Invalid request body"))
	}

	if req.PlayerName == "" {
		return httpx.WriteAppError(c, httpx.BadRequest("VALIDATION_ERROR", "player_name is required"))
	}

	ctx := context.Background()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to start transaction"))
	}
	defer tx.Rollback(ctx)

	// Check if session exists
	var maxNonMembers int
	getSessionQuery := `SELECT max_non_members FROM open_play_sessions WHERE id = $1`
	err = tx.QueryRow(ctx, getSessionQuery, sessionID).Scan(&maxNonMembers)
	if err == pgx.ErrNoRows {
		return httpx.WriteAppError(c, httpx.NotFound("Session not found"))
	}
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to fetch session"))
	}

	// Create a temporary player record for this non-member
	playerID := generateUUID()
	createPlayerQuery := `
		INSERT INTO players (id, name, phone_number, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
	`
	_, err = tx.Exec(ctx, createPlayerQuery, playerID, req.PlayerName, req.PhoneNumber)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to create player record"))
	}

	// Add participant to session
	participantID := generateUUID()
	addParticipantQuery := `
		INSERT INTO open_play_participants (id, session_id, player_id, participant_type, participant_status, joined_at)
		VALUES ($1, $2, $3, 'NON_MEMBER', 'REGISTERED', NOW())
		ON CONFLICT (session_id, player_id) DO NOTHING
	`
	result, err := tx.Exec(ctx, addParticipantQuery, participantID, sessionID, playerID)
	if err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to add participant"))
	}

	if result.RowsAffected() == 0 {
		return httpx.WriteAppError(c, httpx.BadRequest("DUPLICATE", "Player already registered in this session"))
	}

	if err = tx.Commit(ctx); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("DB_ERROR", "Failed to commit transaction"))
	}

	return c.JSON(fiber.Map{
		"message": "Player added successfully",
		"player_id": playerID,
		"participant_id": participantID,
	})
}

func generateUUID() string {
	return uuid.New().String()
}
