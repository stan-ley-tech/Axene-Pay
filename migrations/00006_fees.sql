-- Fee schedules, fee collection terms, Axene fees (ADR-0011), and the
-- operator audit log.

-- +goose Up

-- Exclusion constraints over (scope, time range) need btree_gist; it is a
-- trusted extension, so the database owner may create it.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE fee_schedules (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id            text        NOT NULL UNIQUE CHECK (public_id ~ '^fsc_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- NULL: platform default applying to merchants without their own schedule.
    merchant_id          uuid        REFERENCES merchants (id),
    environment          text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    method               text        NOT NULL CHECK (method IN ('mpesa')),
    -- NULL: any provider.
    provider             text        CHECK (provider ~ '^[a-z][a-z0-9_]{1,40}$'),
    currency             char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    percentage_ppm       integer     NOT NULL CHECK (percentage_ppm BETWEEN 0 AND 1000000),
    fixed_minor          bigint      NOT NULL CHECK (fixed_minor >= 0),
    minimum_minor        bigint      CHECK (minimum_minor >= 0),
    maximum_minor        bigint      CHECK (maximum_minor >= 0),
    rounding             text        NOT NULL CHECK (rounding IN ('half_up', 'half_even', 'up', 'down')),
    fee_payer            text        NOT NULL CHECK (fee_payer IN ('merchant', 'customer')),
    provider_fee_bearer  text        NOT NULL CHECK (provider_fee_bearer IN ('merchant', 'axene', 'customer')),
    reversal_policy      text        NOT NULL CHECK (reversal_policy IN ('refund_fee', 'retain_fee', 'pending_approval')),
    effective_from       timestamptz NOT NULL,
    effective_to         timestamptz,
    status               text        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'retired')),
    description          text        NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    -- Activation records who approved the terms and where (contract, memo).
    approved_by          text,
    approval_reference   text,
    activated_at         timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (minimum_minor IS NULL OR maximum_minor IS NULL OR minimum_minor <= maximum_minor),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    CHECK (status = 'draft' OR (approved_by IS NOT NULL AND approval_reference IS NOT NULL AND activated_at IS NOT NULL)),
    -- Two active schedules may not cover the same scope at the same time.
    EXCLUDE USING gist (
        (COALESCE(merchant_id::text, '')) WITH =,
        environment WITH =, method WITH =, (COALESCE(provider, '')) WITH =, currency WITH =,
        tstzrange(effective_from, effective_to) WITH &&
    ) WHERE (status = 'active')
);
CREATE INDEX fee_schedules_lookup_idx ON fee_schedules (merchant_id, environment, method, currency) WHERE status = 'active';

-- Pricing terms are immutable once a schedule leaves draft: corrections are
-- new versions. Only retirement (status and effective_to) may change.
-- +goose StatementBegin
CREATE FUNCTION fee_schedule_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'fee schedule % is % and cannot be deleted', OLD.public_id, OLD.status
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.status <> 'draft' AND (
        NEW.merchant_id IS DISTINCT FROM OLD.merchant_id OR NEW.environment <> OLD.environment OR
        NEW.method <> OLD.method OR NEW.provider IS DISTINCT FROM OLD.provider OR NEW.currency <> OLD.currency OR
        NEW.percentage_ppm <> OLD.percentage_ppm OR NEW.fixed_minor <> OLD.fixed_minor OR
        NEW.minimum_minor IS DISTINCT FROM OLD.minimum_minor OR NEW.maximum_minor IS DISTINCT FROM OLD.maximum_minor OR
        NEW.rounding <> OLD.rounding OR NEW.fee_payer <> OLD.fee_payer OR
        NEW.provider_fee_bearer <> OLD.provider_fee_bearer OR NEW.reversal_policy <> OLD.reversal_policy OR
        NEW.effective_from <> OLD.effective_from OR NEW.approved_by IS DISTINCT FROM OLD.approved_by OR
        NEW.approval_reference IS DISTINCT FROM OLD.approval_reference OR NEW.activated_at IS DISTINCT FROM OLD.activated_at
    ) THEN
        RAISE EXCEPTION 'fee schedule % is % and its terms cannot change', OLD.public_id, OLD.status
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.status = 'retired' AND NEW.status <> 'retired' THEN
        RAISE EXCEPTION 'retired fee schedule % cannot be reactivated', OLD.public_id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER fee_schedules_immutable BEFORE UPDATE OR DELETE ON fee_schedules
    FOR EACH ROW EXECUTE FUNCTION fee_schedule_immutable();

