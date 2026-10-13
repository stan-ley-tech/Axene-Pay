-- Double-entry ledger (docs/ledger.md).

-- +goose Up

CREATE TABLE ledger_accounts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    book         text        NOT NULL CHECK (book IN ('axene', 'settlement_memo')),
    code         text        NOT NULL CHECK (code ~ '^[a-z_]+(:[A-Za-z0-9_]+)*$' AND length(code) <= 200),
    currency     char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    type         text        NOT NULL CHECK (type IN ('asset', 'liability', 'revenue', 'expense', 'memo')),
    merchant_id  uuid        REFERENCES merchants (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book, code, currency),
    CHECK ((book = 'settlement_memo') = (type = 'memo'))
);

CREATE TABLE journal_entries (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id          text        NOT NULL UNIQUE CHECK (public_id ~ '^jnl_[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
    book               text        NOT NULL CHECK (book IN ('axene', 'settlement_memo')),
    posting_reason     text        NOT NULL CHECK (posting_reason IN (
                           'payment_succeeded', 'payment_reversed', 'fee_accrued', 'fee_collected', 'fee_reversed',
                           'provider_fee_withheld', 'provider_fee_axene', 'settlement_confirmed', 'adjustment')),
    source_type        text        NOT NULL,
    source_id          text        NOT NULL,
    description        text        NOT NULL,
    effective_at       timestamptz NOT NULL,
    -- A correction references the entry it reverses; each entry is reversed at most once.
    reverses_entry_id  uuid        UNIQUE REFERENCES journal_entries (id),
    request_id         text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- Exactly-once financial effects: a replayed event cannot post twice.
    UNIQUE (source_type, source_id, posting_reason)
);
CREATE INDEX journal_entries_source_idx ON journal_entries (source_type, source_id);

CREATE TABLE journal_lines (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_id      uuid        NOT NULL REFERENCES journal_entries (id),
    account_id    uuid        NOT NULL REFERENCES ledger_accounts (id),
    direction     text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_minor  bigint      NOT NULL CHECK (amount_minor > 0),
    currency      char(3)     NOT NULL
);
CREATE INDEX journal_lines_entry_idx ON journal_lines (entry_id);
CREATE INDEX journal_lines_account_idx ON journal_lines (account_id);

-- A line must use its account's currency, and its entry's book must be the
-- account's book.
-- +goose StatementBegin
CREATE FUNCTION journal_line_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    acct ledger_accounts%ROWTYPE;
    entry_book text;
BEGIN
    SELECT * INTO acct FROM ledger_accounts WHERE id = NEW.account_id;
    SELECT book INTO entry_book FROM journal_entries WHERE id = NEW.entry_id;
    IF acct.currency <> NEW.currency THEN
        RAISE EXCEPTION 'journal line currency % does not match account currency %', NEW.currency, acct.currency
            USING ERRCODE = 'check_violation';
    END IF;
    IF acct.book <> entry_book THEN
        RAISE EXCEPTION 'journal line account book % does not match entry book %', acct.book, entry_book
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER journal_lines_consistency BEFORE INSERT ON journal_lines
    FOR EACH ROW EXECUTE FUNCTION journal_line_consistency();

-- Every entry must have at least two lines and balance per currency. Checked
-- at commit (deferred) so lines can be inserted one by one.
-- +goose StatementBegin
CREATE FUNCTION journal_entry_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target uuid;
    line_count integer;
    unbalanced integer;
BEGIN
    -- Separate branches: PL/pgSQL plans each statement only when it runs, so
    -- the field that does not exist on the other table is never resolved.
    IF TG_TABLE_NAME = 'journal_entries' THEN
        target := NEW.id;
    ELSE
        target := NEW.entry_id;
    END IF;
    SELECT count(*) INTO line_count FROM journal_lines WHERE entry_id = target;
    IF line_count < 2 THEN
        RAISE EXCEPTION 'journal entry % has % lines; at least 2 required', target, line_count
            USING ERRCODE = 'check_violation';
    END IF;
    SELECT count(*) INTO unbalanced FROM (
        SELECT currency
        FROM journal_lines WHERE entry_id = target
        GROUP BY currency
        HAVING sum(CASE WHEN direction = 'debit' THEN amount_minor ELSE 0 END)
            <> sum(CASE WHEN direction = 'credit' THEN amount_minor ELSE 0 END)
    ) u;
    IF unbalanced > 0 THEN
        RAISE EXCEPTION 'journal entry % does not balance', target USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER journal_entries_balanced AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION journal_entry_balanced();
CREATE CONSTRAINT TRIGGER journal_lines_balanced AFTER INSERT ON journal_lines
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION journal_entry_balanced();

CREATE TRIGGER journal_entries_append_only BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER journal_lines_append_only BEFORE UPDATE OR DELETE ON journal_lines
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_accounts_append_only BEFORE UPDATE OR DELETE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose Down
DROP TABLE journal_lines;
DROP TABLE journal_entries;
DROP TABLE ledger_accounts;
DROP FUNCTION journal_entry_balanced();
DROP FUNCTION journal_line_consistency();
