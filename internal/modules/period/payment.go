package period

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type MembershipPayment struct {
	ID              string  `json:"id"`
	PeriodID        string  `json:"period_id"`
	PeriodName      string  `json:"period_name,omitempty"`
	PlayerID        string  `json:"player_id"`
	PlayerName      string  `json:"player_name,omitempty"`
	PlayerPhone     *string `json:"player_phone,omitempty"`
	BaseAmount      int64   `json:"base_amount"`
	UniqueCode      int     `json:"unique_code"`
	TotalAmount     int64   `json:"total_amount"`
	Status          string  `json:"status"` // WAITING_PAYMENT, PENDING, PAID, FAILED
	ExpiresAt       string  `json:"expires_at"`
	RemainingSecs   int64   `json:"remaining_seconds"`
	PaidAt          *string `json:"paid_at"`
	ProofURL        *string `json:"proof_url"`
	ProofPath       *string `json:"proof_path,omitempty"`
	Note            *string `json:"note"`
	VerifiedBy      *string `json:"verified_by"`
	VerifiedAt      *string `json:"verified_at"`
	RejectionReason *string `json:"rejection_reason"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

type RegisteredMemberItem struct {
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
	Tier     string `json:"tier,omitempty"`
	JoinedAt string `json:"joined_at"`
	Status   string `json:"status"`
}

type PendingPaymentItem struct {
	PaymentID string  `json:"payment_id"`
	PlayerID  string  `json:"player_id"`
	Name      string  `json:"name"`
	PaidAt    *string `json:"paid_at"`
	CreatedAt string  `json:"created_at"`
	Status    string  `json:"status"`
}

type PeriodRegistrationInfo struct {
	Period            Period                 `json:"period"`
	CurrentMembers    int                    `json:"current_members"`
	RemainingSlots    *int                   `json:"remaining_slots"`
	IsFull            bool                   `json:"is_full"`
	CanRegister       bool                   `json:"can_register"`
	CloseReason       string                 `json:"close_reason,omitempty"`
	EligiblePlayers   []RegistrationPlayer   `json:"eligible_players,omitempty"`
	RegisteredMembers []RegisteredMemberItem `json:"registered_members"`
	PendingPayments   []PendingPaymentItem   `json:"pending_payments"`
}

type RegistrationPlayer struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Phone *string `json:"phone"`
}

// ExpireStalePayments updates all WAITING_PAYMENT payments past their expires_at to FAILED.
func (s *Service) ExpireStalePayments(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE membership_payments
		SET status = 'FAILED', updated_at = now()
		WHERE status = 'WAITING_PAYMENT' AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// GetRegistrationInfo returns public period registration details and quota status.
func (s *Service) GetRegistrationInfo(c *fiber.Ctx) error {
	periodID := c.Params("id")
	if _, err := uuid.Parse(periodID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid period ID."))
	}
	_, _ = s.autoClosePeriodIfExpired(c.Context(), periodID)
	p, err := s.fetch(c.Context(), periodID)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	var currentMembers int
	_ = s.db.QueryRow(c.Context(), `
		SELECT COUNT(*) FROM memberships
		WHERE period_id = $1 AND status <> 'WITHDRAWN'`, periodID).Scan(&currentMembers)

	var remainingSlots *int
	if p.MaxMembers != nil && *p.MaxMembers > 0 {
		rem := *p.MaxMembers - currentMembers
		if rem < 0 {
			rem = 0
		}
		remainingSlots = &rem
	}

	isFull := p.MaxMembers != nil && currentMembers >= *p.MaxMembers
	isExpired := isRegistrationDeadlinePassed(p.RegistrationExpiresAt, time.Now().UTC())
	canRegister := p.IsRegistrationOpen && !isFull && !isExpired && p.Status == "ACTIVE"

	closeReason := ""
	if p.Status != "ACTIVE" {
		closeReason = "Periode ini belum aktif atau sudah selesai."
	} else if isFull {
		closeReason = "Kuota member untuk periode ini sudah penuh."
	} else if isExpired {
		closeReason = "Batas waktu pendaftaran member untuk periode ini telah berakhir."
	} else if !p.IsRegistrationOpen {
		closeReason = "Pendaftaran member untuk periode ini sedang ditutup oleh pengurus."
	}

	eligiblePlayers := []RegistrationPlayer{}
	if p.Status == "ACTIVE" {
		plRows, err := s.db.Query(c.Context(), `
			SELECT p.id::text, p.name, p.phone
			FROM players p
			WHERE p.status = 'ACTIVE'
			  AND p.id NOT IN (
			      SELECT player_id FROM memberships WHERE period_id = $1 AND status <> 'WITHDRAWN'
			  )
			ORDER BY p.name ASC`, periodID)
		if err == nil {
			defer plRows.Close()
			for plRows.Next() {
				var rp RegistrationPlayer
				if err := plRows.Scan(&rp.ID, &rp.Name, &rp.Phone); err == nil {
					eligiblePlayers = append(eligiblePlayers, rp)
				}
			}
		}
	}

	registeredMembers := []RegisteredMemberItem{}
	mRows, err := s.db.Query(c.Context(), `
		SELECT p.id::text, p.name, 
		       COALESCE(to_char(m.joined_at, 'YYYY-MM-DD"T"HH24:MI:SSZ'), to_char(now(), 'YYYY-MM-DD"T"HH24:MI:SSZ')), 
		       m.status
		FROM memberships m
		JOIN players p ON p.id = m.player_id
		WHERE m.period_id = $1 AND m.status <> 'WITHDRAWN'
		ORDER BY m.joined_at ASC, p.name ASC`, periodID)
	if err == nil {
		defer mRows.Close()
		for mRows.Next() {
			var it RegisteredMemberItem
			if err := mRows.Scan(&it.PlayerID, &it.Name, &it.JoinedAt, &it.Status); err == nil {
				registeredMembers = append(registeredMembers, it)
			}
		}
	}

	pendingPayments := []PendingPaymentItem{}
	payRows, err := s.db.Query(c.Context(), `
		SELECT mp.id::text, p.id::text, p.name,
		       to_char(mp.paid_at, 'YYYY-MM-DD'),
		       to_char(mp.created_at, 'YYYY-MM-DD"T"HH24:MI:SSZ'),
		       mp.status
		FROM membership_payments mp
		JOIN players p ON p.id = mp.player_id
		WHERE mp.period_id = $1 AND mp.status = 'PENDING'
		ORDER BY mp.created_at ASC`, periodID)
	if err == nil {
		defer payRows.Close()
		for payRows.Next() {
			var pi PendingPaymentItem
			if err := payRows.Scan(&pi.PaymentID, &pi.PlayerID, &pi.Name, &pi.PaidAt, &pi.CreatedAt, &pi.Status); err == nil {
				pendingPayments = append(pendingPayments, pi)
			}
		}
	}

	return httpx.OK(c, http.StatusOK, PeriodRegistrationInfo{
		Period:            p,
		CurrentMembers:    currentMembers,
		RemainingSlots:    remainingSlots,
		IsFull:            isFull,
		CanRegister:       canRegister,
		CloseReason:       closeReason,
		EligiblePlayers:   eligiblePlayers,
		RegisteredMembers: registeredMembers,
		PendingPayments:   pendingPayments,
	})
}

// ListOpenRegistrationPeriods returns active periods currently accepting member registrations.
func (s *Service) ListOpenRegistrationPeriods(c *fiber.Ctx) error {
	_, _ = s.autoCloseExpiredRegistrations(c.Context())
	rows, err := s.db.Query(c.Context(), `
		SELECT `+periodCols+`
		FROM membership_periods
		WHERE status = 'ACTIVE'
		  AND is_registration_open = TRUE
		  AND (registration_expires_at IS NULL OR registration_expires_at > now())
		ORDER BY start_date DESC`)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load periods.")
	}
	defer rows.Close()

	list := []PeriodRegistrationInfo{}
	for rows.Next() {
		if p, err := scanPeriod(rows); err == nil {
			var currentMembers int
			_ = s.db.QueryRow(c.Context(), `
				SELECT COUNT(*) FROM memberships
				WHERE period_id = $1 AND status <> 'WITHDRAWN'`, p.ID).Scan(&currentMembers)
			var remainingSlots *int
			if p.MaxMembers != nil && *p.MaxMembers > 0 {
				rem := *p.MaxMembers - currentMembers
				if rem < 0 {
					rem = 0
				}
				remainingSlots = &rem
			}
			isFull := p.MaxMembers != nil && currentMembers >= *p.MaxMembers
			list = append(list, PeriodRegistrationInfo{
				Period:         p,
				CurrentMembers: currentMembers,
				RemainingSlots: remainingSlots,
				IsFull:         isFull,
				CanRegister:    !isFull,
			})
		}
	}
	return httpx.OK(c, http.StatusOK, list)
}

// ListRegistrationPlayers returns active players who are not yet members in this period.
func (s *Service) ListRegistrationPlayers(c *fiber.Ctx) error {
	periodID := c.Params("id")
	if _, err := uuid.Parse(periodID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid period ID.")
	}

	rows, err := s.db.Query(c.Context(), `
		SELECT p.id::text, p.name, p.phone
		FROM players p
		WHERE p.status = 'ACTIVE'
		  AND p.id NOT IN (
		      SELECT player_id FROM memberships
		      WHERE period_id = $1 AND status <> 'WITHDRAWN'
		  )
		ORDER BY p.name ASC`, periodID)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load players.")
	}
	defer rows.Close()

	players := []RegistrationPlayer{}
	for rows.Next() {
		var pl RegistrationPlayer
		if err := rows.Scan(&pl.ID, &pl.Name, &pl.Phone); err == nil {
			players = append(players, pl)
		}
	}
	return httpx.OK(c, http.StatusOK, players)
}

type initiatePaymentInput struct {
	PlayerID string `json:"player_id"`
}

// InitiatePayment generates a nominal with unique 3-digit code and a 30-minute countdown.
func (s *Service) InitiatePayment(c *fiber.Ctx) error {
	periodID := c.Params("id")
	if _, err := uuid.Parse(periodID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid period ID."))
	}
	_, _ = s.autoClosePeriodIfExpired(c.Context(), periodID)
	p, err := s.fetch(c.Context(), periodID)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	if p.Status != "ACTIVE" {
		return httpx.Conflict("PERIOD_INACTIVE", "Periode ini belum aktif atau sudah selesai.")
	}
	if isRegistrationDeadlinePassed(p.RegistrationExpiresAt, time.Now().UTC()) {
		return httpx.Conflict("REGISTRATION_EXPIRED", "Batas waktu pendaftaran untuk periode ini telah berakhir.")
	}
	if !p.IsRegistrationOpen {
		return httpx.Conflict("REGISTRATION_CLOSED", "Pendaftaran untuk periode ini sedang ditutup.")
	}

	var in initiatePaymentInput
	if err := httpx.Decode(c, &in); err != nil || in.PlayerID == "" {
		return httpx.BadRequest("BAD_REQUEST", "Pemain harus dipilih.")
	}
	if _, err := uuid.Parse(in.PlayerID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "ID Pemain tidak valid.")
	}

	// Quota check
	if p.MaxMembers != nil {
		var memberCount int
		_ = s.db.QueryRow(c.Context(), `
			SELECT COUNT(*) FROM memberships
			WHERE period_id = $1 AND status <> 'WITHDRAWN'`, periodID).Scan(&memberCount)
		if memberCount >= *p.MaxMembers {
			return httpx.Conflict("PERIOD_FULL", "Kuota member untuk periode ini sudah penuh.")
		}
	}

	// Verify player exists and active
	var playerName string
	var playerStatus string
	err = s.db.QueryRow(c.Context(), `SELECT name, status FROM players WHERE id = $1`, in.PlayerID).
		Scan(&playerName, &playerStatus)
	if err != nil || playerStatus != "ACTIVE" {
		return httpx.BadRequest("BAD_REQUEST", "Pemain tidak ditemukan atau tidak aktif.")
	}

	// Check if already an active member
	var isMember bool
	_ = s.db.QueryRow(c.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM memberships
			WHERE period_id = $1 AND player_id = $2 AND status <> 'WITHDRAWN'
		)`, periodID, in.PlayerID).Scan(&isMember)
	if isMember {
		return httpx.Conflict("ALREADY_MEMBER", "Pemain sudah terdaftar sebagai member di periode ini.")
	}

	// Expire any stale WAITING_PAYMENT first
	_, _ = s.ExpireStalePayments(c.Context())

	// Check if active payment already exists for this player
	var existing MembershipPayment
	var expTime time.Time
	err = s.db.QueryRow(c.Context(), `
		SELECT mp.id::text, mp.period_id::text, mp.player_id::text, pl.name, mp.base_amount,
		       mp.unique_code, mp.total_amount, mp.status, mp.expires_at, mp.paid_at::text,
		       mp.proof_url, mp.note, mp.rejection_reason, mp.created_at::text, mp.updated_at::text
		FROM membership_payments mp
		JOIN players pl ON pl.id = mp.player_id
		WHERE mp.period_id = $1 AND mp.player_id = $2 AND mp.status IN ('WAITING_PAYMENT', 'PENDING')
		ORDER BY mp.created_at DESC LIMIT 1`, periodID, in.PlayerID).
		Scan(&existing.ID, &existing.PeriodID, &existing.PlayerID, &existing.PlayerName,
			&existing.BaseAmount, &existing.UniqueCode, &existing.TotalAmount, &existing.Status,
			&expTime, &existing.PaidAt, &existing.ProofURL, &existing.Note, &existing.RejectionReason,
			&existing.CreatedAt, &existing.UpdatedAt)

	if err == nil {
		existing.ExpiresAt = expTime.Format(time.RFC3339)
		rem := int64(time.Until(expTime).Seconds())
		if rem < 0 {
			rem = 0
		}
		existing.RemainingSecs = rem
		existing.PeriodName = p.Name
		return c.Status(http.StatusConflict).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "ACTIVE_PAYMENT_EXISTS",
				"message": "Selesaikan pembayaran sebelumnya terlebih dahulu.",
				"payment": existing,
			},
		})
	}

	// Find active unique codes in this period to prevent collisions
	rows, err := s.db.Query(c.Context(), `
		SELECT unique_code FROM membership_payments
		WHERE period_id = $1 AND status IN ('WAITING_PAYMENT', 'PENDING')`, periodID)
	usedCodes := map[int]bool{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var code int
			if err := rows.Scan(&code); err == nil {
				usedCodes[code] = true
			}
		}
	}

	// Generate random 3-digit code between 1 and 999
	uniqueCode := 0
	for attempt := 0; attempt < 50; attempt++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(999))
		cCandidate := int(n.Int64()) + 1 // 1..999
		if !usedCodes[cCandidate] {
			uniqueCode = cCandidate
			break
		}
	}
	if uniqueCode == 0 {
		return httpx.Conflict("NO_CODE_AVAILABLE", "Semua kode unik sedang digunakan, silakan coba sesaat lagi.")
	}

	baseAmount := p.CommitmentFee
	totalAmount := baseAmount + int64(uniqueCode)
	expiresAt := time.Now().Add(30 * time.Minute)
	paymentID := uuid.New().String()

	var createdAt, updatedAt time.Time
	err = s.db.QueryRow(c.Context(), `
		INSERT INTO membership_payments
		(id, period_id, player_id, base_amount, unique_code, total_amount, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'WAITING_PAYMENT', $7)
		RETURNING created_at, updated_at`,
		paymentID, periodID, in.PlayerID, baseAmount, uniqueCode, totalAmount, expiresAt).
		Scan(&createdAt, &updatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal membuat tagihan pembayaran.")
	}

	res := MembershipPayment{
		ID:            paymentID,
		PeriodID:      periodID,
		PeriodName:    p.Name,
		PlayerID:      in.PlayerID,
		PlayerName:    playerName,
		BaseAmount:    baseAmount,
		UniqueCode:    uniqueCode,
		TotalAmount:   totalAmount,
		Status:        "WAITING_PAYMENT",
		ExpiresAt:     expiresAt.Format(time.RFC3339),
		RemainingSecs: int64(time.Until(expiresAt).Seconds()),
		CreatedAt:     createdAt.Format(time.RFC3339),
		UpdatedAt:     updatedAt.Format(time.RFC3339),
	}
	return httpx.OK(c, http.StatusCreated, res)
}

