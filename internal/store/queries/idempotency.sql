-- name: DeleteExpiredIdempotencyKey :exec
DELETE FROM idempotency_keys
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment)
  AND key = sqlc.arg(key) AND expires_at < sqlc.arg(now);

-- name: InsertIdempotencyKey :execrows
-- A concurrent insert of the same key waits for the first transaction and
-- then inserts nothing, so exactly one request owns the key.
INSERT INTO idempotency_keys
    (merchant_id, environment, key, request_method, request_path, request_fingerprint, status, expires_at)
VALUES (sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(key), sqlc.arg(request_method),
        sqlc.arg(request_path), sqlc.arg(request_fingerprint), 'in_progress', sqlc.arg(expires_at))
ON CONFLICT DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment) AND key = sqlc.arg(key);

-- name: SetIdempotencyResource :exec
UPDATE idempotency_keys SET resource_id = sqlc.arg(resource_id)
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment) AND key = sqlc.arg(key);

-- name: CompleteIdempotencyKey :execrows
UPDATE idempotency_keys
SET status = 'completed', response_status = sqlc.arg(response_status),
    response_body = sqlc.arg(response_body), completed_at = now()
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment)
  AND key = sqlc.arg(key) AND status = 'in_progress';

-- name: DeleteIdempotencyKey :exec
-- Releases a key whose request failed before any side effect.
DELETE FROM idempotency_keys
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment)
  AND key = sqlc.arg(key) AND status = 'in_progress' AND resource_id IS NULL;

-- name: PruneIdempotencyKeys :execrows
DELETE FROM idempotency_keys
WHERE (merchant_id, environment, key) IN (
    SELECT k.merchant_id, k.environment, k.key FROM idempotency_keys k
    WHERE k.expires_at < sqlc.arg(now)
    LIMIT sqlc.arg(max_rows)
);
