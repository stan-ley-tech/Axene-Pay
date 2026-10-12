-- name: InsertPayment :one
INSERT INTO payments
    (public_id, merchant_id, environment, amount_minor, currency, method, phone_number,
     description, merchant_reference, settlement_destination_id)
VALUES (sqlc.arg(public_id), sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(amount_minor),
        sqlc.arg(currency), sqlc.arg(method), sqlc.arg(phone_number), sqlc.arg(description),
        sqlc.narg(merchant_reference), sqlc.arg(settlement_destination_id))
RETURNING *;

-- name: GetPaymentForMerchant :one
-- Merchant-scoped lookup: another merchant's payment is indistinguishable
-- from a missing one.
SELECT * FROM payments
WHERE public_id = sqlc.arg(public_id) AND merchant_id = sqlc.arg(merchant_id)
  AND environment = sqlc.arg(environment);

-- name: GetPaymentByID :one
SELECT * FROM payments WHERE id = sqlc.arg(id);

-- name: LockPayment :one
SELECT * FROM payments WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: UpdatePaymentStatus :execrows
-- Guarded by the expected current status in addition to the row lock.
UPDATE payments
SET status = sqlc.arg(to_status), status_reason = sqlc.narg(status_reason), updated_at = now()
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status);

-- name: GetSettlementDestinationByID :one
SELECT * FROM settlement_destinations WHERE id = sqlc.arg(id);

-- name: InsertAttempt :one
-- created_at comes from the application clock: the worker compares it
-- with that clock when sweeping unsubmitted attempts.
INSERT INTO payment_attempts (public_id, payment_id, provider, routing_reason, created_at)
VALUES (sqlc.arg(public_id), sqlc.arg(payment_id), sqlc.arg(provider), sqlc.arg(routing_reason), sqlc.arg(created_at)::timestamptz)
RETURNING *;

-- name: GetAttempt :one
SELECT * FROM payment_attempts WHERE id = sqlc.arg(id);

-- name: GetAttemptByProviderReference :one
SELECT * FROM payment_attempts
WHERE provider = sqlc.arg(provider) AND provider_reference = sqlc.arg(provider_reference);

-- name: ListAttemptsForPayment :many
SELECT * FROM payment_attempts WHERE payment_id = sqlc.arg(payment_id) ORDER BY created_at, id;

-- name: ClaimAttemptSubmission :execrows
-- Only one process may ever call the provider for an attempt.
UPDATE payment_attempts SET submission_claimed_at = sqlc.arg(now)::timestamptz, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'PENDING_SUBMISSION' AND submission_claimed_at IS NULL;

-- name: UpdateAttemptStatus :execrows
UPDATE payment_attempts
SET status               = sqlc.arg(to_status),
    provider_reference   = COALESCE(sqlc.narg(provider_reference), provider_reference),
    provider_receipt     = COALESCE(sqlc.narg(provider_receipt), provider_receipt),
    last_raw_code        = COALESCE(sqlc.narg(last_raw_code), last_raw_code),
    last_raw_message     = COALESCE(sqlc.narg(last_raw_message), last_raw_message),
    submitted_at         = COALESCE(sqlc.narg(submitted_at), submitted_at),
    resolved_at          = COALESCE(sqlc.narg(resolved_at), resolved_at),
    next_verification_at = sqlc.narg(next_verification_at),
    verification_count   = verification_count + sqlc.arg(verification_increment),
    updated_at           = now()
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status);

-- name: RecordClaimedOutcome :exec
UPDATE payment_attempts
SET claimed_outcome = sqlc.arg(claimed_outcome), claimed_at = sqlc.arg(now)::timestamptz,
    next_verification_at = sqlc.narg(next_verification_at), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: ClaimDueVerifications :many
-- Leases due attempts by pushing next_verification_at forward, so a crashed
-- worker's attempts become due again after the lease.
UPDATE payment_attempts
SET next_verification_at = sqlc.arg(lease_until)::timestamptz, updated_at = now()
WHERE id IN (
    SELECT due.id FROM payment_attempts due
    WHERE due.next_verification_at <= sqlc.arg(now)::timestamptz
    ORDER BY due.next_verification_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: ListUnclaimedSubmissions :many
-- Attempts persisted but never handed to a provider (e.g. the API crashed
-- between commit and submission).
SELECT * FROM payment_attempts
WHERE status = 'PENDING_SUBMISSION' AND submission_claimed_at IS NULL AND created_at < sqlc.arg(created_before)
ORDER BY created_at
LIMIT sqlc.arg(max_rows);

-- name: ListExpiredSubmissionClaims :many
-- Attempts whose submitting process stopped without recording an outcome.
-- The provider may have received them: they become SUBMISSION_UNKNOWN.
SELECT * FROM payment_attempts
WHERE status = 'PENDING_SUBMISSION' AND submission_claimed_at < sqlc.arg(claimed_before)::timestamptz
ORDER BY submission_claimed_at
LIMIT sqlc.arg(max_rows);

-- name: InsertTransition :one
INSERT INTO payment_transitions
    (payment_id, attempt_id, callback_id, from_status, to_status, evidence, actor, reason, request_id)
VALUES (sqlc.arg(payment_id), sqlc.narg(attempt_id), sqlc.narg(callback_id), sqlc.arg(from_status),
        sqlc.arg(to_status), sqlc.arg(evidence), sqlc.arg(actor), sqlc.narg(reason), sqlc.arg(request_id))
RETURNING id, created_at;

-- name: ListTransitionsForPayment :many
SELECT * FROM payment_transitions WHERE payment_id = sqlc.arg(payment_id) ORDER BY id;

-- name: GetPaymentByPublicID :one
-- Unscoped lookup for internal consumers (webhook fan-out). Never use on a
-- merchant-facing path: use GetPaymentForMerchant there.
SELECT * FROM payments WHERE public_id = sqlc.arg(public_id);
