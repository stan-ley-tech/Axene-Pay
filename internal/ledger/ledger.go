// Package ledger posts balanced, immutable double-entry journal entries
// (docs/ledger.md). Balances are always derived from journal lines.
//
// Posting is idempotent: an entry is identified by its source and posting
// reason, and posting the same (source, reason) again returns the existing
// entry without effect. Callers post inside the database transaction that
// makes the business change, passing that transaction's queries.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Book is a separately balanced set of accounts.
type Book string

const (
	// BookAxene holds Axene's own financial positions.
	BookAxene Book = "axene"
	// BookSettlementMemo tracks funds Axene does not hold.
	BookSettlementMemo Book = "settlement_memo"
)

// AccountType classifies an account.
type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
	Memo      AccountType = "memo"
)

// Reason is a closed set of business reasons for a posting.
type Reason string

const (
	ReasonPaymentSucceeded    Reason = "payment_succeeded"
	ReasonPaymentReversed     Reason = "payment_reversed"
	ReasonFeeAccrued          Reason = "fee_accrued"
	ReasonFeeCollected        Reason = "fee_collected"
	ReasonFeeReversed         Reason = "fee_reversed"
	ReasonProviderFeeWithheld Reason = "provider_fee_withheld"
	ReasonProviderFeeAxene    Reason = "provider_fee_axene"
	ReasonSettlementConfirmed Reason = "settlement_confirmed"
	ReasonAdjustment          Reason = "adjustment"
)

// Direction is debit or credit.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Account identifies an account; it is created on first use.
type Account struct {
	Book       Book
	Code       string
	Type       AccountType
	MerchantID pgtype.UUID
}

// Line is one side of an entry.
type Line struct {
	Account   Account
	Direction Direction
	Amount    money.Amount
}

// Entry is a journal entry to post.
type Entry struct {
	Book        Book
	Reason      Reason
	SourceType  string
	SourceID    string
	Description string
	EffectiveAt time.Time
	RequestID   string
	Lines       []Line
}

var (
	// ErrUnbalanced means debits and credits differ for some currency.
	ErrUnbalanced = errors.New("ledger: entry does not balance")
	// ErrInvalid wraps other entry validation failures.
	ErrInvalid = errors.New("ledger: invalid entry")
)

// Posted describes the outcome of Post.
type Posted struct {
	EntryID  pgtype.UUID
	PublicID string
	// Created is false when the entry already existed (idempotent replay).
	Created bool
}

// Validate checks an entry without touching the database.
func (e Entry) Validate() error {
	if e.SourceType == "" || e.SourceID == "" || e.Reason == "" || e.Description == "" {
		return fmt.Errorf("%w: source, reason, and description are required", ErrInvalid)
	}
	if len(e.Lines) < 2 {
		return fmt.Errorf("%w: at least two lines required", ErrInvalid)
	}
	sums := map[string]int64{}
	for _, l := range e.Lines {
		if l.Account.Book != e.Book {
			return fmt.Errorf("%w: account %s is in book %s, entry is in %s", ErrInvalid, l.Account.Code, l.Account.Book, e.Book)
		}
		if !l.Amount.IsPositive() {
			return fmt.Errorf("%w: line amounts must be positive", ErrInvalid)
		}
		if (e.Book == BookSettlementMemo) != (l.Account.Type == Memo) {
			return fmt.Errorf("%w: memo accounts belong only to the memo book", ErrInvalid)
		}
		cur := l.Amount.Currency().Code()
		var delta int64
		switch l.Direction {
		case Debit:
			delta = l.Amount.Minor()
		case Credit:
			delta = -l.Amount.Minor()
		default:
			return fmt.Errorf("%w: direction must be debit or credit", ErrInvalid)
		}
		s := sums[cur]
		if (delta > 0 && s > (1<<63-1)-delta) || (delta < 0 && s < (-1<<63)-delta) {
			return fmt.Errorf("%w: amounts overflow", ErrInvalid)
		}
		sums[cur] = s + delta
	}
	for cur, s := range sums {
		if s != 0 {
			return fmt.Errorf("%w: %s off by %d minor units", ErrUnbalanced, cur, s)
		}
	}
	return nil
}

// Post records e, or returns the existing entry for the same source and
// reason. It must run inside the caller's transaction (q bound to it).
func Post(ctx context.Context, q *store.Queries, e Entry) (Posted, error) {
	return post(ctx, q, e, pgtype.UUID{})
}

