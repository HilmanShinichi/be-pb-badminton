-- Open Play Sessions: Independent sessions for open play registration
-- Not tied to membership_periods; can be created anytime
CREATE TABLE open_play_sessions (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    schedule_date DATE NOT NULL,
    start_time TIME NOT NULL,
    end_time TIME,
    venue_id UUID REFERENCES venues(id),
    max_members INT NOT NULL DEFAULT 20,
    max_non_members INT NOT NULL DEFAULT 18,
    max_waiting_list INT NOT NULL DEFAULT 3,
    cancellation_window_hours INT NOT NULL DEFAULT 24,
    min_non_member_fee BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'OPEN',
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_open_play_sessions_date ON open_play_sessions (schedule_date);
CREATE INDEX idx_open_play_sessions_status ON open_play_sessions (status);

-- Participant types
CREATE TYPE participant_type AS ENUM ('MEMBER', 'NON_MEMBER');
CREATE TYPE participant_status AS ENUM ('REGISTERED', 'WAITING', 'CANCELLED', 'AUTO_FILLED');

-- Open Play Participants: Track who joined
CREATE TABLE open_play_participants (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES open_play_sessions(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id),
    participant_type participant_type NOT NULL,
    participant_status participant_status NOT NULL DEFAULT 'REGISTERED',
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    filled_from_waiting_at TIMESTAMPTZ,
    UNIQUE (session_id, player_id)
);

CREATE INDEX idx_open_play_participants_session ON open_play_participants (session_id);
CREATE INDEX idx_open_play_participants_player ON open_play_participants (player_id);
CREATE INDEX idx_open_play_participants_status ON open_play_participants (participant_status);

-- Cancellation Requests: Track cancellations with reasons
CREATE TYPE cancellation_status AS ENUM ('PENDING', 'APPROVED', 'REJECTED');

CREATE TABLE open_play_cancellations (
    id UUID PRIMARY KEY,
    participant_id UUID NOT NULL REFERENCES open_play_participants(id) ON DELETE CASCADE,
    session_id UUID NOT NULL REFERENCES open_play_sessions(id),
    reason TEXT NOT NULL,
    request_status cancellation_status NOT NULL DEFAULT 'PENDING',
    reviewed_by UUID REFERENCES users(id),
    reviewed_at TIMESTAMPTZ,
    review_notes TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_open_play_cancellations_session ON open_play_cancellations (session_id);
CREATE INDEX idx_open_play_cancellations_status ON open_play_cancellations (request_status);
