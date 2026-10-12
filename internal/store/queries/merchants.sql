-- name: CreateMerchant :one
INSERT INTO merchants (public_id, name, status)
VALUES (sqlc.arg(public_id), sqlc.arg(name), sqlc.arg(status))
RETURNING *;

-- name: GetMerchantByPublicID :one
SELECT * FROM merchants WHERE public_id = sqlc.arg(public_id);

-- name: SetMerchantStatus :one
UPDATE merchants SET status = sqlc.arg(status), updated_at = now()
WHERE public_id = sqlc.arg(public_id)
RETURNING *;

-- name: CreateSettlementDestination :one
INSERT INTO settlement_destinations (public_id, merchant_id, environment, type, identifier, label)
VALUES (sqlc.arg(public_id), sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(type), sqlc.arg(identifier), sqlc.arg(label))
RETURNING *;

-- name: GetSettlementDestination :one
SELECT * FROM settlement_destinations
WHERE public_id = sqlc.arg(public_id) AND merchant_id = sqlc.arg(merchant_id);

-- name: CreateProviderConfig :one
INSERT INTO merchant_provider_configs
    (public_id, merchant_id, environment, method, provider, settlement_destination_id, priority)
VALUES (sqlc.arg(public_id), sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(method),
        sqlc.arg(provider), sqlc.arg(settlement_destination_id), sqlc.arg(priority))
RETURNING *;

-- name: ListRoutingCandidates :many
-- Every configuration for the method, in deterministic routing order.
-- Disabled entries are returned so the routing reason can say why they
-- were skipped.
SELECT c.public_id   AS config_public_id,
       c.provider,
       c.priority,
       c.enabled,
       d.id          AS destination_id,
       d.public_id   AS destination_public_id,
       d.type        AS destination_type,
       d.identifier  AS destination_identifier,
       d.status      AS destination_status
FROM merchant_provider_configs c
JOIN settlement_destinations d ON d.id = c.settlement_destination_id
WHERE c.merchant_id = sqlc.arg(merchant_id)
  AND c.environment = sqlc.arg(environment)
  AND c.method = sqlc.arg(method)
ORDER BY c.priority, c.provider;

-- name: CreateAPIKey :one
INSERT INTO api_keys (public_id, merchant_id, environment, display_hint, secret_hash)
VALUES (sqlc.arg(public_id), sqlc.arg(merchant_id), sqlc.arg(environment), sqlc.arg(display_hint), sqlc.arg(secret_hash))
RETURNING id, public_id, environment, display_hint, created_at;

-- name: GetAPIKeyForAuth :one
SELECT k.id,
       k.public_id,
       k.environment,
       k.revoked_at,
       m.id         AS merchant_id,
       m.public_id  AS merchant_public_id,
       m.status     AS merchant_status
FROM api_keys k
JOIN merchants m ON m.id = k.merchant_id
WHERE k.secret_hash = sqlc.arg(secret_hash);

-- name: TouchAPIKey :exec
-- Coarse last-used tracking: at most one write per key per minute.
UPDATE api_keys SET last_used_at = now()
WHERE id = sqlc.arg(id)
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = now()
WHERE public_id = sqlc.arg(public_id) AND revoked_at IS NULL;
