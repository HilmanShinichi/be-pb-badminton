-- Membership registration payments with unique code and S3 proof upload
ALTER TABLE membership_periods
    ADD COLUMN IF NOT EXISTS is_registration_open BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS membership_payments (
    id UUID PRIMARY KEY,
    period_id UUID NOT NULL REFERENCES membership_periods(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    base_amount BIGINT NOT NULL,
    unique_code INT NOT NULL,
    total_amount BIGINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'WAITING_PAYMENT',
    expires_at TIMESTAMPTZ NOT NULL,
    paid_at DATE,
    proof_url TEXT,
    proof_path TEXT,
    note TEXT,
    verified_by UUID REFERENCES users(id) ON DELETE SET NULL,
    verified_at TIMESTAMPTZ,
    rejection_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_membership_payment_status CHECK (status IN ('WAITING_PAYMENT', 'PENDING', 'PAID', 'FAILED')),
    CONSTRAINT chk_membership_payment_code CHECK (unique_code >= 1 AND unique_code <= 999)
);

CREATE INDEX IF NOT EXISTS idx_membership_payments_period ON membership_payments (period_id);
CREATE INDEX IF NOT EXISTS idx_membership_payments_player ON membership_payments (player_id);
CREATE INDEX IF NOT EXISTS idx_membership_payments_status ON membership_payments (status);
CREATE INDEX IF NOT EXISTS idx_membership_payments_expires ON membership_payments (expires_at) WHERE status = 'WAITING_PAYMENT';

-- Prevent concurrent active payments for the same player in a period
CREATE UNIQUE INDEX IF NOT EXISTS uq_membership_payments_active_player
    ON membership_payments (period_id, player_id)
    WHERE status IN ('WAITING_PAYMENT', 'PENDING');

-- Prevent concurrent identical unique codes for active payments in a period
CREATE UNIQUE INDEX IF NOT EXISTS uq_membership_payments_active_code
    ON membership_payments (period_id, unique_code)
    WHERE status IN ('WAITING_PAYMENT', 'PENDING');

-- Ensure Supabase storage bucket exists if storage schema is available
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = 'storage') THEN
        INSERT INTO storage.buckets (id, name, public)
        VALUES ('payment-proofs', 'payment-proofs', true)
        ON CONFLICT (id) DO NOTHING;
    END IF;
END $$;