// GetPayment fetches a single payment with real-time remaining countdown and auto-expiration.
func (s *Service) GetPayment(c *fiber.Ctx) error {
	paymentID := c.Params("id")
	if _, err := uuid.Parse(paymentID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid payment ID.")
	}

	// Auto-expire stale
	_, _ = s.ExpireStalePayments(c.Context())

	var mp MembershipPayment
	var expTime time.Time
	err := s.db.QueryRow(c.Context(), `
		SELECT mp.id::text, mp.period_id::text, pr.name, mp.player_id::text, pl.name, pl.phone,
		       mp.base_amount, mp.unique_code, mp.total_amount, mp.status, mp.expires_at,
		       mp.paid_at::text, mp.proof_url, mp.note, mp.rejection_reason,
		       mp.created_at::text, mp.updated_at::text
		FROM membership_payments mp
		JOIN membership_periods pr ON pr.id = mp.period_id
		JOIN players pl ON pl.id = mp.player_id
		WHERE mp.id = $1`, paymentID).
		Scan(&mp.ID, &mp.PeriodID, &mp.PeriodName, &mp.PlayerID, &mp.PlayerName, &mp.PlayerPhone,
			&mp.BaseAmount, &mp.UniqueCode, &mp.TotalAmount, &mp.Status, &expTime,
			&mp.PaidAt, &mp.ProofURL, &mp.Note, &mp.RejectionReason, &mp.CreatedAt, &mp.UpdatedAt)
	if err != nil {
		return httpx.NotFound("Pembayaran tidak ditemukan.")
	}

	mp.ExpiresAt = expTime.Format(time.RFC3339)
	rem := int64(time.Until(expTime).Seconds())
	if rem < 0 {
		rem = 0
	}
	mp.RemainingSecs = rem

	return httpx.OK(c, http.StatusOK, mp)
}

