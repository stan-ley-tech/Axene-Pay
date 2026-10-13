//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/ledger"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var ctx = context.Background()

func kes(minor int64) money.Amount {
	a, _ := money.New(minor, money.MustCurrency("KES"))
	return a
}

var (
	receivable = ledger.Account{Book: ledger.BookAxene, Code: "fees_receivable:mer_1", Type: ledger.Asset}
	revenue    = ledger.Account{Book: ledger.BookAxene, Code: "fee_revenue", Type: ledger.Revenue}
)

func accrual(source string, minor int64) ledger.Entry {
	return ledger.Entry{
		Book: ledger.BookAxene, Reason: ledger.ReasonFeeAccrued, SourceType: "fee", SourceID: source,
		Description: "fee accrued", EffectiveAt: time.Now(), RequestID: "req",
		Lines: []ledger.Line{{Account: receivable, Direction: ledger.Debit, Amount: kes(minor)},
			{Account: revenue, Direction: ledger.Credit, Amount: kes(minor)}},
	}
}

func post(t *testing.T, pool *pgxpool.Pool, e ledger.Entry) ledger.Posted {
	t.Helper()
	var p ledger.Posted
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		p, err = ledger.Post(ctx, store.New(tx), e)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func balance(t *testing.T, pool *pgxpool.Pool, a ledger.Account) int64 {
	t.Helper()
	b, err := ledger.Balance(ctx, store.New(pool), a.Book, a.Code, money.MustCurrency("KES"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pgCode(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestPostIsIdempotentAndBalancesAreDerived(t *testing.T) {
	pool := testdb.New(t)
	first := post(t, pool, accrual("fee_A", 150))
	if !first.Created {
		t.Fatal("first post not created")
	}
	again := post(t, pool, accrual("fee_A", 150))
	if again.Created || again.EntryID != first.EntryID {
		t.Fatalf("replay created a second entry: %+v vs %+v", again, first)
	}
	post(t, pool, accrual("fee_B", 50))
	if b := balance(t, pool, receivable); b != 200 {
		t.Fatalf("receivable = %d, want 200", b)
	}
	if b := balance(t, pool, revenue); b != -200 {
		t.Fatalf("revenue = %d, want -200 (credit balance)", b)
	}
}

func TestConcurrentPostsOfOneSourcePostOnce(t *testing.T) {
	pool := testdb.New(t)
	var wg sync.WaitGroup
	created := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				p, err := ledger.Post(ctx, store.New(tx), accrual("fee_C", 75))
				created <- p.Created
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	if n != 1 || balance(t, pool, receivable) != 75 {
		t.Fatalf("created %d entries, balance %d", n, balance(t, pool, receivable))
	}
}

func TestReverseMirrorsOnceAndZeroesBalances(t *testing.T) {
	pool := testdb.New(t)
	post(t, pool, accrual("fee_D", 300))
	var rev ledger.Posted
	reverse := func() (ledger.Posted, error) {
		var p ledger.Posted
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			p, err = ledger.Reverse(ctx, store.New(tx), "fee", "fee_D", ledger.ReasonFeeAccrued, ledger.ReasonFeeReversed, "fee waived", "req", time.Now())
			return err
		})
		return p, err
	}
	rev, err := reverse()
	if err != nil || !rev.Created {
		t.Fatalf("reverse: %+v %v", rev, err)
	}
	if again, err := reverse(); err != nil || again.Created {
		t.Fatalf("second reverse: %+v %v", again, err)
	}
	if balance(t, pool, receivable) != 0 || balance(t, pool, revenue) != 0 {
		t.Fatal("reversal did not zero balances")
	}
	entries, _ := store.New(pool).ListJournalEntriesForSource(ctx, store.ListJournalEntriesForSourceParams{SourceType: "fee", SourceID: "fee_D"})
	if len(entries) != 2 || entries[1].ReversesEntryID != entries[0].ID {
		t.Fatalf("entries %+v", entries)
	}
}

// The database enforces balance and consistency even if application code
// is bypassed.
func TestSchemaRejectsUnbalancedAndInconsistentEntries(t *testing.T) {
	pool := testdb.New(t)
	q := store.New(pool)
	for _, a := range []ledger.Account{receivable, revenue, {Book: ledger.BookSettlementMemo, Code: "processed_volume:mer_1", Type: ledger.Memo}} {
		if err := q.EnsureLedgerAccount(ctx, store.EnsureLedgerAccountParams{Book: string(a.Book), Code: a.Code, Currency: "KES", Type: string(a.Type)}); err != nil {
			t.Fatal(err)
		}
	}
	acct := func(book, code, cur string) store.LedgerAccount {
		a, err := q.GetLedgerAccount(ctx, store.GetLedgerAccountParams{Book: book, Code: code, Currency: cur})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	recv, rev, memo := acct("axene", receivable.Code, "KES"), acct("axene", revenue.Code, "KES"), acct("settlement_memo", "processed_volume:mer_1", "KES")

	write := func(lines func(tx pgx.Tx, entry store.InsertJournalEntryRow) error) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			tq := store.New(tx)
			e, err := tq.InsertJournalEntry(ctx, store.InsertJournalEntryParams{
				PublicID: ids.New(ids.JournalEntry), Book: "axene", PostingReason: "adjustment", SourceType: "test",
				SourceID: ids.New(ids.Event), Description: "raw", EffectiveAt: time.Now(), RequestID: "r",
			})
			if err != nil {
				return err
			}
			return lines(tx, e)
		})
	}
	line := func(tx pgx.Tx, e store.InsertJournalEntryRow, a store.LedgerAccount, dir string, amt int64, cur string) error {
		return store.New(tx).InsertJournalLine(ctx, store.InsertJournalLineParams{EntryID: e.ID, AccountID: a.ID, Direction: dir, AmountMinor: amt, Currency: cur})
	}

	cases := map[string]func(tx pgx.Tx, e store.InsertJournalEntryRow) error{
		"unbalanced": func(tx pgx.Tx, e store.InsertJournalEntryRow) error {
			if err := line(tx, e, recv, "debit", 100, "KES"); err != nil {
				return err
			}
			return line(tx, e, rev, "credit", 99, "KES")
		},
		"single line": func(tx pgx.Tx, e store.InsertJournalEntryRow) error { return line(tx, e, recv, "debit", 100, "KES") },
		"no lines":    func(tx pgx.Tx, e store.InsertJournalEntryRow) error { return nil },
		"currency mismatch": func(tx pgx.Tx, e store.InsertJournalEntryRow) error {
			if err := line(tx, e, recv, "debit", 100, "USD"); err != nil {
				return err
			}
			return line(tx, e, rev, "credit", 100, "USD")
		},
		"cross-book line": func(tx pgx.Tx, e store.InsertJournalEntryRow) error {
			if err := line(tx, e, memo, "debit", 100, "KES"); err != nil {
				return err
			}
			return line(tx, e, rev, "credit", 100, "KES")
		},
	}
	for name, lines := range cases {
		if err := write(lines); pgCode(err) != "23514" {
			t.Errorf("%s: err = %v, want check_violation", name, err)
		}
	}

	posted := post(t, pool, accrual("fee_E", 10))
	for _, stmt := range []string{
		`UPDATE journal_lines SET amount_minor = 1 WHERE entry_id = $1`,
		`DELETE FROM journal_lines WHERE entry_id = $1`,
		`UPDATE journal_entries SET description = 'x' WHERE id = $1`,
		`DELETE FROM journal_entries WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, stmt, posted.EntryID); pgCode(err) != "42501" {
			t.Errorf("%s: err = %v, want insufficient_privilege", stmt, err)
		}
	}
	tb, err := q.TrialBalance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tb {
		if r.Debits != r.Credits {
			t.Errorf("trial balance %s %s: %d != %d", r.Book, r.Currency, r.Debits, r.Credits)
		}
	}
}

func TestAccountTypeConflictRejected(t *testing.T) {
	pool := testdb.New(t)
	post(t, pool, accrual("fee_F", 10))
	bad := accrual("fee_G", 10)
	bad.Lines[0].Account.Type = ledger.Liability // same code, different type
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(ctx, store.New(tx), bad)
		return err
	})
	if !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}
