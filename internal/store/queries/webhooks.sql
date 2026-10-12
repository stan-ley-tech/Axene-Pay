-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints
    (public_id, merchant_id, environment, url, event_types, description, secret_ciphertext, secret_key_version)
VALUES (sqlc.arg(public_id), sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(url),
        sqlc.arg(event_types)::text[], sqlc.arg(description), sqlc.arg(secret_ciphertext), sqlc.arg(secret_key_version))
RETURNING *;

-- name: ListWebhookEndpoints :many
SELECT * FROM webhook_endpoints
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment)
ORDER BY created_at, id;

-- name: GetWebhookEndpoint :one
SELECT * FROM webhook_endpoints
WHERE public_id = sqlc.arg(public_id) AND merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment);

-- name: GetWebhookEndpointByID :one
SELECT * FROM webhook_endpoints WHERE id = sqlc.arg(id);

-- name: DisableWebhookEndpoint :execrows
UPDATE webhook_endpoints SET status = 'disabled', updated_at = now()
WHERE public_id = sqlc.arg(public_id) AND merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment);

-- name: ListEnabledEndpoints :many
SELECT * FROM webhook_endpoints
WHERE merchant_id = sqlc.arg(merchant_id) AND environment = sqlc.arg(environment) AND status = 'enabled'
ORDER BY created_at, id;

-- name: GetOutboxEventByPublicID :one
SELECT * FROM outbox_events WHERE public_id = sqlc.arg(public_id);

-- name: InsertWebhookFanout :execrows
INSERT INTO webhook_event_fanouts (event_id, deliveries, fanned_out_at)
VALUES (sqlc.arg(event_id), sqlc.arg(deliveries), sqlc.arg(now)::timestamptz)
ON CONFLICT DO NOTHING;

-- name: ListUnfannedEvents :many
-- Published payment events without a fan-out record: the broker message
-- was lost or not yet consumed. The sweep fans them out directly.
SELECT e.* FROM outbox_events e
LEFT JOIN webhook_event_fanouts f ON f.event_id = e.id
WHERE f.event_id IS NULL
  AND e.aggregate_type = 'payment'
  AND e.published_at IS NOT NULL
  AND e.published_at < sqlc.arg(published_before)::timestamptz
ORDER BY e.created_at, e.id
LIMIT sqlc.arg(max_rows);

-- name: InsertWebhookDelivery :execrows
INSERT INTO webhook_deliveries
    (public_id, endpoint_id, event_id, event_public_id, event_type, body, next_attempt_at, created_at)
VALUES (sqlc.arg(public_id), sqlc.arg(endpoint_id), sqlc.arg(event_id), sqlc.arg(event_public_id),
        sqlc.arg(event_type), sqlc.arg(body), sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT DO NOTHING;

-- name: ClaimDueDeliveries :many
UPDATE webhook_deliveries
SET next_attempt_at = sqlc.arg(lease_until)::timestamptz, updated_at = now()
WHERE id IN (
    SELECT due.id FROM webhook_deliveries due
    WHERE due.next_attempt_at <= sqlc.arg(now)::timestamptz
    ORDER BY due.next_attempt_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: RecordDeliveryAttempt :exec
INSERT INTO webhook_delivery_attempts (delivery_id, attempted_at, status_code, error, duration_ms)
VALUES (sqlc.arg(delivery_id), sqlc.arg(attempted_at)::timestamptz, sqlc.narg(status_code), sqlc.narg(error), sqlc.arg(duration_ms));

-- name: MarkDeliveryDelivered :exec
UPDATE webhook_deliveries
SET status = 'delivered', attempts = attempts + 1, next_attempt_at = NULL, last_status_code = sqlc.narg(status_code),
    last_error = NULL, delivered_at = sqlc.arg(now)::timestamptz, updated_at = now()
WHERE id = sqlc.arg(id);

-- name: MarkDeliveryRetry :exec
UPDATE webhook_deliveries
SET attempts = attempts + 1, next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_status_code = sqlc.narg(status_code), last_error = sqlc.narg(error), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: MarkDeliveryDead :exec
UPDATE webhook_deliveries
SET status = 'dead', attempts = attempts + 1, next_attempt_at = NULL,
    last_status_code = sqlc.narg(status_code), last_error = sqlc.narg(error), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: DeferDelivery :exec
-- Endpoint disabled: stop delivering without consuming attempts.
UPDATE webhook_deliveries SET status = 'dead', next_attempt_at = NULL, last_error = 'endpoint disabled', updated_at = now()
WHERE id = sqlc.arg(id);

-- name: ListDeliveriesForEndpoint :many
SELECT * FROM webhook_deliveries WHERE endpoint_id = sqlc.arg(endpoint_id)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_rows);

-- name: GetDeliveryForMerchant :one
SELECT sqlc.embed(d), e.public_id AS endpoint_public_id
FROM webhook_deliveries d
JOIN webhook_endpoints e ON e.id = d.endpoint_id
WHERE d.public_id = sqlc.arg(public_id) AND e.merchant_id = sqlc.arg(merchant_id) AND e.environment = sqlc.arg(environment);

-- name: ReplayDelivery :execrows
-- Gives a finished delivery a fresh attempt budget. Pending deliveries are
-- left alone.
UPDATE webhook_deliveries
SET status = 'pending', attempts = 0, next_attempt_at = sqlc.arg(now)::timestamptz, delivered_at = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('delivered', 'dead');

-- name: ListAttemptsForDelivery :many
SELECT * FROM webhook_delivery_attempts WHERE delivery_id = sqlc.arg(delivery_id) ORDER BY id;
