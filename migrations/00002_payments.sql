-- Payments, provider attempts, transitions, callbacks, idempotency keys.
-- State values mirror internal/payments/state.go and attempt_state.go.

-- +goose Up

CREATE TABLE idempotency_keys (
    merchant_id          uuid        NOT NULL REFERENCES merchants (id),
    environment          text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    key                  text        NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_method       text        NOT NULL,
    request_path         text        NOT NULL,
    request_fingerprint  bytea       NOT NULL CHECK (length(request_fingerprint) = 32),
    status               text        NOT NULL CHECK (status IN ('in_progress', 'completed')),
    response_status      integer,
    response_body        bytea,
    -- Public ID of the resource the request created, so an in-progress key
    -- orphaned by a crash can be completed from the resource's current state.
    resource_id          text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    expires_at           timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, environment, key),
    CHECK ((status = 'completed') = (response_status IS NOT NULL AND completed_at IS NOT NULL))
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);

CREATE TABLE payments (
    id                         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id                  text        NOT NULL UNIQUE CHECK (public_id ~ '^pay_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    merchant_id                uuid        NOT NULL REFERENCES merchants (id),
    environment                text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    amount_minor               bigint      NOT NULL CHECK (amount_minor > 0),
    currency                   char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    method                     text        NOT NULL CHECK (method IN ('mpesa')),
    -- Personal data: the payer's MSISDN. Never logged unmasked.
    phone_number               text        NOT NULL CHECK (phone_number ~ '^254[17][0-9]{8}$'),
    description                text        NOT NULL DEFAULT '' CHECK (length(description) <= 200),
    merchant_reference         text        CHECK (length(merchant_reference) BETWEEN 1 AND 200),
    -- Fixed at creation (ADR-0008): later configuration changes never rewrite history.
    settlement_destination_id  uuid        NOT NULL,
    status                     text        NOT NULL DEFAULT 'CREATED' CHECK (status IN (
                                   'CREATED', 'PENDING_PROVIDER', 'PENDING_VERIFICATION', 'UNRESOLVED',
                                   'SUCCEEDED', 'FAILED', 'CANCELLED', 'REVERSED')),
    status_reason              text,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (settlement_destination_id, merchant_id, environment)
        REFERENCES settlement_destinations (id, merchant_id, environment)
);
CREATE INDEX payments_merchant_created_idx ON payments (merchant_id, environment, created_at DESC, id DESC);

CREATE TABLE payment_attempts (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id             text        NOT NULL UNIQUE CHECK (public_id ~ '^att_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    payment_id            uuid        NOT NULL REFERENCES payments (id),
    provider              text        NOT NULL,
    routing_reason        text        NOT NULL,
    status                text        NOT NULL DEFAULT 'PENDING_SUBMISSION' CHECK (status IN (
                              'PENDING_SUBMISSION', 'SUBMITTED', 'SUBMISSION_UNKNOWN', 'REJECTED',
                              'CONFIRMED_SUCCESS', 'CONFIRMED_FAILURE', 'UNRESOLVED', 'ABANDONED')),
    provider_reference    text,
    provider_receipt      text,
    -- Outcome claimed by the latest callback: unverified (ADR-0009).
    claimed_outcome       text        CHECK (claimed_outcome IN ('succeeded', 'declined', 'rejected', 'unknown')),
    claimed_at            timestamptz,
    verification_count    integer     NOT NULL DEFAULT 0 CHECK (verification_count >= 0),
    next_verification_at  timestamptz,
    last_raw_code         text,
    last_raw_message      text,
    -- Set when a process takes responsibility for calling the provider. An
    -- attempt is submitted at most once: a claim that expires without a
    -- recorded outcome becomes SUBMISSION_UNKNOWN, never a resubmission.
    submission_claimed_at timestamptz,
    submitted_at          timestamptz,
    resolved_at           timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_attempts_payment_idx ON payment_attempts (payment_id, created_at);
-- At most one unresolved attempt per payment: an uncertain attempt is never
-- retried or failed over while it is open.
CREATE UNIQUE INDEX payment_attempts_one_open_idx ON payment_attempts (payment_id)
    WHERE status IN ('PENDING_SUBMISSION', 'SUBMITTED', 'SUBMISSION_UNKNOWN');
CREATE UNIQUE INDEX payment_attempts_provider_ref_idx ON payment_attempts (provider, provider_reference)
    WHERE provider_reference IS NOT NULL;
-- Worker queues, served by index scans.
CREATE INDEX payment_attempts_due_verification_idx ON payment_attempts (next_verification_at)
    WHERE next_verification_at IS NOT NULL;
CREATE INDEX payment_attempts_pending_submission_idx ON payment_attempts (created_at)
    WHERE status = 'PENDING_SUBMISSION';

-- Inbound provider callbacks, stored before any interpretation. Every
-- receipt is kept; duplicates point at the first receipt.
CREATE TABLE provider_callbacks (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id           text        NOT NULL UNIQUE CHECK (public_id ~ '^cbk_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    provider            text        NOT NULL,
    parse_status        text        NOT NULL CHECK (parse_status IN ('parsed', 'rejected')),
    parse_error         text,
    dedup_key           text,
    provider_reference  text,
    claimed_outcome     text        CHECK (claimed_outcome IN ('succeeded', 'declined', 'rejected', 'unknown')),
    attempt_id          uuid        REFERENCES payment_attempts (id),
    duplicate_of        uuid        REFERENCES provider_callbacks (id),
    raw_body            bytea       NOT NULL,
    request_id          text        NOT NULL,
    received_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX provider_callbacks_dedup_idx ON provider_callbacks (provider, dedup_key)
    WHERE dedup_key IS NOT NULL AND duplicate_of IS NULL;
CREATE INDEX provider_callbacks_attempt_idx ON provider_callbacks (attempt_id);
CREATE TRIGGER provider_callbacks_append_only BEFORE UPDATE OR DELETE ON provider_callbacks
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Every payment state change, with the evidence that justified it.
CREATE TABLE payment_transitions (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id   uuid        NOT NULL REFERENCES payments (id),
    attempt_id   uuid        REFERENCES payment_attempts (id),
    callback_id  uuid        REFERENCES provider_callbacks (id),
    from_status  text        NOT NULL,
    to_status    text        NOT NULL,
    evidence     text        NOT NULL,
    actor        text        NOT NULL,
    reason       text,
    request_id   text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (from_status <> to_status)
);
CREATE INDEX payment_transitions_payment_idx ON payment_transitions (payment_id, id);
CREATE TRIGGER payment_transitions_append_only BEFORE UPDATE OR DELETE ON payment_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose Down
DROP TABLE payment_transitions;
DROP TABLE provider_callbacks;
DROP TABLE payment_attempts;
DROP TABLE payments;
DROP TABLE idempotency_keys;