func post(ctx context.Context, q *store.Queries, e Entry, reverses pgtype.UUID) (Posted, error) {
	if err := e.Validate(); err != nil {
		return Posted{}, err
	}
	row, err := q.InsertJournalEntry(ctx, store.InsertJournalEntryParams{
		PublicID: ids.New(ids.JournalEntry), Book: string(e.Book), PostingReason: string(e.Reason),
		SourceType: e.SourceType, SourceID: e.SourceID, Description: e.Description,
		EffectiveAt: e.EffectiveAt, ReversesEntryID: reverses, RequestID: e.RequestID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := q.GetJournalEntryBySource(ctx, store.GetJournalEntryBySourceParams{
			SourceType: e.SourceType, SourceID: e.SourceID, PostingReason: string(e.Reason),
		})
		if err != nil {
			return Posted{}, fmt.Errorf("ledger: load existing entry: %w", err)
		}
		return Posted{EntryID: existing.ID, PublicID: existing.PublicID}, nil
	}
	if err != nil {
		return Posted{}, fmt.Errorf("ledger: insert entry: %w", err)
	}
	for _, l := range e.Lines {
		acct, err := ensureAccount(ctx, q, l.Account, l.Amount.Currency().Code())
		if err != nil {
			return Posted{}, err
		}
		if err := q.InsertJournalLine(ctx, store.InsertJournalLineParams{
			EntryID: row.ID, AccountID: acct, Direction: string(l.Direction),
			AmountMinor: l.Amount.Minor(), Currency: l.Amount.Currency().Code(),
		}); err != nil {
			return Posted{}, fmt.Errorf("ledger: insert line: %w", err)
		}
	}
	return Posted{EntryID: row.ID, PublicID: row.PublicID, Created: true}, nil
}

func ensureAccount(ctx context.Context, q *store.Queries, a Account, currency string) (pgtype.UUID, error) {
	if err := q.EnsureLedgerAccount(ctx, store.EnsureLedgerAccountParams{
		Book: string(a.Book), Code: a.Code, Currency: currency, Type: string(a.Type), MerchantID: a.MerchantID,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("ledger: ensure account %s: %w", a.Code, err)
	}
	acct, err := q.GetLedgerAccount(ctx, store.GetLedgerAccountParams{Book: string(a.Book), Code: a.Code, Currency: currency})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("ledger: load account %s: %w", a.Code, err)
	}
	if acct.Type != string(a.Type) {
		return pgtype.UUID{}, fmt.Errorf("%w: account %s exists with type %s, not %s", ErrInvalid, a.Code, acct.Type, a.Type)
	}
	return acct.ID, nil
}

// Reverse posts a compensating entry that mirrors the entry recorded for
// (sourceType, sourceID, original) with every direction flipped. It is
// idempotent under reason; reversing an entry twice is impossible
// (reverses_entry_id is unique).
func Reverse(ctx context.Context, q *store.Queries, sourceType, sourceID string, original, reason Reason, description, requestID string, at time.Time) (Posted, error) {
	orig, err := q.GetJournalEntryBySource(ctx, store.GetJournalEntryBySourceParams{
		SourceType: sourceType, SourceID: sourceID, PostingReason: string(original),
	})
	if err != nil {
		return Posted{}, fmt.Errorf("ledger: load entry to reverse: %w", err)
	}
	lines, err := q.ListJournalLines(ctx, orig.ID)
	if err != nil {
		return Posted{}, err
	}
	e := Entry{Book: Book(orig.Book), Reason: reason, SourceType: sourceType, SourceID: sourceID,
		Description: description, EffectiveAt: at, RequestID: requestID}
	for _, l := range lines {
		cur, err := money.ParseCurrency(l.Currency)
		if err != nil {
			return Posted{}, err
		}
		amt, _ := money.New(l.AmountMinor, cur)
		dir := Credit
		if Direction(l.Direction) == Credit {
			dir = Debit
		}
		e.Lines = append(e.Lines, Line{
			Account:   Account{Book: Book(l.Book), Code: l.Code, Type: AccountType(l.AccountType), MerchantID: l.MerchantID},
			Direction: dir, Amount: amt,
		})
	}
	return post(ctx, q, e, orig.ID)
}

// Balance returns debits minus credits for an account.
func Balance(ctx context.Context, q *store.Queries, book Book, code string, c money.Currency) (int64, error) {
	return q.AccountBalance(ctx, store.AccountBalanceParams{Book: string(book), Code: code, Currency: c.Code()})
}
