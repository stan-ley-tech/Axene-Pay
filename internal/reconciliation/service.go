package reconciliation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/audit"
	"github.com/stan-ley-tech/axene-pay/internal/fees"
	"github.com/stan-ley-tech/axene-pay/internal/ledger"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var (
	// ErrDuplicateStatement means the same file was imported before.
	ErrDuplicateStatement = errors.New("reconciliation: statement already imported")
	// ErrNotFound means the exception does not exist.
	ErrNotFound = errors.New("reconciliation: not found")
	// ErrInvalid wraps invalid operator requests.
	ErrInvalid = errors.New("reconciliation: invalid request")
)

// Service imports statements, runs reconciliation, and resolves exceptions.
type Service struct {
	pool     *pgxpool.Pool
	q        *store.Queries
	payments *payments.Service
	fees     *fees.Service
	// SettlementGrace: a verified payment with no settlement line after this
	// long is reported as missing_settlement.
	grace time.Duration
	now   func() time.Time
	log   *slog.Logger
}

// Deps configure a Service.
type Deps struct {
	Pool            *pgxpool.Pool
	Payments        *payments.Service
	Fees            *fees.Service
	SettlementGrace time.Duration
	Clock           func() time.Time
	Logger          *slog.Logger
}

// NewService returns a Service.
func NewService(d Deps) (*Service, error) {
	if d.Pool == nil || d.Payments == nil || d.Fees == nil || d.Logger == nil {
		return nil, errors.New("reconciliation: missing dependency")
	}
	if d.SettlementGrace <= 0 {
		return nil, errors.New("reconciliation: settlement grace must be positive")
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	return &Service{pool: d.Pool, q: store.New(d.Pool), payments: d.Payments, fees: d.Fees, grace: d.SettlementGrace,
		now: func() time.Time { return d.Clock().UTC() }, log: d.Logger}, nil
}

// Import stores a normalized statement as a settlement batch. The whole file
// is rejected if any line is malformed, and the same file cannot be
// imported twice.
func (s *Service) Import(ctx context.Context, provider, environment, sourceReference, operator string, r io.Reader) (store.SettlementBatch, error) {
	if environment != "sandbox" && environment != "live" {
		return store.SettlementBatch{}, fmt.Errorf("%w: environment must be sandbox or live", ErrInvalid)
	}
	raw, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return store.SettlementBatch{}, err
	}
	lines, err := ParseStatement(bytes.NewReader(raw))
	if err != nil {
		return store.SettlementBatch{}, err
	}
	sum := sha256.Sum256(raw)
	var batch store.SettlementBatch
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		batch, err = q.InsertSettlementBatch(ctx, store.InsertSettlementBatchParams{
			PublicID: ids.New(ids.SettlementBatch), Provider: provider, Environment: environment, SourceReference: sourceReference,
			SourceSha256: sum[:], LineCount: int32(len(lines)), ImportedBy: operator, ImportedAt: s.now(),
		})
		if pgCode(err) == "23505" {
			return ErrDuplicateStatement
		}
		if err != nil {
			return err
		}
		for _, l := range lines {
			if err := q.InsertSettlementLine(ctx, store.InsertSettlementLineParams{
				PublicID: ids.New(ids.SettlementLine), BatchID: batch.ID, LineNumber: int32(l.Number),
				ProviderReference: nonEmpty(l.ProviderReference), Receipt: nonEmpty(l.Receipt), DestinationIdentifier: l.Destination,
				Currency: l.Currency.Code(), GrossMinor: l.Gross, ProviderFeeMinor: l.ProviderFee, NetMinor: l.Net,
				AxeneFeeCollectedMinor: l.AxeneFeeCollected, SettledAt: l.SettledAt,
			}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, operator, "settlement.import", "settlement_batch", batch.PublicID,
			map[string]any{"provider": provider, "environment": environment, "source_reference": sourceReference, "lines": len(lines)},
			"import-"+batch.PublicID)
	})
	return batch, err
}

// RunResult summarizes a reconciliation run.
type RunResult struct {
	RunID      string
	Matched    int
	Exceptions int
}

