-- Settlement statements and reconciliation runs (docs/ledger.md §6).

-- +goose Up

CREATE TABLE settlement_batches (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id         text        NOT NULL UNIQUE CHECK (public_id ~ '^stb_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    provider          text        NOT NULL,
    environment       text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    source_reference  text        NOT NULL CHECK (length(source_reference) BETWEEN 1 AND 300),
    -- SHA-256 of the imported file: the same statement cannot be imported twice.
    source_sha256     bytea       NOT NULL CHECK (length(source_sha256) = 32),
    line_count        integer     NOT NULL CHECK (line_count >= 0),
    imported_by       text        NOT NULL,
    imported_at       timestamptz NOT NULL,
    UNIQUE (provider, environment, source_sha256)
);

CREATE TABLE settlement_lines (
    id                         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id                  text        NOT NULL UNIQUE CHECK (public_id ~ '^stl_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    batch_id                   uuid        NOT NULL REFERENCES settlement_batches (id),
    line_number                integer     NOT NULL CHECK (line_number >= 1),
    provider_reference         text,
    receipt                    text,
    destination_identifier     text        NOT NULL,
    currency                   char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    gross_minor                bigint      NOT NULL CHECK (gross_minor > 0),
    provider_fee_minor         bigint      NOT NULL CHECK (provider_fee_minor >= 0),
    net_minor                  bigint      NOT NULL CHECK (net_minor >= 0),
    -- Only when the collection mechanism reports Axene's share.
    axene_fee_collected_minor  bigint      CHECK (axene_fee_collected_minor >= 0),
    settled_at                 timestamptz NOT NULL,
    payment_id                 uuid        REFERENCES payments (id),
    match_status               text        NOT NULL DEFAULT 'pending' CHECK (match_status IN ('pending', 'matched', 'exception')),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    UNIQUE (batch_id, line_number),
    CHECK (provider_reference IS NOT NULL OR receipt IS NOT NULL),
    CHECK (gross_minor = provider_fee_minor + net_minor + COALESCE(axene_fee_collected_minor, 0)),
    CHECK ((match_status = 'matched') = (payment_id IS NOT NULL))
);
CREATE INDEX settlement_lines_pending_idx ON settlement_lines (batch_id) WHERE match_status = 'pending';
CREATE INDEX settlement_lines_reference_idx ON settlement_lines (provider_reference);
CREATE INDEX settlement_lines_receipt_idx ON settlement_lines (receipt);
-- A payment is settled by at most one statement line.
CREATE UNIQUE INDEX settlement_lines_payment_idx ON settlement_lines (payment_id) WHERE payment_id IS NOT NULL;

CREATE TABLE reconciliation_runs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id     text        NOT NULL UNIQUE CHECK (public_id ~ '^rcr_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    provider      text        NOT NULL,
    environment   text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    run_by        text        NOT NULL,
    started_at    timestamptz NOT NULL,
    finished_at   timestamptz,
    lines_matched integer     NOT NULL DEFAULT 0,
    exceptions    integer     NOT NULL DEFAULT 0
);

ALTER TABLE reconciliation_exceptions ADD COLUMN settlement_line_id uuid REFERENCES settlement_lines (id);
ALTER TABLE reconciliation_exceptions ADD COLUMN run_id uuid REFERENCES reconciliation_runs (id);
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_type_check;
ALTER TABLE reconciliation_exceptions ADD CONSTRAINT reconciliation_exceptions_type_check CHECK (type IN (
    'verification_exhausted', 'evidence_conflict', 'evidence_mismatch', 'late_callback_conflict', 'unmatched_callback',
    'fee_schedule_missing', 'fee_policy_required',
    'amount_mismatch', 'status_mismatch', 'unmatched_statement_line', 'duplicate_statement_line',
    'missing_settlement', 'fee_collection_mismatch'));
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_check1;
ALTER TABLE reconciliation_exceptions ADD CONSTRAINT reconciliation_exceptions_subject_check
    CHECK (payment_id IS NOT NULL OR callback_id IS NOT NULL OR settlement_line_id IS NOT NULL);
CREATE UNIQUE INDEX recon_exceptions_line_idx ON reconciliation_exceptions (type, settlement_line_id)
    WHERE settlement_line_id IS NOT NULL;
-- Payment-level uniqueness must not collide with line-level exceptions.
DROP INDEX recon_exceptions_payment_idx;
CREATE UNIQUE INDEX recon_exceptions_payment_idx ON reconciliation_exceptions (type, payment_id)
    WHERE attempt_id IS NULL AND callback_id IS NULL AND settlement_line_id IS NULL AND payment_id IS NOT NULL;

-- +goose Down
DROP INDEX recon_exceptions_payment_idx;
CREATE UNIQUE INDEX recon_exceptions_payment_idx ON reconciliation_exceptions (type, payment_id)
    WHERE attempt_id IS NULL AND callback_id IS NULL AND payment_id IS NOT NULL;
DROP INDEX recon_exceptions_line_idx;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_subject_check;
ALTER TABLE reconciliation_exceptions ADD CONSTRAINT reconciliation_exceptions_check1
    CHECK (payment_id IS NOT NULL OR callback_id IS NOT NULL);
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_type_check;
ALTER TABLE reconciliation_exceptions ADD CONSTRAINT reconciliation_exceptions_type_check CHECK (type IN (
    'verification_exhausted', 'evidence_conflict', 'evidence_mismatch', 'late_callback_conflict', 'unmatched_callback',
    'fee_schedule_missing', 'fee_policy_required'));
ALTER TABLE reconciliation_exceptions DROP COLUMN run_id;
ALTER TABLE reconciliation_exceptions DROP COLUMN settlement_line_id;
DROP TABLE reconciliation_runs;
DROP TABLE settlement_lines;
DROP TABLE settlement_batches;
