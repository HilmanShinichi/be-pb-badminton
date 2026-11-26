package openplay

import "time"

type ParticipantType string
type ParticipantStatus string
type CancellationStatus string

const (
	ParticipantTypeMember    ParticipantType = "MEMBER"
	ParticipantTypeNonMember ParticipantType = "NON_MEMBER"

	ParticipantStatusRegistered ParticipantStatus = "REGISTERED"
	ParticipantStatusWaiting    ParticipantStatus = "WAITING"
	ParticipantStatusCancelled  ParticipantStatus = "CANCELLED"
	ParticipantStatusAutoFilled ParticipantStatus = "AUTO_FILLED"

	CancellationStatusPending  CancellationStatus = "PENDING"
	CancellationStatusApproved CancellationStatus = "APPROVED"
	CancellationStatusRejected CancellationStatus = "REJECTED"
)

// OpenPlaySession represents an open play event
type OpenPlaySession struct {
	ID                       string     `json:"id"`
	Name                     string     `json:"name"`
	Description              *string    `json:"description"`
	ScheduleDate             string     `json:"schedule_date"` // YYYY-MM-DD
	StartTime                string     `json:"start_time"`    // HH:MM
	EndTime                  *string    `json:"end_time"`      // HH:MM
	VenueID                  *string    `json:"venue_id"`
	MaxMembers               int        `json:"max_members"`
	MaxNonMembers            int        `json:"max_non_members"`
	MaxWaitingList           int        `json:"max_waiting_list"`
	CancellationWindowHours  int        `json:"cancellation_window_hours"`
	MinNonMemberFee          int64      `json:"min_non_member_fee"`
	Status                   string     `json:"status"` // OPEN, CLOSED, CANCELLED
	CreatedBy                *string    `json:"created_by"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	RegisteredMemberCount    int        `json:"registered_member_count,omitempty"`
	RegisteredNonMemberCount int        `json:"registered_non_member_count,omitempty"`
	WaitingListCount         int        `json:"waiting_list_count,omitempty"`
}

// Participant represents a player joining an open play session
type Participant struct {
	ID                   string            `json:"id"`
	SessionID            string            `json:"session_id"`
	PlayerID             string            `json:"player_id"`
	PlayerName           string            `json:"player_name"`
	ParticipantType      ParticipantType   `json:"participant_type"` // MEMBER or NON_MEMBER
	ParticipantStatus    ParticipantStatus `json:"participant_status"`
	JoinedAt             time.Time         `json:"joined_at"`
	FilledFromWaitingAt  *time.Time        `json:"filled_from_waiting_at"`
}

// CancellationRequest represents a request to cancel participation
type CancellationRequest struct {
	ID             string             `json:"id"`
	ParticipantID  string             `json:"participant_id"`
	SessionID      string             `json:"session_id"`
	PlayerID       string             `json:"player_id"`
	PlayerName     string             `json:"player_name"`
	Reason         string             `json:"reason"`
	Status         CancellationStatus `json:"request_status"`
	ReviewedBy     *string            `json:"reviewed_by"`
	ReviewedByName *string            `json:"reviewed_by_name"`
	ReviewedAt     *time.Time         `json:"reviewed_at"`
	ReviewNotes    *string            `json:"review_notes"`
	RequestedAt    time.Time          `json:"requested_at"`
	CreatedAt      time.Time          `json:"created_at"`
}

// DTOs for API requests

type CreateSessionRequest struct {
	Name                    string  `json:"name"`
	Description             *string `json:"description"`
	ScheduleDate            string  `json:"schedule_date"` // YYYY-MM-DD
	StartTime               string  `json:"start_time"`    // HH:MM
	EndTime                 *string `json:"end_time"`      // HH:MM
	VenueID                 *string `json:"venue_id"`
	MaxMembers              int     `json:"max_members"`
	MaxNonMembers           int     `json:"max_non_members"`
	MaxWaitingList          int     `json:"max_waiting_list"`
	CancellationWindowHours int     `json:"cancellation_window_hours"`
	MinNonMemberFee         int64   `json:"min_non_member_fee"`
}

type UpdateSessionRequest struct {
	Name                    *string `json:"name"`
	Description             *string `json:"description"`
	ScheduleDate            *string `json:"schedule_date"`
	StartTime               *string `json:"start_time"`
	EndTime                 *string `json:"end_time"`
	VenueID                 *string `json:"venue_id"`
	MaxMembers              *int    `json:"max_members"`
	MaxNonMembers           *int    `json:"max_non_members"`
	MaxWaitingList          *int    `json:"max_waiting_list"`
	CancellationWindowHours *int    `json:"cancellation_window_hours"`
	MinNonMemberFee         *int64  `json:"min_non_member_fee"`
	Status                  *string `json:"status"`
}

type JoinSessionRequest struct {
	PlayerID        string          `json:"player_id"`
	ParticipantType ParticipantType `json:"participant_type"` // MEMBER or NON_MEMBER
}

type CancelParticipationRequest struct {
	Reason string `json:"reason"`
}

type ApproveCancellationRequest struct {
	AdminNotes *string `json:"admin_notes"`
}

// Responses

type SessionResponse struct {
	Session               *OpenPlaySession `json:"session"`
	RegisteredMembers     []*Participant   `json:"registered_members"`
	RegisteredNonMembers  []*Participant   `json:"registered_non_members"`
	WaitingList           []*Participant   `json:"waiting_list"`
	PendingCancellations  []*CancellationRequest `json:"pending_cancellations,omitempty"`
}

type ParticipantListResponse struct {
	Members     []*Participant `json:"members"`
	NonMembers  []*Participant `json:"non_members"`
	WaitingList []*Participant `json:"waiting_list"`
}

type CancellationRequestDetail struct {
	ID            string             `json:"id"`
	ParticipantID string             `json:"participant_id"`
	SessionID     string             `json:"session_id"`
	SessionName   string             `json:"session_name"`
	PlayerName    string             `json:"player_name"`
	Reason        string             `json:"reason"`
	Status        CancellationStatus `json:"request_status"`
	AdminNotes    *string            `json:"admin_notes"`
	RequestedAt   time.Time          `json:"requested_at"`
	CreatedAt     time.Time          `json:"created_at"`
}

type RejectCancellationRequest struct {
	AdminNotes *string `json:"admin_notes"`
}