-- How Axene collects its fees from a merchant. Only mechanisms whose funds
-- flow and provider capability are confirmed may be added here (by
-- migration and ADR); 'simulated' is for development and is refused by the
-- application in production.
CREATE TABLE fee_collection_terms (
    merchant_id  uuid        NOT NULL REFERENCES merchants (id),
    environment  text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    mechanism    text        NOT NULL DEFAULT 'none_configured' CHECK (mechanism IN ('none_configured', 'simulated')),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, environment)
);

CREATE TABLE axene_fees (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id              text        NOT NULL UNIQUE CHECK (public_id ~ '^fee_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    -- One fee per payment: the financial effect of a success happens once.
    payment_id             uuid        NOT NULL UNIQUE REFERENCES payments (id),
    merchant_id            uuid        NOT NULL REFERENCES merchants (id),
    environment            text        NOT NULL CHECK (environment IN ('sandbox', 'live')),
    schedule_id            uuid        NOT NULL REFERENCES fee_schedules (id),
    currency               char(3)     NOT NULL,
    base_amount_minor      bigint      NOT NULL CHECK (base_amount_minor > 0),
    percentage_minor       bigint      NOT NULL CHECK (percentage_minor >= 0),
    fixed_minor            bigint      NOT NULL CHECK (fixed_minor >= 0),
    fee_minor              bigint      NOT NULL CHECK (fee_minor >= 0),
    fee_payer              text        NOT NULL,
    provider_fee_bearer    text        NOT NULL,
    reversal_policy        text        NOT NULL,
    status                 text        NOT NULL DEFAULT 'accrued' CHECK (status IN (
                               'accrued', 'collection_pending', 'collected', 'collection_failed',
                               'waived', 'refunded', 'review_required')),
    collection_mechanism   text,
    collection_reference   text,
    collection_evidence    text,
    collected_at           timestamptz,
    next_collection_at     timestamptz,
    collection_attempts    integer     NOT NULL DEFAULT 0 CHECK (collection_attempts >= 0),
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CHECK (fee_minor <= base_amount_minor),
    -- Collected requires evidence; evidence stays on the record if a later
    -- reversal sends the fee to review.
    CHECK (status <> 'collected' OR (collection_reference IS NOT NULL AND collection_evidence IS NOT NULL AND collected_at IS NOT NULL))
);
CREATE INDEX axene_fees_merchant_idx ON axene_fees (merchant_id, environment, created_at);
CREATE INDEX axene_fees_due_idx ON axene_fees (next_collection_at) WHERE next_collection_at IS NOT NULL;

CREATE TABLE fee_transitions (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fee_id       uuid        NOT NULL REFERENCES axene_fees (id),
    from_status  text,
    to_status    text        NOT NULL,
    actor        text        NOT NULL,
    reason       text,
    evidence     text,
    request_id   text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX fee_transitions_fee_idx ON fee_transitions (fee_id, id);
CREATE TRIGGER fee_transitions_append_only BEFORE UPDATE OR DELETE ON fee_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Financial exception types for fees.
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_type_check;
ALTER TABLE reconciliation_exceptions ADD CONSTRAINT reconciliation_exceptions_type_check CHECK (type IN (
    'verification_exhausted', 'evidence_conflict', 'evidence_mismatch', 'late_callback_conflict', 'unmatched_callback',
    'fee_schedule_missing', 'fee_policy_required'));
-- Payment-level exceptions (no attempt or callback) are unique per type.
CREATE UNIQUE INDEX recon_exceptions_payment_idx ON reconciliation_exceptions (type, payment_id)
    WHERE attempt_id IS NULL AND callback_id IS NULL AND payment_id IS NOT NULL;

-- Who did what, when: every operator action that changes financial or
-- configuration state.
CREATE TABLE audit_log (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor        text        NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    action       text        NOT NULL,
    target_type  text        NOT NULL,
    target_id    text        NOT NULL,
    detail       jsonb       NOT NULL DEFAULT '{}',
    request_id   text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id, id);
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose Down
DROP TABLE audit_log;
DROP INDEX recon_exceptions_payment_idx;
ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_type_check;
ALTER TABLE reconciliation_exceptions ADD CONSTRAINT reconciliation_exceptions_type_check CHECK (type IN (
    'verification_exhausted', 'evidence_conflict', 'evidence_mismatch', 'late_callback_conflict', 'unmatched_callback'));
DROP TABLE fee_transitions;
DROP TABLE axene_fees;
DROP TABLE fee_collection_terms;
DROP TABLE fee_schedules;
DROP FUNCTION fee_schedule_immutable();