// SubmitPaymentProof accepts payment proof image upload, saves to S3, and moves status to PENDING.
func (s *Service) SubmitPaymentProof(c *fiber.Ctx) error {
	paymentID := c.Params("id")
	if _, err := uuid.Parse(paymentID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid payment ID.")
	}

	// First verify payment exists
	var status string
	var periodID string
	var expiresAt time.Time
	err := s.db.QueryRow(c.Context(), `
		SELECT status, period_id::text, expires_at
		FROM membership_payments WHERE id = $1`, paymentID).
		Scan(&status, &periodID, &expiresAt)
	if err != nil {
		return httpx.NotFound("Pembayaran tidak ditemukan.")
	}

	// Auto-expire check: if status is WAITING_PAYMENT and past 30 minutes, reject and mark FAILED
	if status == "WAITING_PAYMENT" && time.Now().After(expiresAt) {
		_, _ = s.db.Exec(c.Context(), `
			UPDATE membership_payments SET status = 'FAILED', updated_at = now() WHERE id = $1`, paymentID)
		return httpx.BadRequest("PAYMENT_EXPIRED", "Waktu pembayaran telah habis (melebihi 30 menit). Silakan daftar ulang.")
	}

	if status != "WAITING_PAYMENT" {
		return httpx.Conflict("INVALID_STATUS", fmt.Sprintf("Pembayaran sedang berstatus %s, tidak dapat mengunggah bukti.", status))
	}

	// Parse multipart file
	fileHeader, err := c.FormFile("proof")
	if err != nil {
		fileHeader, err = c.FormFile("file")
	}
	if err != nil {
		return httpx.BadRequest("MISSING_FILE", "File bukti pembayaran wajib diunggah.")
	}

	if fileHeader.Size > 5*1024*1024 {
		return httpx.BadRequest("FILE_TOO_LARGE", "Ukuran file bukti pembayaran maksimal 5MB.")
	}

	f, err := fileHeader.Open()
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal membaca file bukti.")
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal membaca file bukti.")
	}

	contentType := fileHeader.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}

	if !strings.HasPrefix(contentType, "image/") {
		return httpx.BadRequest("INVALID_FILE_TYPE", "File harus berupa gambar (JPG, PNG, WebP).")
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if ext == "" {
		if strings.Contains(contentType, "png") {
			ext = ".png"
		} else if strings.Contains(contentType, "webp") {
			ext = ".webp"
		} else {
			ext = ".jpg"
		}
	}

	// Generate S3 key: proofs/{period_id}/{payment_id}-{timestamp}{ext}
	s3Key := fmt.Sprintf("proofs/%s/%s-%d%s", periodID, paymentID, time.Now().Unix(), ext)

	publicURL, err := s.storage.Upload(c.Context(), s3Key, data, contentType)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "STORAGE_ERROR", fmt.Sprintf("Gagal mengunggah bukti ke storage: %v", err))
	}

	paidAtStr := strings.TrimSpace(c.FormValue("paid_at"))
	if paidAtStr == "" {
		paidAtStr = time.Now().Format("2006-01-02")
	} else {
		if _, err := time.Parse("2006-01-02", paidAtStr); err != nil {
			return httpx.BadRequest("INVALID_DATE", "Format tanggal bayar harus YYYY-MM-DD.")
		}
	}

	note := strings.TrimSpace(c.FormValue("note"))

	// Status transitions to PENDING!
	var mp MembershipPayment
	var expTime time.Time
	err = s.db.QueryRow(c.Context(), `
		UPDATE membership_payments
		SET status = 'PENDING',
		    proof_url = $2,
		    proof_path = $3,
		    paid_at = $4,
		    note = NULLIF($5, ''),
		    updated_at = now()
		WHERE id = $1
		RETURNING id::text, period_id::text, player_id::text, base_amount, unique_code, total_amount,
		          status, expires_at, paid_at::text, proof_url, note, created_at::text, updated_at::text`,
		paymentID, publicURL, s3Key, paidAtStr, note).
		Scan(&mp.ID, &mp.PeriodID, &mp.PlayerID, &mp.BaseAmount, &mp.UniqueCode, &mp.TotalAmount,
			&mp.Status, &expTime, &mp.PaidAt, &mp.ProofURL, &mp.Note, &mp.CreatedAt, &mp.UpdatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menyimpan status bukti pembayaran.")
	}

	mp.ExpiresAt = expTime.Format(time.RFC3339)
	rem := int64(time.Until(expTime).Seconds())
	if rem < 0 {
		rem = 0
	}
	mp.RemainingSecs = rem

	return httpx.OK(c, http.StatusOK, mp)
}

