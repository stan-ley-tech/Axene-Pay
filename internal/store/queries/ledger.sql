-- name: EnsureLedgerAccount :exec
INSERT INTO ledger_accounts (book, code, currency, type, merchant_id)
VALUES (sqlc.arg(book), sqlc.arg(code), sqlc.arg(currency), sqlc.arg(type), sqlc.narg(merchant_id))
ON CONFLICT (book, code, currency) DO NOTHING;

-- name: GetLedgerAccount :one
SELECT * FROM ledger_accounts
WHERE book = sqlc.arg(book) AND code = sqlc.arg(code) AND currency = sqlc.arg(currency);

-- name: InsertJournalEntry :one
-- Returns no row when an entry for the same source and reason exists: the
-- financial effect has already been recorded.
INSERT INTO journal_entries
    (public_id, book, posting_reason, source_type, source_id, description, effective_at, reverses_entry_id, request_id)
VALUES (sqlc.arg(public_id), sqlc.arg(book), sqlc.arg(posting_reason), sqlc.arg(source_type), sqlc.arg(source_id),
        sqlc.arg(description), sqlc.arg(effective_at)::timestamptz, sqlc.narg(reverses_entry_id), sqlc.arg(request_id))
ON CONFLICT (source_type, source_id, posting_reason) DO NOTHING
RETURNING id, public_id;

-- name: GetJournalEntryBySource :one
SELECT * FROM journal_entries
WHERE source_type = sqlc.arg(source_type) AND source_id = sqlc.arg(source_id) AND posting_reason = sqlc.arg(posting_reason);

-- name: InsertJournalLine :exec
INSERT INTO journal_lines (entry_id, account_id, direction, amount_minor, currency)
VALUES (sqlc.arg(entry_id), sqlc.arg(account_id), sqlc.arg(direction), sqlc.arg(amount_minor), sqlc.arg(currency));

-- name: ListJournalLines :many
SELECT l.*, a.book, a.code, a.type AS account_type, a.merchant_id
FROM journal_lines l
JOIN ledger_accounts a ON a.id = l.account_id
WHERE l.entry_id = sqlc.arg(entry_id)
ORDER BY l.id;

-- name: ListJournalEntriesForSource :many
SELECT * FROM journal_entries
WHERE source_type = sqlc.arg(source_type) AND source_id = sqlc.arg(source_id)
ORDER BY created_at, id;

-- name: AccountBalance :one
-- Debits minus credits, derived from lines.
SELECT COALESCE(sum(CASE WHEN l.direction = 'debit' THEN l.amount_minor ELSE -l.amount_minor END), 0)::bigint AS balance
FROM journal_lines l
JOIN ledger_accounts a ON a.id = l.account_id
WHERE a.book = sqlc.arg(book) AND a.code = sqlc.arg(code) AND a.currency = sqlc.arg(currency);

-- name: TrialBalance :many
-- Per book and currency, total debits and credits across all lines.
SELECT a.book, a.currency,
       COALESCE(sum(CASE WHEN l.direction = 'debit' THEN l.amount_minor ELSE 0 END), 0)::bigint AS debits,
       COALESCE(sum(CASE WHEN l.direction = 'credit' THEN l.amount_minor ELSE 0 END), 0)::bigint AS credits
FROM journal_lines l
JOIN ledger_accounts a ON a.id = l.account_id
GROUP BY a.book, a.currency
ORDER BY a.book, a.currency;
