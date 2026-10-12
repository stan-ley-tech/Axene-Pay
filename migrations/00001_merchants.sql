-- Merchants, settlement destinations (ADR-0008), provider configuration, API keys.

-- +goose Up

-- Rejects UPDATE and DELETE on tables that are append-only by design.
-- +goose StatementBegin
CREATE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on append-only table % is not allowed', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TABLE merchants (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id   text        NOT NULL UNIQUE CHECK (public_id ~ '^mer_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    -- D-03: live API keys only for approved merchants.
    status      text        NOT NULL DEFAULT 'pending_review'
                            CHECK (status IN ('pending_review', 'approved', 'suspended')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Where a provider settles a merchant's funds. Under ADR-0008 the owner is
-- always the merchant; 'axene' is reserved for a future centralized model
-- and deliberately not allowed yet.
CREATE TABLE settlement_destinations (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id    text        NOT NULL UNIQUE CHECK (public_id ~ '^dst_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    merchant_id  uuid        NOT NULL REFERENCES merchants (id),
    environment  text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    owner        text        NOT NULL DEFAULT 'merchant' CHECK (owner IN ('merchant')),
    type         text        NOT NULL CHECK (type IN ('mpesa_paybill', 'mpesa_till')),
    identifier   text        NOT NULL CHECK (identifier ~ '^[0-9]{1,20}$'),
    label        text        NOT NULL DEFAULT '' CHECK (length(label) <= 200),
    status       text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, environment, type, identifier),
    -- Target for composite foreign keys that guarantee a destination belongs
    -- to the same merchant as the row referencing it.
    UNIQUE (id, merchant_id, environment)
);

-- Which provider may collect which method for a merchant, and into which
-- destination. Credentials are never stored here: they live in the secret
-- store and are referenced by provider configuration at runtime.
CREATE TABLE merchant_provider_configs (
    id                         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id                  text        NOT NULL UNIQUE CHECK (public_id ~ '^pcf_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    merchant_id                uuid        NOT NULL REFERENCES merchants (id),
    environment                text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    method                     text        NOT NULL CHECK (method IN ('mpesa')),
    provider                   text        NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{1,40}$'),
    settlement_destination_id  uuid        NOT NULL,
    -- Lower runs first. Routing is deterministic: priority, then provider name.
    priority                   integer     NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 10000),
    enabled                    boolean     NOT NULL DEFAULT true,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, environment, method, provider),
    FOREIGN KEY (settlement_destination_id, merchant_id, environment)
        REFERENCES settlement_destinations (id, merchant_id, environment)
);

CREATE TABLE api_keys (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id     text        NOT NULL UNIQUE CHECK (public_id ~ '^key_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    merchant_id   uuid        NOT NULL REFERENCES merchants (id),
    environment   text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    -- Non-secret prefix shown to humans, e.g. 'sk_test_4F9Q'.
    display_hint  text        NOT NULL,
    -- HMAC-SHA-256(pepper, secret). The secret itself is never stored.
    secret_hash   bytea       NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz,
    last_used_at  timestamptz
);
CREATE INDEX api_keys_merchant_idx ON api_keys (merchant_id);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE merchant_provider_configs;
DROP TABLE settlement_destinations;
DROP TABLE merchants;
DROP FUNCTION forbid_mutation();
