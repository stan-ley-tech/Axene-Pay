// Package accounting records the financial consequences of payment outcomes
// under the funds flow in force (direct merchant settlement, ADR-0008):
// the settlement memorandum posting and Axene's fee accrual. It runs inside
// the payment transition's transaction, so an outcome and its financial
// effects commit together or not at all, exactly once.
package accounting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stan-ley-tech/axene-pay/internal/fees"
	"github.com/stan-ley-tech/axene-pay/internal/ledger"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Effects implements payments.FinancialEffects.
type Effects struct {
	Fees *fees.Service
}

var _ payments.FinancialEffects = (*Effects)(nil)

type facts struct {
	payment     store.Payment
	merchant    store.Merchant
	destination store.SettlementDestination
	provider    string
	amount      money.Amount
}

func load(ctx context.Context, q *store.Queries, p store.Payment) (facts, error) {
	m, err := q.GetMerchantByID(ctx, p.MerchantID)
	if err != nil {
		return facts{}, err
	}
	d, err := q.GetSettlementDestinationByID(ctx, p.SettlementDestinationID)
	if err != nil {
		return facts{}, err
	}
	attempts, err := q.ListAttemptsForPayment(ctx, p.ID)
	if err != nil {
		return facts{}, err
	}
	if len(attempts) == 0 {
		return facts{}, errors.New("accounting: payment has no attempts")
	}
	cur, err := money.ParseCurrency(p.Currency)
	if err != nil {
		return facts{}, err
	}
	amt, err := money.New(p.AmountMinor, cur)
	if err != nil {
		return facts{}, err
	}
	return facts{payment: p, merchant: m, destination: d, provider: attempts[len(attempts)-1].Provider, amount: amt}, nil
}

func memoLines(f facts) []ledger.Line {
	return []ledger.Line{
		{Account: ledger.Account{Book: ledger.BookSettlementMemo, Code: "settlement_expected:" + f.merchant.PublicID + ":" + f.destination.PublicID,
			Type: ledger.Memo, MerchantID: f.merchant.ID}, Direction: ledger.Debit, Amount: f.amount},
		{Account: ledger.Account{Book: ledger.BookSettlementMemo, Code: "processed_volume:" + f.merchant.PublicID,
			Type: ledger.Memo, MerchantID: f.merchant.ID}, Direction: ledger.Credit, Amount: f.amount},
	}
}

// PaymentSucceeded posts the expected settlement and accrues Axene's fee.
func (e *Effects) PaymentSucceeded(ctx context.Context, q *store.Queries, p store.Payment, requestID string) error {
	f, err := load(ctx, q, p)
	if err != nil {
		return err
	}
	if _, err := ledger.Post(ctx, q, ledger.Entry{
		Book: ledger.BookSettlementMemo, Reason: ledger.ReasonPaymentSucceeded, SourceType: "payment", SourceID: p.PublicID,
		Description: "verified payment; settlement expected at " + f.destination.Type + " " + f.destination.Identifier,
		EffectiveAt: p.UpdatedAt, RequestID: requestID, Lines: memoLines(f),
	}); err != nil {
		return fmt.Errorf("accounting: memo posting: %w", err)
	}
	_, err = e.Fees.Accrue(ctx, q, fees.PaymentFacts{
		PaymentID: p.ID, PaymentPublicID: p.PublicID, MerchantID: p.MerchantID, MerchantPublicID: f.merchant.PublicID,
		Environment: p.Environment, Method: p.Method, Provider: f.provider, Amount: f.amount, CreatedAt: p.CreatedAt,
	}, requestID)
	if errors.Is(err, fees.ErrNoSchedule) {
		// Never a silent zero fee: surface the gap for an operator.
		return exception(ctx, q, "fee_schedule_missing", p.ID, map[string]any{
			"reason": "no active fee schedule covered this payment when it was created", "provider": f.provider,
		})
	}
	return err
}

// PaymentReversed compensates the settlement memo and applies the fee's
// reversal policy.
func (e *Effects) PaymentReversed(ctx context.Context, q *store.Queries, p store.Payment, requestID string) error {
	f, err := load(ctx, q, p)
	if err != nil {
		return err
	}
	if _, err := ledger.Reverse(ctx, q, "payment", p.PublicID, ledger.ReasonPaymentSucceeded, ledger.ReasonPaymentReversed,
		"payment reversed", requestID, p.UpdatedAt); err != nil {
		return fmt.Errorf("accounting: reverse memo: %w", err)
	}
	out, err := e.Fees.ApplyReversal(ctx, q, p.ID, f.merchant.PublicID, requestID)
	if err != nil {
		return err
	}
	if out == fees.ReversalReviewRequired {
		return exception(ctx, q, "fee_policy_required", p.ID, map[string]any{
			"reason": "payment reversed; fee needs a decision under its reversal policy or a billing adjustment",
		})
	}
	return nil
}

func exception(ctx context.Context, q *store.Queries, kind string, paymentID pgtype.UUID, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.InsertReconException(ctx, store.InsertReconExceptionParams{
		PublicID: ids.New(ids.ReconException), Type: kind, PaymentID: paymentID, Detail: b,
	})
	return err
}
