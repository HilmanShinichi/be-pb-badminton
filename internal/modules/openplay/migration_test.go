package openplay

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMabarToOpenPlayMigration verifies the sync_mabar_to_open_play() function
// correctly migrates attendances from a mabar_sessions to open_play_participants.
func TestMabarToOpenPlayMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Setup: Create test database connection
	dbURL := "postgres://postgres:postgres@localhost:5432/pb_kecebong"
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping: cannot connect to test database: %v", err)
	}
	defer pool.Close()

	// Create test data: player, period, mabar session, attendances
	playerId := uuid.New()
	periodId := uuid.New()
	mabarId := uuid.New()
	openPlayId := uuid.New()

	// Insert test player
	_, err = pool.Exec(ctx, `
		INSERT INTO players (id, name, phone, is_active)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (id) DO NOTHING
	`, playerId, "Test Player", "08123456789")
	if err != nil {
		t.Fatalf("insert player: %v", err)
	}

	// Insert test period
	now := time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO membership_periods (id, period_name, start_date, end_date, is_registration_open)
		VALUES ($1, $2, $3, $4, true)
		ON CONFLICT (id) DO NOTHING
	`, periodId, "Test Period", now, now.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("insert period: %v", err)
	}

	// Insert test mabar session
	_, err = pool.Exec(ctx, `
		INSERT INTO mabar_sessions (id, period_id, session_date, notes, shuttlecock_count)
		VALUES ($1, $2, $3, $4, 0)
		ON CONFLICT (id) DO NOTHING
	`, mabarId, periodId, now.Format("2006-01-02"), "Test Session")
	if err != nil {
		t.Fatalf("insert mabar session: %v", err)
	}

	// Insert test attendances (simulating auto-added members)
	_, err = pool.Exec(ctx, `
		INSERT INTO attendances (id, mabar_id, player_id, status, is_member)
		VALUES ($1, $2, $3, 'LISTED', true)
		ON CONFLICT (id) DO NOTHING
	`, uuid.New(), mabarId, playerId)
	if err != nil {
		t.Fatalf("insert attendance: %v", err)
	}

	// Insert test open_play_session
	_, err = pool.Exec(ctx, `
		INSERT INTO open_play_sessions (id, period_id, session_date, notes, member_slots, non_member_slots, waiting_slots)
		VALUES ($1, $2, $3, $4, 20, 18, 3)
		ON CONFLICT (id) DO NOTHING
	`, openPlayId, periodId, now.Format("2006-01-02"), "Test Open Play")
	if err != nil {
		t.Fatalf("insert open_play_session: %v", err)
	}

	// Test: Call sync_mabar_to_open_play function
	var syncedCount int
	var errorMsg *string
	err = pool.QueryRow(ctx, `
		SELECT synced_count, error_message
		FROM sync_mabar_to_open_play($1, $2)
	`, mabarId, openPlayId).Scan(&syncedCount, &errorMsg)
	if err != nil {
		t.Fatalf("sync_mabar_to_open_play: %v", err)
	}

	if syncedCount != 1 {
		t.Errorf("synced_count = %d, want 1", syncedCount)
	}
	if errorMsg != nil {
		t.Errorf("error_message = %v, want nil", *errorMsg)
	}

	// Verify: Check if participant was created in open_play_participants
	var participantCount int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM open_play_participants
		WHERE open_play_session_id = $1 AND player_id = $2 AND participant_type = 'MEMBER'
	`, openPlayId, playerId).Scan(&participantCount)
	if err != nil {
		t.Fatalf("query participants: %v", err)
	}

	if participantCount != 1 {
		t.Errorf("participant_count = %d, want 1", participantCount)
	}

	// Verify: Check participant status is REGISTERED
	var status string
	err = pool.QueryRow(ctx, `
		SELECT status FROM open_play_participants
		WHERE open_play_session_id = $1 AND player_id = $2
		LIMIT 1
	`, openPlayId, playerId).Scan(&status)
	if err != nil {
		t.Fatalf("query participant status: %v", err)
	}

	if status != "REGISTERED" {
		t.Errorf("status = %s, want REGISTERED", status)
	}

	// Verify: Check mabar_sessions.open_play_session_id is now linked
	var linkedId uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT open_play_session_id FROM mabar_sessions WHERE id = $1
	`, mabarId).Scan(&linkedId)
	if err != nil {
		t.Fatalf("query mabar link: %v", err)
	}

	if linkedId != openPlayId {
		t.Errorf("linked open_play_session_id = %s, want %s", linkedId, openPlayId)
	}

	t.Log("✓ Migration test passed: attendances synced, participant created, session linked")
}

// TestUpdateMabarFromOpenPlay verifies reverse sync when open_play_participants status changes.
func TestUpdateMabarFromOpenPlay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Setup: Create test database connection
	dbURL := "postgres://postgres:postgres@localhost:5432/pb_kecebong"
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping: cannot connect to test database: %v", err)
	}
	defer pool.Close()

	// Create test data
	playerId := uuid.New()
	periodId := uuid.New()
	mabarId := uuid.New()
	openPlayId := uuid.New()
	participantId := uuid.New()

	// Insert test player
	_, err = pool.Exec(ctx, `
		INSERT INTO players (id, name, phone, is_active)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (id) DO NOTHING
	`, playerId, "Test Player 2", "08123456790")
	if err != nil {
		t.Fatalf("insert player: %v", err)
	}

	// Insert test period
	now := time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO membership_periods (id, period_name, start_date, end_date, is_registration_open)
		VALUES ($1, $2, $3, $4, true)
		ON CONFLICT (id) DO NOTHING
	`, periodId, "Test Period 2", now, now.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("insert period: %v", err)
	}

	// Insert test mabar session
	_, err = pool.Exec(ctx, `
		INSERT INTO mabar_sessions (id, period_id, session_date, notes, shuttlecock_count, open_play_session_id)
		VALUES ($1, $2, $3, $4, 0, $5)
		ON CONFLICT (id) DO NOTHING
	`, mabarId, periodId, now.Format("2006-01-02"), "Test Session 2", openPlayId)
	if err != nil {
		t.Fatalf("insert mabar session: %v", err)
	}

	// Insert test attendance
	attendanceId := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO attendances (id, mabar_id, player_id, status, is_member)
		VALUES ($1, $2, $3, 'LISTED', true)
		ON CONFLICT (id) DO NOTHING
	`, attendanceId, mabarId, playerId)
	if err != nil {
		t.Fatalf("insert attendance: %v", err)
	}

	// Insert test open_play_session
	_, err = pool.Exec(ctx, `
		INSERT INTO open_play_sessions (id, period_id, session_date, notes, member_slots, non_member_slots, waiting_slots)
		VALUES ($1, $2, $3, $4, 20, 18, 3)
		ON CONFLICT (id) DO NOTHING
	`, openPlayId, periodId, now.Format("2006-01-02"), "Test Open Play 2")
	if err != nil {
		t.Fatalf("insert open_play_session: %v", err)
	}

	// Insert test participant
	_, err = pool.Exec(ctx, `
		INSERT INTO open_play_participants (id, open_play_session_id, player_id, participant_type, status, created_at)
		VALUES ($1, $2, $3, 'MEMBER', 'REGISTERED', NOW())
		ON CONFLICT (id) DO NOTHING
	`, participantId, openPlayId, playerId)
	if err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	// Test: Call update_mabar_from_open_play to change status to CANCELLED
	var newAttendanceStatus string
	err = pool.QueryRow(ctx, `
		SELECT update_mabar_from_open_play($1, $2)
	`, participantId, "CANCELLED").Scan(&newAttendanceStatus)
	if err != nil {
		t.Fatalf("update_mabar_from_open_play: %v", err)
	}

	if newAttendanceStatus != "CANCELLED" {
		t.Errorf("returned status = %s, want CANCELLED", newAttendanceStatus)
	}

	// Verify: Check if attendance status was updated
	var attendanceStatus string
	err = pool.QueryRow(ctx, `
		SELECT status FROM attendances WHERE id = $1
	`, attendanceId).Scan(&attendanceStatus)
	if err != nil {
		t.Fatalf("query attendance status: %v", err)
	}

	if attendanceStatus != "CANCELLED" {
		t.Errorf("attendance status = %s, want CANCELLED", attendanceStatus)
	}

	t.Log("✓ Reverse sync test passed: attendance status updated from open_play_participants change")
}