type updateRegStatusInput struct {
	IsOpen                *bool   `json:"is_registration_open"`
	RegistrationExpiresAt *string `json:"registration_expires_at"`
}

// SetRegistrationStatus lets admin open/close registration and update the registration deadline.
func (s *Service) SetRegistrationStatus(c *fiber.Ctx) error {
	periodID := c.Params("id")
	cur, err := s.fetch(c.Context(), periodID)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	var in updateRegStatusInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid request body.")
	}

	isOpen := cur.IsRegistrationOpen
	if in.IsOpen != nil {
		isOpen = *in.IsOpen
	}
	expiresAt, err := coalesceOptionalUTCTimestamp(in.RegistrationExpiresAt, cur.RegistrationExpiresAt)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}

	now := time.Now().UTC()
	if expiresAt != nil && !expiresAt.After(now) {
		if in.IsOpen != nil && *in.IsOpen {
			return httpx.Conflict("REGISTRATION_DEADLINE_PASSED", "Batas waktu pendaftaran sudah lewat. Perpanjang atau kosongkan batas waktu terlebih dahulu sebelum membuka pendaftaran.")
		}
		if in.RegistrationExpiresAt != nil && strings.TrimSpace(*in.RegistrationExpiresAt) != "" {
			return httpx.Unprocessable("Batas waktu pendaftaran harus di masa depan.")
		}
		isOpen = false
	}

	var savedExpires *string
	err = s.db.QueryRow(c.Context(), `
		UPDATE membership_periods
		SET is_registration_open = $2,
		    registration_expires_at = $3,
		    updated_at = now()
		WHERE id = $1
		RETURNING to_char(registration_expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		periodID, isOpen, expiresAt).Scan(&savedExpires)
	if err != nil {
		return httpx.NotFound("Periode tidak ditemukan.")
	}

	return httpx.OK(c, http.StatusOK, fiber.Map{
		"period_id":               periodID,
		"is_registration_open":    isOpen,
		"registration_expires_at": savedExpires,
	})
}

type updateRegNotesInput struct {
	RegistrationNotes *string `json:"registration_notes"`
}

// UpdateRegistrationNotes lets admin customize member registration notes / S&K.
func (s *Service) UpdateRegistrationNotes(c *fiber.Ctx) error {
	periodID := c.Params("id")
	if _, err := uuid.Parse(periodID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid period ID.")
	}
	if err := s.guardImmutable(c.Context(), periodID); err != nil {
		return httpx.WriteAppError(c, err)
	}

	var in updateRegNotesInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid request body.")
	}

	notes := normalizeOptionalText(in.RegistrationNotes)
	var saved *string
	err := s.db.QueryRow(c.Context(), `
		UPDATE membership_periods
		SET registration_notes = $2, updated_at = now()
		WHERE id = $1
		RETURNING registration_notes`, periodID, notes).Scan(&saved)
	if err != nil {
		return httpx.NotFound("Periode tidak ditemukan.")
	}

	return httpx.OK(c, http.StatusOK, fiber.Map{
		"period_id":          periodID,
		"registration_notes": saved,
	})
}

// ListPayments returns all membership payments for a period (admin view).
func (s *Service) ListPayments(c *fiber.Ctx) error {
	periodID := c.Params("id")
	if _, err := uuid.Parse(periodID); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid period ID.")
	}

	// Auto-expire stale WAITING_PAYMENT
	_, _ = s.ExpireStalePayments(c.Context())

	statusFilter := strings.TrimSpace(c.Query("status"))

	query := `
		SELECT mp.id::text, mp.period_id::text, pr.name, mp.player_id::text, pl.name, pl.phone,
		       mp.base_amount, mp.unique_code, mp.total_amount, mp.status, mp.expires_at,
		       mp.paid_at::text, mp.proof_url, mp.note, mp.verified_by::text, mp.verified_at::text,
		       mp.rejection_reason, mp.created_at::text, mp.updated_at::text
		FROM membership_payments mp
		JOIN membership_periods pr ON pr.id = mp.period_id
		JOIN players pl ON pl.id = mp.player_id
		WHERE mp.period_id = $1`

	args := []any{periodID}
	if statusFilter != "" && statusFilter != "ALL" {
		query += " AND mp.status = $2"
		args = append(args, statusFilter)
	}
	query += " ORDER BY mp.created_at DESC"

	rows, err := s.db.Query(c.Context(), query, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load payments.")
	}
	defer rows.Close()

	payments := []MembershipPayment{}
	for rows.Next() {
		var mp MembershipPayment
		var expTime time.Time
		if err := rows.Scan(&mp.ID, &mp.PeriodID, &mp.PeriodName, &mp.PlayerID, &mp.PlayerName, &mp.PlayerPhone,
			&mp.BaseAmount, &mp.UniqueCode, &mp.TotalAmount, &mp.Status, &expTime,
			&mp.PaidAt, &mp.ProofURL, &mp.Note, &mp.VerifiedBy, &mp.VerifiedAt,
			&mp.RejectionReason, &mp.CreatedAt, &mp.UpdatedAt); err == nil {
			mp.ExpiresAt = expTime.Format(time.RFC3339)
			rem := int64(time.Until(expTime).Seconds())
			if rem < 0 {
				rem = 0
			}
			mp.RemainingSecs = rem
			payments = append(payments, mp)
		}
	}

	return httpx.OK(c, http.StatusOK, payments)
}

type verifyPaymentInput struct {
	Status          string `json:"status"` // PAID or FAILED
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// VerifyPayment lets admin approve (PAID) or reject (FAILED) a payment.
// When PAID, it atomically activates membership and creates a COMMITMENT_FEE revenue.
func (s *Service) VerifyPayment(c *fiber.Ctx) error {
	periodID := c.Params("id")
	paymentID := c.Params("paymentId")
	if _, err := uuid.Parse(periodID); err != nil || uuid.Validate(paymentID) != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid ID.")
	}

	var in verifyPaymentInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid request body.")
	}
	if in.Status != "PAID" && in.Status != "FAILED" {
		return httpx.BadRequest("INVALID_STATUS", "Status harus PAID atau FAILED.")
	}

	claims := httpx.ClaimsFrom(c)
	var adminUserID *string
	if claims != nil && claims.UserID != "" {
		adminUserID = &claims.UserID
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not start transaction.")
	}
	defer tx.Rollback(c.Context())

	var playerID string
	var totalAmount, baseAmount int64
	var paidAt *string
	var currentStatus string
	err = tx.QueryRow(c.Context(), `
		SELECT player_id::text, total_amount, base_amount, paid_at::text, status
		FROM membership_payments
		WHERE id = $1 AND period_id = $2
		FOR UPDATE`, paymentID, periodID).
		Scan(&playerID, &totalAmount, &baseAmount, &paidAt, &currentStatus)
	if err != nil {
		return httpx.NotFound("Pembayaran tidak ditemukan.")
	}

	if currentStatus == "PAID" {
		return httpx.Conflict("ALREADY_PAID", "Pembayaran ini sudah disetujui sebelumnya.")
	}

	if in.Status == "PAID" {
		var maxMembers *int
		if err := tx.QueryRow(c.Context(), `
			SELECT max_members FROM membership_periods
			WHERE id = $1
			FOR UPDATE`, periodID).Scan(&maxMembers); err != nil {
			return httpx.NotFound("Periode tidak ditemukan.")
		}

		var otherMemberCount int
		if err := tx.QueryRow(c.Context(), `
			SELECT COUNT(*) FROM memberships
			WHERE period_id = $1 AND status <> 'WITHDRAWN' AND player_id <> $2`,
			periodID, playerID).Scan(&otherMemberCount); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal memeriksa kuota member.")
		}

		canApprove, fillsLastSlot := evaluateQuotaForApproval(maxMembers, otherMemberCount)
		if !canApprove {
			return httpx.Conflict("PERIOD_FULL", "Kuota member untuk periode ini sudah penuh.")
		}

		// 1. Update payment status to PAID
		_, err = tx.Exec(c.Context(), `
			UPDATE membership_payments
			SET status = 'PAID',
			    verified_by = $2,
			    verified_at = now(),
			    rejection_reason = NULL,
			    updated_at = now()
			WHERE id = $1`, paymentID, adminUserID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal memperbarui status pembayaran.")
		}

		// 2. Add or activate member in period
		_, err = tx.Exec(c.Context(), `
			INSERT INTO memberships (id, period_id, player_id, commitment_fee, status, joined_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 'ACTIVE', now())
			ON CONFLICT (period_id, player_id) DO UPDATE
			SET status = 'ACTIVE', commitment_fee = EXCLUDED.commitment_fee, joined_at = now()`,
			periodID, playerID, baseAmount)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menambahkan pemain sebagai member.")
		}

		// 3. Record revenue so commitment fee is settled (LUNAS)
		var revenueDate time.Time
		if paidAt != nil && *paidAt != "" {
			revenueDate, _ = time.Parse("2006-01-02", *paidAt)
		}
		if revenueDate.IsZero() {
			revenueDate = time.Now()
		}

		_, err = tx.Exec(c.Context(), `
			INSERT INTO revenues (id, period_id, source, player_id, amount, note, occurred_at)
			VALUES (gen_random_uuid(), $1, 'COMMITMENT_FEE', $2, $3, 'Pembayaran komitmen member online', $4)`,
			periodID, playerID, totalAmount, revenueDate)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal mencatat penerimaan kas.")
		}

		// 4. Auto-fail remaining pending/waiting payments once the final slot is filled
		if fillsLastSlot {
			_, err = tx.Exec(c.Context(), `
				UPDATE membership_payments
				SET status = 'FAILED',
				    verified_by = $2,
				    verified_at = now(),
				    rejection_reason = 'Kuota member untuk periode ini sudah penuh.',
				    updated_at = now()
				WHERE period_id = $1
				  AND id <> $3
				  AND status IN ('WAITING_PAYMENT', 'PENDING')`,
				periodID, adminUserID, paymentID)
			if err != nil {
				return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal memperbarui sisa antrean pembayaran.")
			}
		}
	} else {
		// FAILED
		_, err = tx.Exec(c.Context(), `
			UPDATE membership_payments
			SET status = 'FAILED',
			    verified_by = $2,
			    verified_at = now(),
			    rejection_reason = NULLIF($3, ''),
			    updated_at = now()
			WHERE id = $1`, paymentID, adminUserID, in.RejectionReason)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal memperbarui status pembayaran.")
		}
	}

	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menyelesaikan verifikasi pembayaran.")
	}

	// Query and return the updated payment details
	var mp MembershipPayment
	var expTime time.Time
	err = s.db.QueryRow(c.Context(), `
		SELECT mp.id::text, mp.period_id::text, pr.name, mp.player_id::text, pl.name, pl.phone,
		       mp.base_amount, mp.unique_code, mp.total_amount, mp.status, mp.expires_at,
		       mp.paid_at::text, mp.proof_url, mp.note, mp.rejection_reason,
		       mp.created_at::text, mp.updated_at::text
		FROM membership_payments mp
		JOIN membership_periods pr ON pr.id = mp.period_id
		JOIN players pl ON pl.id = mp.player_id
		WHERE mp.id = $1`, paymentID).
		Scan(&mp.ID, &mp.PeriodID, &mp.PeriodName, &mp.PlayerID, &mp.PlayerName, &mp.PlayerPhone,
			&mp.BaseAmount, &mp.UniqueCode, &mp.TotalAmount, &mp.Status, &expTime,
			&mp.PaidAt, &mp.ProofURL, &mp.Note, &mp.RejectionReason, &mp.CreatedAt, &mp.UpdatedAt)
	if err != nil {
		return httpx.NotFound("Pembayaran tidak ditemukan.")
	}

	mp.ExpiresAt = expTime.Format(time.RFC3339)
	rem := int64(time.Until(expTime).Seconds())
	if rem < 0 {
		rem = 0
	}
	mp.RemainingSecs = rem

	return httpx.OK(c, http.StatusOK, mp)
}

// DeletePayment lets admin delete a membership payment record regardless of its status.
// If the payment was already PAID, it atomically deletes the membership and commitment fee revenue.
func (s *Service) DeletePayment(c *fiber.Ctx) error {
	periodID := c.Params("id")
	paymentID := c.Params("paymentId")
	if _, err := uuid.Parse(periodID); err != nil || uuid.Validate(paymentID) != nil {
		return httpx.BadRequest("BAD_REQUEST", "Invalid ID.")
	}

	if err := s.guardImmutable(c.Context(), periodID); err != nil {
		return httpx.WriteAppError(c, err)
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not start transaction.")
	}
	defer tx.Rollback(c.Context())

	var playerID string
	var status string
	var proofPath, proofURL *string
	err = tx.QueryRow(c.Context(), `
		SELECT player_id::text, status, proof_path, proof_url
		FROM membership_payments
		WHERE id = $1 AND period_id = $2
		FOR UPDATE`, paymentID, periodID).
		Scan(&playerID, &status, &proofPath, &proofURL)
	if err != nil {
		return httpx.NotFound("Pembayaran tidak ditemukan.")
	}

	if status == "PAID" {
		// 1. Delete revenue recorded for commitment fee
		_, err = tx.Exec(c.Context(), `
			DELETE FROM revenues
			WHERE period_id = $1 AND player_id = $2 AND source = 'COMMITMENT_FEE'`,
			periodID, playerID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menghapus catatan penerimaan kas.")
		}

		// 2. Delete membership
		_, err = tx.Exec(c.Context(), `
			DELETE FROM memberships
			WHERE period_id = $1 AND player_id = $2`,
			periodID, playerID)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menghapus status keanggotaan.")
		}
	}

	// 3. Delete membership payment record
	tag, err := tx.Exec(c.Context(), `
		DELETE FROM membership_payments
		WHERE id = $1 AND period_id = $2`,
		paymentID, periodID)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menghapus data pembayaran.")
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("Pembayaran tidak ditemukan.")
	}

	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gagal menyelesaikan penghapusan pembayaran.")
	}

	// 4. Delete payment proof asset from storage (S3 Supabase / local)
	var assetKey string
	if proofPath != nil && *proofPath != "" {
		assetKey = *proofPath
	} else if proofURL != nil && *proofURL != "" {
		assetKey = *proofURL
	}
	if assetKey != "" && s.storage != nil {
		if err := s.storage.Delete(context.Background(), assetKey); err != nil {
			log.Printf("[period] warning: failed to delete payment proof asset from storage (%s): %v", assetKey, err)
		}
	}

	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true, "id": paymentID})
}

func evaluateQuotaForApproval(maxMembers *int, otherMemberCount int) (canApprove bool, fillsLastSlot bool) {
	if maxMembers == nil || *maxMembers <= 0 {
		return true, false
	}
	if otherMemberCount >= *maxMembers {
		return false, false
	}
	return true, otherMemberCount+1 >= *maxMembers
}