// Run matches every pending statement line for the provider and
// environment, then reports verified payments that have not settled within
// the grace period.
func (s *Service) Run(ctx context.Context, provider, environment, operator string) (RunResult, error) {
	if strings.TrimSpace(operator) == "" {
		return RunResult{}, audit.ErrNoActor
	}
	run, err := s.q.InsertReconRun(ctx, store.InsertReconRunParams{
		PublicID: ids.New(ids.ReconRun), Provider: provider, Environment: environment, RunBy: operator, StartedAt: s.now(),
	})
	if err != nil {
		return RunResult{}, err
	}
	res := RunResult{RunID: run.PublicID}
	for {
		lines, err := s.q.ListPendingSettlementLines(ctx, store.ListPendingSettlementLinesParams{Provider: provider, Environment: environment, MaxRows: 200})
		if err != nil {
			return res, err
		}
		if len(lines) == 0 {
			break
		}
		for _, l := range lines {
			matched, err := s.matchLine(ctx, run, provider, l)
			if err != nil {
				return res, fmt.Errorf("reconciliation: line %s: %w", l.PublicID, err)
			}
			if matched {
				res.Matched++
			} else {
				res.Exceptions++
			}
		}
	}
	missing, err := s.q.ListUnsettledSucceededPayments(ctx, store.ListUnsettledSucceededPaymentsParams{
		Environment: environment, Provider: provider, SettledBefore: s.now().Add(-s.grace), MaxRows: 1000,
	})
	if err != nil {
		return res, err
	}
	for _, p := range missing {
		n, err := s.paymentException(ctx, s.q, "missing_settlement", p.ID, run.ID, map[string]any{
			"reason": "verified payment has no settlement line after the grace period", "grace": s.grace.String(),
		})
		if err != nil {
			return res, err
		}
		res.Exceptions += int(n)
	}
	err = s.q.FinishReconRun(ctx, store.FinishReconRunParams{ID: run.ID, FinishedAt: s.now(), LinesMatched: int32(res.Matched), Exceptions: int32(res.Exceptions)})
	return res, err
}

// matchLine reconciles one statement line in its own transaction.
func (s *Service) matchLine(ctx context.Context, run store.ReconciliationRun, provider string, line store.SettlementLine) (bool, error) {
	matched := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		l, err := q.LockSettlementLine(ctx, line.ID)
		if err != nil || l.MatchStatus != "pending" {
			return err
		}
		exception := func(kind string, paymentID pgtype.UUID, detail map[string]any) error {
			b, _ := json.Marshal(detail)
			if _, err := q.InsertLineException(ctx, store.InsertLineExceptionParams{
				PublicID: ids.New(ids.ReconException), Type: kind, PaymentID: paymentID, SettlementLineID: l.ID, RunID: run.ID, Detail: b,
			}); err != nil {
				return err
			}
			return q.SetSettlementLineStatus(ctx, store.SetSettlementLineStatusParams{ID: l.ID, MatchStatus: "exception"})
		}

		dups, err := q.CountOtherLinesWithReference(ctx, store.CountOtherLinesWithReferenceParams{
			Provider: provider, ID: l.ID, ProviderReference: l.ProviderReference, Receipt: l.Receipt,
		})
		if err != nil {
			return err
		}
		if dups > 0 {
			return exception("duplicate_statement_line", pgtype.UUID{}, map[string]any{"reason": "reference already appears on an earlier statement line"})
		}

		attempt, err := s.findAttempt(ctx, q, provider, l)
		if errors.Is(err, pgx.ErrNoRows) {
			return exception("unmatched_statement_line", pgtype.UUID{}, map[string]any{"reason": "no Axene payment for this provider reference or receipt"})
		}
		if err != nil {
			return err
		}
		payment, err := q.LockPayment(ctx, attempt.PaymentID)
		if err != nil {
			return err
		}
		if payment.Currency != l.Currency || payment.AmountMinor != l.GrossMinor {
			return exception("amount_mismatch", payment.ID, map[string]any{
				"statement_gross": l.GrossMinor, "statement_currency": l.Currency, "payment_amount": payment.AmountMinor, "payment_currency": payment.Currency,
			})
		}
		if payment.Status != string(payments.StatusSucceeded) {
			return exception("status_mismatch", payment.ID, map[string]any{
				"reason": "statement shows the payment settled", "payment_status": payment.Status,
			})
		}
		if err := s.postSettlement(ctx, q, payment, l); err != nil {
			return err
		}
		if n, err := q.MatchSettlementLine(ctx, store.MatchSettlementLineParams{ID: l.ID, PaymentID: payment.ID}); err != nil || n != 1 {
			if err == nil {
				err = fmt.Errorf("line %s could not be matched", l.PublicID)
			}
			return err
		}
		matched = true
		return nil
	})
	return matched, err
}

