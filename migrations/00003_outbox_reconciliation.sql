-- Transactional outbox (ADR-0004) and reconciliation exceptions (ADR-0009).

-- +goose Up

CREATE TABLE outbox_events (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id       text        NOT NULL UNIQUE CHECK (public_id ~ '^evt_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    event_type      text        NOT NULL CHECK (event_type ~ '^[a-z_]+\.[a-z_]+$'),
    aggregate_type  text        NOT NULL,
    aggregate_id    text        NOT NULL,
    -- One event per payment transition: replayed processing cannot emit twice.
    transition_id   bigint      UNIQUE REFERENCES payment_transitions (id),
    payload         jsonb       NOT NULL,
    request_id      text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    publish_attempts integer    NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    last_error      text
);
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (created_at) WHERE published_at IS NULL;

CREATE TABLE reconciliation_exceptions (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id    text        NOT NULL UNIQUE CHECK (public_id ~ '^rex_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    type         text        NOT NULL CHECK (type IN (
                     'verification_exhausted', 'evidence_conflict', 'evidence_mismatch',
                     'late_callback_conflict', 'unmatched_callback')),
    payment_id   uuid        REFERENCES payments (id),
    attempt_id   uuid        REFERENCES payment_attempts (id),
    callback_id  uuid        REFERENCES provider_callbacks (id),
    detail       jsonb       NOT NULL DEFAULT '{}',
    status       text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz,
    resolved_by  text,
    resolution   text,
    CHECK ((status = 'resolved') = (resolved_at IS NOT NULL)),
    CHECK (payment_id IS NOT NULL OR callback_id IS NOT NULL)
);
-- One exception of a type per attempt / per callback, however often the
-- triggering condition is re-detected.
CREATE UNIQUE INDEX recon_exceptions_attempt_idx ON reconciliation_exceptions (type, attempt_id)
    WHERE attempt_id IS NOT NULL AND callback_id IS NULL;
CREATE UNIQUE INDEX recon_exceptions_callback_idx ON reconciliation_exceptions (type, callback_id)
    WHERE callback_id IS NOT NULL;
CREATE INDEX recon_exceptions_open_idx ON reconciliation_exceptions (created_at) WHERE status = 'open';

-- +goose Down
DROP TABLE reconciliation_exceptions;
DROP TABLE outbox_events;
