-- name: InsertOriginalCallback :one
-- Returns no row when an original with the same dedup key already exists;
-- the caller then records this receipt as a duplicate.
INSERT INTO provider_callbacks
    (public_id, provider, parse_status, parse_error, dedup_key, provider_reference,
     claimed_outcome, attempt_id, raw_body, request_id)
VALUES (sqlc.arg(public_id), sqlc.arg(provider), sqlc.arg(parse_status), sqlc.narg(parse_error),
        sqlc.narg(dedup_key), sqlc.narg(provider_reference), sqlc.narg(claimed_outcome),
        sqlc.narg(attempt_id), sqlc.arg(raw_body), sqlc.arg(request_id))
ON CONFLICT DO NOTHING
RETURNING id, public_id;

-- name: InsertDuplicateCallback :one
INSERT INTO provider_callbacks
    (public_id, provider, parse_status, dedup_key, provider_reference, claimed_outcome,
     attempt_id, duplicate_of, raw_body, request_id)
SELECT sqlc.arg(public_id), sqlc.arg(provider), 'parsed', sqlc.arg(dedup_key)::text,
       sqlc.narg(provider_reference), sqlc.narg(claimed_outcome), sqlc.narg(attempt_id),
       o.id, sqlc.arg(raw_body), sqlc.arg(request_id)
FROM provider_callbacks o
WHERE o.provider = sqlc.arg(provider) AND o.dedup_key = sqlc.arg(dedup_key)::text AND o.duplicate_of IS NULL
RETURNING id, public_id;

-- name: ListCallbacksForAttempt :many
SELECT * FROM provider_callbacks WHERE attempt_id = sqlc.arg(attempt_id) ORDER BY received_at, id;

-- name: InsertOutboxEvent :one
INSERT INTO outbox_events
    (public_id, event_type, aggregate_type, aggregate_id, transition_id, payload, request_id)
VALUES (sqlc.arg(public_id), sqlc.arg(event_type), sqlc.arg(aggregate_type), sqlc.arg(aggregate_id),
        sqlc.narg(transition_id), sqlc.arg(payload), sqlc.arg(request_id))
RETURNING id;

-- name: ClaimUnpublishedEvents :many
SELECT * FROM outbox_events
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT sqlc.arg(max_rows)
FOR UPDATE SKIP LOCKED;

-- name: MarkEventPublished :exec
UPDATE outbox_events SET published_at = now(), publish_attempts = publish_attempts + 1, last_error = NULL
WHERE id = sqlc.arg(id);

-- name: MarkEventPublishFailed :exec
UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id);

-- name: OutboxBacklog :one
SELECT count(*)::bigint AS unpublished,
       COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)::float8 AS oldest_age_seconds
FROM outbox_events WHERE published_at IS NULL;

-- name: ListOutboxEventsForAggregate :many
SELECT * FROM outbox_events WHERE aggregate_id = sqlc.arg(aggregate_id) ORDER BY created_at, id;

-- name: InsertReconException :execrows
-- At most one exception per (type, attempt) or (type, callback): repeated
-- detection of the same condition inserts nothing.
INSERT INTO reconciliation_exceptions (public_id, type, payment_id, attempt_id, callback_id, detail)
VALUES (sqlc.arg(public_id), sqlc.arg(type), sqlc.narg(payment_id), sqlc.narg(attempt_id),
        sqlc.narg(callback_id), sqlc.arg(detail))
ON CONFLICT DO NOTHING;

-- name: ListOpenReconExceptions :many
SELECT * FROM reconciliation_exceptions WHERE status = 'open' ORDER BY created_at, id LIMIT sqlc.arg(max_rows);

-- name: ListReconExceptionsForPayment :many
SELECT * FROM reconciliation_exceptions WHERE payment_id = sqlc.arg(payment_id) ORDER BY created_at, id;
