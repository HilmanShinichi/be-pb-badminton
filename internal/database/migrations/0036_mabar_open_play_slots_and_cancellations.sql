-- Migration 0036: Add member registration cutoff, slot quotas, and cancellation management to mabar_sessions
-- Allows admin to configure open pendaftaran, closed member, closed pendaftaran, and slot quotas.

ALTER TABLE mabar_sessions
ADD COLUMN IF NOT EXISTS member_registration_closes_at TIMESTAMPTZ,
ADD COLUMN IF NOT EXISTS max_members INT NOT NULL DEFAULT 20,
ADD COLUMN IF NOT EXISTS max_non_members INT NOT NULL DEFAULT 18,
ADD COLUMN IF NOT EXISTS max_waiting_list INT NOT NULL DEFAULT 3;

-- Table for tracking cancellation requests with reasons and admin review
CREATE TABLE IF NOT EXISTS mabar_cancellations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id UUID NOT NULL REFERENCES mabar_sessions(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED')),
    admin_notes TEXT,
    reviewed_by UUID REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mabar_cancellations_session ON mabar_cancellations(session_id);
CREATE INDEX IF NOT EXISTS idx_mabar_cancellations_status ON mabar_cancellations(status);