func (s *Service) findAttempt(ctx context.Context, q *store.Queries, provider string, l store.SettlementLine) (store.PaymentAttempt, error) {
	if l.ProviderReference != nil {
		a, err := q.GetAttemptByProviderReference(ctx, store.GetAttemptByProviderReferenceParams{Provider: provider, ProviderReference: l.ProviderReference})
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return a, err
		}
	}
	if l.Receipt != nil {
		return q.GetAttemptByReceipt(ctx, store.GetAttemptByReceiptParams{Provider: provider, Receipt: l.Receipt})
	}
	return store.PaymentAttempt{}, pgx.ErrNoRows
}

// postSettlement records the provider fee, the net settlement, and any fee
// collection the statement evidences.
func (s *Service) postSettlement(ctx context.Context, q *store.Queries, p store.Payment, l store.SettlementLine) error {
	m, err := q.GetMerchantByID(ctx, p.MerchantID)
	if err != nil {
		return err
	}
	d, err := q.GetSettlementDestinationByID(ctx, p.SettlementDestinationID)
	if err != nil {
		return err
	}
	cur, err := money.ParseCurrency(l.Currency)
	if err != nil {
		return err
	}
	amt := func(minor int64) money.Amount { a, _ := money.New(minor, cur); return a }
	expected := ledger.Account{Book: ledger.BookSettlementMemo, Code: "settlement_expected:" + m.PublicID + ":" + d.PublicID, Type: ledger.Memo, MerchantID: m.ID}
	requestID := "recon-" + l.PublicID
	desc := "settlement line " + l.PublicID

	fee, hasFee, err := s.fees.ForPayment(ctx, p.ID)
	if err != nil {
		return err
	}
	if l.ProviderFeeMinor > 0 {
		if _, err := ledger.Post(ctx, q, ledger.Entry{
			Book: ledger.BookSettlementMemo, Reason: ledger.ReasonProviderFeeWithheld, SourceType: "settlement_line", SourceID: l.PublicID,
			Description: desc + ": provider fee withheld", EffectiveAt: l.SettledAt, RequestID: requestID,
			Lines: []ledger.Line{
				{Account: ledger.Account{Book: ledger.BookSettlementMemo, Code: "provider_fees_withheld:" + m.PublicID, Type: ledger.Memo, MerchantID: m.ID}, Direction: ledger.Debit, Amount: amt(l.ProviderFeeMinor)},
				{Account: expected, Direction: ledger.Credit, Amount: amt(l.ProviderFeeMinor)},
			},
		}); err != nil {
			return err
		}
		if hasFee && fee.ProviderFeeBearer == "axene" {
			// Axene absorbs the provider fee: an expense and an amount owed
			// to the merchant, whose settlement it reduced.
			if _, err := ledger.Post(ctx, q, ledger.Entry{
				Book: ledger.BookAxene, Reason: ledger.ReasonProviderFeeAxene, SourceType: "settlement_line", SourceID: l.PublicID,
				Description: desc + ": provider fee absorbed by Axene", EffectiveAt: l.SettledAt, RequestID: requestID,
				Lines: []ledger.Line{
					{Account: ledger.Account{Book: ledger.BookAxene, Code: "provider_fee_expense", Type: ledger.Expense}, Direction: ledger.Debit, Amount: amt(l.ProviderFeeMinor)},
					{Account: ledger.Account{Book: ledger.BookAxene, Code: "provider_fee_reimbursement:" + m.PublicID, Type: ledger.Liability, MerchantID: m.ID}, Direction: ledger.Credit, Amount: amt(l.ProviderFeeMinor)},
				},
			}); err != nil {
				return err
			}
		}
	}
	settledToMerchant := l.NetMinor
	if l.AxeneFeeCollectedMinor != nil && *l.AxeneFeeCollectedMinor > 0 {
		// The provider allocated part of the payment to Axene: that share is
		// not settled to the merchant.
		if _, err := ledger.Post(ctx, q, ledger.Entry{
			Book: ledger.BookSettlementMemo, Reason: ledger.ReasonAdjustment, SourceType: "settlement_line", SourceID: l.PublicID,
			Description: desc + ": Axene fee allocated by provider", EffectiveAt: l.SettledAt, RequestID: requestID,
			Lines: []ledger.Line{
				{Account: ledger.Account{Book: ledger.BookSettlementMemo, Code: "axene_fee_allocated:" + m.PublicID, Type: ledger.Memo, MerchantID: m.ID}, Direction: ledger.Debit, Amount: amt(*l.AxeneFeeCollectedMinor)},
				{Account: expected, Direction: ledger.Credit, Amount: amt(*l.AxeneFeeCollectedMinor)},
			},
		}); err != nil {
			return err
		}
	}
	if settledToMerchant > 0 {
		if _, err := ledger.Post(ctx, q, ledger.Entry{
			Book: ledger.BookSettlementMemo, Reason: ledger.ReasonSettlementConfirmed, SourceType: "settlement_line", SourceID: l.PublicID,
			Description: desc + ": settled to " + d.Type + " " + d.Identifier, EffectiveAt: l.SettledAt, RequestID: requestID,
			Lines: []ledger.Line{
				{Account: ledger.Account{Book: ledger.BookSettlementMemo, Code: "settled:" + m.PublicID + ":" + d.PublicID, Type: ledger.Memo, MerchantID: m.ID}, Direction: ledger.Debit, Amount: amt(settledToMerchant)},
				{Account: expected, Direction: ledger.Credit, Amount: amt(settledToMerchant)},
			},
		}); err != nil {
			return err
		}
	}

	if l.AxeneFeeCollectedMinor != nil {
		switch {
		case !hasFee:
			return s.lineException(ctx, q, "fee_collection_mismatch", p.ID, l.ID, map[string]any{"reason": "statement reports an Axene fee but none accrued", "statement_fee": *l.AxeneFeeCollectedMinor})
		case fee.FeeMinor != *l.AxeneFeeCollectedMinor:
			return s.lineException(ctx, q, "fee_collection_mismatch", p.ID, l.ID, map[string]any{"accrued_fee": fee.FeeMinor, "statement_fee": *l.AxeneFeeCollectedMinor})
		default:
			ref := l.PublicID
			if l.Receipt != nil {
				ref = *l.Receipt
			}
			return s.fees.RecordCollectionEvidence(ctx, q, fee, m.PublicID, "provider_statement", ref, "settlement_line:"+l.PublicID, requestID)
		}
	}
	return nil
}

