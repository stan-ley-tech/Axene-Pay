-- name: InsertAudit :exec
INSERT INTO audit_log (actor, action, target_type, target_id, detail, request_id)
VALUES (sqlc.arg(actor), sqlc.arg(action), sqlc.arg(target_type), sqlc.arg(target_id), sqlc.arg(detail), sqlc.arg(request_id));

-- name: ListAuditForTarget :many
SELECT * FROM audit_log WHERE target_type = sqlc.arg(target_type) AND target_id = sqlc.arg(target_id) ORDER BY id;
