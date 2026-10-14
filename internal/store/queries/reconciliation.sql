-- name: InsertSettlementBatch :one
INSERT INTO settlement_batches (public_id, provider, environment, source_reference, source_sha256, line_count, imported_by, imported_at)
VALUES (sqlc.arg(public_id), sqlc.arg(provider), sqlc.arg(environment), sqlc.arg(source_reference), sqlc.arg(source_sha256),
        sqlc.arg(line_count), sqlc.arg(imported_by), sqlc.arg(imported_at)::timestamptz)
RETURNING *;

-- name: InsertSettlementLine :exec
INSERT INTO settlement_lines
    (public_id, batch_id, line_number, provider_reference, receipt, destination_identifier, currency, gross_minor,
     provider_fee_minor, net_minor, axene_fee_collected_minor, settled_at)
VALUES (sqlc.arg(public_id), sqlc.arg(batch_id), sqlc.arg(line_number), sqlc.narg(provider_reference), sqlc.narg(receipt),
        sqlc.arg(destination_identifier), sqlc.arg(currency), sqlc.arg(gross_minor), sqlc.arg(provider_fee_minor),
        sqlc.arg(net_minor), sqlc.narg(axene_fee_collected_minor), sqlc.arg(settled_at)::timestamptz);

-- name: ListPendingSettlementLines :many
SELECT l.* FROM settlement_lines l
JOIN settlement_batches b ON b.id = l.batch_id
WHERE b.provider = sqlc.arg(provider) AND b.environment = sqlc.arg(environment) AND l.match_status = 'pending'
ORDER BY b.imported_at, l.line_number
LIMIT sqlc.arg(max_rows);

-- name: GetSettlementLine :one
SELECT * FROM settlement_lines WHERE id = sqlc.arg(id);

-- name: LockSettlementLine :one
SELECT * FROM settlement_lines WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: MatchSettlementLine :execrows
UPDATE settlement_lines SET match_status = 'matched', payment_id = sqlc.arg(payment_id)
WHERE id = sqlc.arg(id) AND match_status <> 'matched';

-- name: SetSettlementLineStatus :exec
UPDATE settlement_lines SET match_status = sqlc.arg(match_status) WHERE id = sqlc.arg(id) AND match_status <> 'matched';

-- name: CountOtherLinesWithReference :one
-- Lines elsewhere (any batch) carrying the same provider reference or receipt.
SELECT count(*)::bigint FROM settlement_lines l
JOIN settlement_batches b ON b.id = l.batch_id
WHERE b.provider = sqlc.arg(provider) AND l.id <> sqlc.arg(id)
  AND ((sqlc.narg(provider_reference)::text IS NOT NULL AND l.provider_reference = sqlc.narg(provider_reference)::text)
    OR (sqlc.narg(receipt)::text IS NOT NULL AND l.receipt = sqlc.narg(receipt)::text))
  AND (l.match_status = 'matched' OR l.created_at < (SELECT created_at FROM settlement_lines WHERE id = sqlc.arg(id))
       OR (l.created_at = (SELECT created_at FROM settlement_lines WHERE id = sqlc.arg(id)) AND l.id < sqlc.arg(id)));

-- name: GetAttemptByReceipt :one
SELECT * FROM payment_attempts
WHERE provider = sqlc.arg(provider) AND provider_receipt = sqlc.arg(receipt)
ORDER BY created_at DESC
LIMIT 1;

-- name: ListUnsettledSucceededPayments :many
-- Verified payments without a matched settlement line after the grace period.
SELECT p.* FROM payments p
WHERE p.environment = sqlc.arg(environment) AND p.status = 'SUCCEEDED' AND p.updated_at < sqlc.arg(settled_before)::timestamptz
  AND EXISTS (SELECT 1 FROM payment_attempts a WHERE a.payment_id = p.id AND a.provider = sqlc.arg(provider))
  AND NOT EXISTS (SELECT 1 FROM settlement_lines l WHERE l.payment_id = p.id)
ORDER BY p.updated_at
LIMIT sqlc.arg(max_rows);

-- name: InsertReconRun :one
INSERT INTO reconciliation_runs (public_id, provider, environment, run_by, started_at)
VALUES (sqlc.arg(public_id), sqlc.arg(provider), sqlc.arg(environment), sqlc.arg(run_by), sqlc.arg(started_at)::timestamptz)
RETURNING *;

-- name: FinishReconRun :exec
UPDATE reconciliation_runs SET finished_at = sqlc.arg(finished_at)::timestamptz, lines_matched = sqlc.arg(lines_matched),
    exceptions = sqlc.arg(exceptions)
WHERE id = sqlc.arg(id);

-- name: InsertLineException :execrows
INSERT INTO reconciliation_exceptions (public_id, type, payment_id, settlement_line_id, run_id, detail)
VALUES (sqlc.arg(public_id), sqlc.arg(type), sqlc.narg(payment_id), sqlc.arg(settlement_line_id), sqlc.narg(run_id), sqlc.arg(detail))
ON CONFLICT DO NOTHING;

-- name: GetReconException :one
SELECT * FROM reconciliation_exceptions WHERE public_id = sqlc.arg(public_id);

-- name: LockReconException :one
SELECT * FROM reconciliation_exceptions WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: ResolveReconException :execrows
UPDATE reconciliation_exceptions
SET status = 'resolved', resolved_at = sqlc.arg(now)::timestamptz, resolved_by = sqlc.arg(resolved_by), resolution = sqlc.arg(resolution)
WHERE id = sqlc.arg(id) AND status = 'open';

-- name: ListReconExceptions :many
SELECT * FROM reconciliation_exceptions
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
ORDER BY created_at, id
LIMIT sqlc.arg(max_rows);