func (s *Service) lineException(ctx context.Context, q *store.Queries, kind string, paymentID, lineID pgtype.UUID, detail map[string]any) error {
	b, _ := json.Marshal(detail)
	_, err := q.InsertLineException(ctx, store.InsertLineExceptionParams{
		PublicID: ids.New(ids.ReconException), Type: kind, PaymentID: paymentID, SettlementLineID: lineID, Detail: b,
	})
	return err
}

func (s *Service) paymentException(ctx context.Context, q *store.Queries, kind string, paymentID, runID pgtype.UUID, detail map[string]any) (int64, error) {
	b, _ := json.Marshal(detail)
	return q.InsertReconException(ctx, store.InsertReconExceptionParams{
		PublicID: ids.New(ids.ReconException), Type: kind, PaymentID: paymentID, Detail: b,
	})
}

// Action is how an operator resolves an exception.
type Action string

const (
	// ActionAcknowledge closes the exception without changing any record
	// (e.g. explained timing difference, external correction made).
	ActionAcknowledge Action = "acknowledge"
	// ActionPaymentSucceeded / Failed / Reversed apply a reconciliation
	// outcome to the exception's payment through the payment state machine.
	ActionPaymentSucceeded Action = "payment_succeeded"
	ActionPaymentFailed    Action = "payment_failed"
	ActionPaymentReversed  Action = "payment_reversed"
	// ActionRematch puts the exception's statement line back for matching
	// (after the payment's outcome was corrected).
	ActionRematch Action = "rematch"
)

