-- Merchant webhooks (ADR-0010).

-- +goose Up

CREATE TABLE webhook_endpoints (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id           text        NOT NULL UNIQUE CHECK (public_id ~ '^whe_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    merchant_id         uuid        NOT NULL REFERENCES merchants (id),
    environment         text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    url                 text        NOT NULL CHECK (length(url) <= 2048 AND url ~ '^https?://'),
    -- Empty means every payment event.
    event_types         text[]      NOT NULL DEFAULT '{}',
    description         text        NOT NULL DEFAULT '' CHECK (length(description) <= 200),
    status              text        NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    -- AES-256-GCM(nonce || ciphertext) of the signing secret; the endpoint's
    -- public_id is the additional authenticated data.
    secret_ciphertext   bytea       NOT NULL,
    secret_key_version  integer     NOT NULL DEFAULT 1 CHECK (secret_key_version >= 1),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id, environment) WHERE status = 'enabled';

-- One row per outbox event once deliveries were created for it, whichever
-- trigger (broker consumer or sweep) got there first.
CREATE TABLE webhook_event_fanouts (
    event_id       uuid        PRIMARY KEY REFERENCES outbox_events (id),
    deliveries     integer     NOT NULL CHECK (deliveries >= 0),
    fanned_out_at  timestamptz NOT NULL
);

CREATE TABLE webhook_deliveries (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id        text        NOT NULL UNIQUE CHECK (public_id ~ '^whd_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    endpoint_id      uuid        NOT NULL REFERENCES webhook_endpoints (id),
    event_id         uuid        NOT NULL REFERENCES outbox_events (id),
    event_public_id  text        NOT NULL,
    event_type       text        NOT NULL,
    -- Exact body sent on every attempt (snapshot taken at fan-out).
    body             bytea       NOT NULL,
    status           text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'dead')),
    attempts         integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  timestamptz,
    last_status_code integer,
    last_error       text,
    delivered_at     timestamptz,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (endpoint_id, event_id),
    CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE next_attempt_at IS NOT NULL;
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id, created_at DESC, id DESC);

CREATE TABLE webhook_delivery_attempts (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    delivery_id   uuid        NOT NULL REFERENCES webhook_deliveries (id),
    attempted_at  timestamptz NOT NULL,
    status_code   integer,
    error         text,
    duration_ms   integer     NOT NULL CHECK (duration_ms >= 0),
    CHECK (status_code IS NOT NULL OR error IS NOT NULL)
);
CREATE INDEX webhook_delivery_attempts_delivery_idx ON webhook_delivery_attempts (delivery_id, id);
CREATE TRIGGER webhook_delivery_attempts_append_only BEFORE UPDATE OR DELETE ON webhook_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose Down
DROP TABLE webhook_delivery_attempts;
DROP TABLE webhook_deliveries;
DROP TABLE webhook_event_fanouts;
DROP TABLE webhook_endpoints;
