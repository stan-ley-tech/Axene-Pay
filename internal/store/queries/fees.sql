-- name: CreateFeeSchedule :one
INSERT INTO fee_schedules
    (public_id, merchant_id, environment, method, provider, currency, percentage_ppm, fixed_minor,
     minimum_minor, maximum_minor, rounding, fee_payer, provider_fee_bearer, reversal_policy,
     effective_from, effective_to, description)
VALUES (sqlc.arg(public_id), sqlc.narg(merchant_id), sqlc.arg(environment), sqlc.arg(method), sqlc.narg(provider),
        sqlc.arg(currency), sqlc.arg(percentage_ppm), sqlc.arg(fixed_minor), sqlc.narg(minimum_minor),
        sqlc.narg(maximum_minor), sqlc.arg(rounding), sqlc.arg(fee_payer), sqlc.arg(provider_fee_bearer),
        sqlc.arg(reversal_policy), sqlc.arg(effective_from)::timestamptz, sqlc.narg(effective_to), sqlc.arg(description))
RETURNING *;

-- name: GetFeeSchedule :one
SELECT * FROM fee_schedules WHERE public_id = sqlc.arg(public_id);

-- name: GetFeeScheduleByID :one
SELECT * FROM fee_schedules WHERE id = sqlc.arg(id);

-- name: ActivateFeeSchedule :execrows
UPDATE fee_schedules
SET status = 'active', approved_by = sqlc.arg(approved_by), approval_reference = sqlc.arg(approval_reference),
    activated_at = sqlc.arg(now)::timestamptz
WHERE public_id = sqlc.arg(public_id) AND status = 'draft';

-- name: EndFeeSchedule :execrows
-- Stops a schedule applying to payments created at or after ends_at. A
-- schedule ended before it started is retired outright.
UPDATE fee_schedules
SET effective_to = sqlc.arg(ends_at)::timestamptz,
    status = CASE WHEN sqlc.arg(ends_at)::timestamptz <= effective_from THEN 'retired' ELSE status END
WHERE public_id = sqlc.arg(public_id) AND status = 'active'
  AND (effective_to IS NULL OR effective_to > sqlc.arg(ends_at)::timestamptz);

-- name: ListFeeSchedules :many
SELECT * FROM fee_schedules
WHERE environment = sqlc.arg(environment)
  AND (sqlc.narg(merchant_id)::uuid IS NULL OR merchant_id = sqlc.narg(merchant_id)::uuid)
ORDER BY created_at, id;

-- name: SelectFeeSchedule :one
-- The most specific active schedule in effect at the given time:
-- merchant beats platform default, a named provider beats "any provider".
SELECT * FROM fee_schedules
WHERE status = 'active'
  AND environment = sqlc.arg(environment) AND method = sqlc.arg(method) AND currency = sqlc.arg(currency)
  AND (merchant_id = sqlc.arg(merchant_id) OR merchant_id IS NULL)
  AND (provider = sqlc.arg(provider) OR provider IS NULL)
  AND effective_from <= sqlc.arg(at)::timestamptz
  AND (effective_to IS NULL OR effective_to > sqlc.arg(at)::timestamptz)
ORDER BY (merchant_id IS NULL), (provider IS NULL)
LIMIT 1;

-- name: GetFeeCollectionMechanism :one
SELECT mechanism FROM fee_collection_terms
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment);

-- name: SetFeeCollectionMechanism :exec
INSERT INTO fee_collection_terms (merchant_id, environment, mechanism)
VALUES (sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(mechanism))
ON CONFLICT (merchant_id, environment) DO UPDATE SET mechanism = EXCLUDED.mechanism, updated_at = now();

-- name: ScheduleAccruedFees :execrows
-- After a mechanism is configured, accrued fees become due for collection.
UPDATE axene_fees SET next_collection_at = sqlc.arg(now)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment)
  AND status = 'accrued' AND next_collection_at IS NULL;

-- name: InsertAxeneFee :one
INSERT INTO axene_fees
    (public_id, payment_id, merchant_id, environment, schedule_id, currency, base_amount_minor, percentage_minor,
     fixed_minor, fee_minor, fee_payer, provider_fee_bearer, reversal_policy, next_collection_at, created_at)
VALUES (sqlc.arg(public_id), sqlc.arg(payment_id), sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(schedule_id),
        sqlc.arg(currency), sqlc.arg(base_amount_minor), sqlc.arg(percentage_minor), sqlc.arg(fixed_minor),
        sqlc.arg(fee_minor), sqlc.arg(fee_payer), sqlc.arg(provider_fee_bearer), sqlc.arg(reversal_policy),
        sqlc.narg(next_collection_at), sqlc.arg(created_at)::timestamptz)
ON CONFLICT (payment_id) DO NOTHING
RETURNING *;

-- name: GetFeeByPaymentID :one
SELECT * FROM axene_fees WHERE payment_id = sqlc.arg(payment_id);

-- name: GetFeeByPublicID :one
SELECT * FROM axene_fees WHERE public_id = sqlc.arg(public_id);

-- name: LockFee :one
SELECT * FROM axene_fees WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: UpdateFeeStatus :execrows
UPDATE axene_fees
SET status               = sqlc.arg(to_status),
    collection_mechanism = COALESCE(sqlc.narg(collection_mechanism), collection_mechanism),
    collection_reference = COALESCE(sqlc.narg(collection_reference), collection_reference),
    collection_evidence  = COALESCE(sqlc.narg(collection_evidence), collection_evidence),
    collected_at         = COALESCE(sqlc.narg(collected_at), collected_at),
    next_collection_at   = sqlc.narg(next_collection_at),
    collection_attempts  = collection_attempts + sqlc.arg(attempt_increment),
    updated_at           = now()
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status);

-- name: InsertFeeTransition :exec
INSERT INTO fee_transitions (fee_id, from_status, to_status, actor, reason, evidence, request_id)
VALUES (sqlc.arg(fee_id), sqlc.narg(from_status), sqlc.arg(to_status), sqlc.arg(actor), sqlc.narg(reason),
        sqlc.narg(evidence), sqlc.arg(request_id));

-- name: ListFeeTransitions :many
SELECT * FROM fee_transitions WHERE fee_id = sqlc.arg(fee_id) ORDER BY id;

-- name: ClaimDueFees :many
UPDATE axene_fees
SET next_collection_at = sqlc.arg(lease_until)::timestamptz, updated_at = now()
WHERE id IN (
    SELECT due.id FROM axene_fees due
    WHERE due.next_collection_at <= sqlc.arg(now)::timestamptz
      AND due.status IN ('accrued', 'collection_pending', 'collection_failed')
    ORDER BY due.next_collection_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FeeTotalsByStatus :many
-- Operational report: fee amounts per status (provisional until reconciled).
SELECT status, currency, count(*)::bigint AS fees, COALESCE(sum(fee_minor), 0)::bigint AS amount_minor
FROM axene_fees
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment)
GROUP BY status, currency
ORDER BY status, currency;

-- name: GetMerchantByID :one
SELECT * FROM merchants WHERE id = sqlc.arg(id);