// Resolve closes an open exception with an identified operator and a
// written reason, applying the chosen action in the same transaction and
// recording an audit entry.
func (s *Service) Resolve(ctx context.Context, exceptionID string, action Action, operator, note string) (store.ReconciliationException, error) {
	if strings.TrimSpace(operator) == "" {
		return store.ReconciliationException{}, audit.ErrNoActor
	}
	if strings.TrimSpace(note) == "" {
		return store.ReconciliationException{}, fmt.Errorf("%w: a resolution note is required", ErrInvalid)
	}
	requestID := "resolve-" + exceptionID
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		ex, err := q.GetReconException(ctx, exceptionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ex, err = q.LockReconException(ctx, ex.ID); err != nil {
			return err
		}
		if ex.Status != "open" {
			return fmt.Errorf("%w: exception is already resolved", ErrInvalid)
		}
		actor := "operator:" + operator
		switch action {
		case ActionAcknowledge:
		case ActionPaymentSucceeded, ActionPaymentFailed, ActionPaymentReversed:
			if !ex.PaymentID.Valid {
				return fmt.Errorf("%w: exception has no payment", ErrInvalid)
			}
			to := map[Action]payments.Status{ActionPaymentSucceeded: payments.StatusSucceeded,
				ActionPaymentFailed: payments.StatusFailed, ActionPaymentReversed: payments.StatusReversed}[action]
			if err := s.payments.ReconcileTx(ctx, q, ex.PaymentID, to, actor, note, requestID); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			// With the outcome corrected, the statement line is matched again
			// on the next run.
			if ex.SettlementLineID.Valid {
				if err := q.SetSettlementLineStatus(ctx, store.SetSettlementLineStatusParams{ID: ex.SettlementLineID, MatchStatus: "pending"}); err != nil {
					return err
				}
			}
		case ActionRematch:
			if !ex.SettlementLineID.Valid {
				return fmt.Errorf("%w: exception has no statement line", ErrInvalid)
			}
			if err := q.SetSettlementLineStatus(ctx, store.SetSettlementLineStatusParams{ID: ex.SettlementLineID, MatchStatus: "pending"}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unknown action %q", ErrInvalid, action)
		}
		resolution := string(action) + ": " + note
		if n, err := q.ResolveReconException(ctx, store.ResolveReconExceptionParams{ID: ex.ID, Now: s.now(), ResolvedBy: &actor, Resolution: &resolution}); err != nil || n != 1 {
			if err == nil {
				err = fmt.Errorf("%w: exception changed concurrently", ErrInvalid)
			}
			return err
		}
		return audit.Record(ctx, q, operator, "reconciliation.resolve", "reconciliation_exception", ex.PublicID,
			map[string]any{"action": action, "note": note, "type": ex.Type}, requestID)
	})
	if err != nil {
		return store.ReconciliationException{}, err
	}
	return s.q.GetReconException(ctx, exceptionID)
}

// Exceptions lists exceptions, optionally only open ones.
func (s *Service) Exceptions(ctx context.Context, onlyOpen bool, max int32) ([]store.ReconciliationException, error) {
	var status *string
	if onlyOpen {
		open := "open"
		status = &open
	}
	return s.q.ListReconExceptions(ctx, store.ListReconExceptionsParams{Status: status, MaxRows: max})
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pgCode(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
