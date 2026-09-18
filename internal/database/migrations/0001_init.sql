CREATE TABLE players (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    phone TEXT,
    notes TEXT,
    status TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_players_name ON players (name);
CREATE INDEX idx_players_phone ON players (phone);

CREATE TABLE users (
    id UUID PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'ADMIN',
    player_id UUID REFERENCES players(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE venues (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    address TEXT,
    notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE courts (
    id UUID PRIMARY KEY,
    venue_id UUID NOT NULL REFERENCES venues(id),
    name TEXT NOT NULL,
    notes TEXT
);

CREATE TABLE membership_periods (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    number_of_sessions INT NOT NULL DEFAULT 0,
    max_members INT,
    commitment_fee BIGINT NOT NULL DEFAULT 0,
    member_contribution BIGINT NOT NULL DEFAULT 0,
    non_member_fee BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'DRAFT',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    id UUID PRIMARY KEY,
    period_id UUID NOT NULL REFERENCES membership_periods(id),
    player_id UUID NOT NULL REFERENCES players(id),
    commitment_fee BIGINT NOT NULL DEFAULT 0,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL DEFAULT 'ACTIVE',
    UNIQUE (period_id, player_id)
);

CREATE TABLE mabar_sessions (
    id UUID PRIMARY KEY,
    type TEXT NOT NULL,
    period_id UUID REFERENCES membership_periods(id),
    venue_id UUID REFERENCES venues(id),
    date DATE NOT NULL,
    start_time TIME,
    end_time TIME,
    duration_minutes INT,
    description TEXT,
    venue_description TEXT,
    court_cost BIGINT NOT NULL DEFAULT 0,
    pricing_mode TEXT,
    shuttlecock_price BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'SCHEDULED',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_mabar_sessions_date ON mabar_sessions (date);

CREATE TABLE attendances (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES mabar_sessions(id),
    player_id UUID NOT NULL REFERENCES players(id),
    status TEXT NOT NULL DEFAULT 'LISTED',
    is_member BOOLEAN NOT NULL DEFAULT FALSE,
    replacement_for_player_id UUID REFERENCES players(id),
    listed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    cancelled_at TIMESTAMPTZ,
    no_show_reason TEXT,
    UNIQUE (session_id, player_id)
);
CREATE INDEX idx_attendances_player ON attendances (player_id);

CREATE TABLE matches (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES mabar_sessions(id),
    court_id UUID REFERENCES courts(id),
    sequence INT NOT NULL,
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    shuttlecock_used INT NOT NULL DEFAULT 0,
    UNIQUE (session_id, sequence)
);

CREATE TABLE match_players (
    match_id UUID NOT NULL REFERENCES matches(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id),
    team INT,
    PRIMARY KEY (match_id, player_id)
);

CREATE TABLE shuttlecock_products (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    unit_name TEXT NOT NULL DEFAULT 'pc',
    units_per_pack INT NOT NULL DEFAULT 12,
    purchase_price BIGINT NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE shuttlecock_transactions (
    id UUID PRIMARY KEY,
    product_id UUID NOT NULL REFERENCES shuttlecock_products(id),
    type TEXT NOT NULL,
    units INT NOT NULL,
    unit_price BIGINT,
    session_id UUID REFERENCES mabar_sessions(id),
    match_id UUID REFERENCES matches(id),
    note TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_shuttlecock_tx_product ON shuttlecock_transactions (product_id);

CREATE TABLE revenues (
    id UUID PRIMARY KEY,
    session_id UUID REFERENCES mabar_sessions(id),
    period_id UUID REFERENCES membership_periods(id),
    source TEXT NOT NULL,
    player_id UUID REFERENCES players(id),
    amount BIGINT NOT NULL,
    note TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE expenses (
    id UUID PRIMARY KEY,
    session_id UUID REFERENCES mabar_sessions(id),
    period_id UUID REFERENCES membership_periods(id),
    category TEXT NOT NULL,
    amount BIGINT NOT NULL,
    note TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE player_bills (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES mabar_sessions(id),
    player_id UUID NOT NULL REFERENCES players(id),
    court_share BIGINT NOT NULL DEFAULT 0,
    shuttlecock_count INT NOT NULL DEFAULT 0,
    shuttlecock_contribution BIGINT NOT NULL DEFAULT 0,
    other_charge BIGINT NOT NULL DEFAULT 0,
    total BIGINT NOT NULL DEFAULT 0,
    payment_status TEXT NOT NULL DEFAULT 'UNPAID',
    UNIQUE (session_id, player_id)
);

CREATE TABLE payments (
    id UUID PRIMARY KEY,
    bill_id UUID REFERENCES player_bills(id),
    player_id UUID NOT NULL REFERENCES players(id),
    amount BIGINT NOT NULL,
    method TEXT,
    paid_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_logs (
    id UUID PRIMARY KEY,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    before JSONB,
    after JSONB,
    ip TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO shuttlecock_products (id, name, unit_name, units_per_pack, purchase_price)
VALUES (gen_random_uuid(), 'Default Tube 12', 'pc', 12, 125000);
