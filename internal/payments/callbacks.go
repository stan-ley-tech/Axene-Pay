package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// ErrUnknownProvider means a callback named a provider this deployment does
// not have.
var ErrUnknownProvider = errors.New("payments: unknown provider")

// IngestCallback durably records a provider callback and, when it matches a
// payment awaiting the provider, moves that payment to PENDING_VERIFICATION
// (ADR-0009). It never produces a terminal state. Duplicate and late
// callbacks are recorded without side effects; contradictions and unknown
// references open reconciliation exceptions.
//
// A nil error means the callback is durably stored and the provider may be
// told it was received. A wrapped providers.ErrCallbackRejected means it was
// stored but could not be parsed or authenticated.
func (s *Service) IngestCallback(ctx context.Context, providerName string, req providers.CallbackRequest, requestID string) error {
	p, err := s.providers.Get(providerName)
	if err != nil {
		return ErrUnknownProvider
	}
	ev, perr := p.ParseCallback(ctx, req)
	if perr != nil {
		msg := perr.Error()
		if _, err := s.q.InsertOriginalCallback(ctx, store.InsertOriginalCallbackParams{
			PublicID: ids.New(ids.Callback), Provider: providerName, ParseStatus: "rejected",
			ParseError: &msg, RawBody: req.Body, RequestID: requestID,
		}); err != nil {
			return fmt.Errorf("payments: store rejected callback: %w", err)
		}
		s.metrics.Callback(providerName, "rejected")
		s.log.WarnContext(ctx, "provider callback rejected", "request_id", requestID, "provider", providerName, "err", perr)
		return perr
	}

	result := "accepted"
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		attempt, err := q.GetAttemptByProviderReference(ctx, store.GetAttemptByProviderReferenceParams{
			Provider: providerName, ProviderReference: &ev.ProviderReference,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			result = "unmatched"
			cbID, original, err := s.storeCallback(ctx, q, providerName, ev, req, pgtype.UUID{}, requestID)
			if err != nil || !original {
				if !original {
					result = "duplicate"
				}
				return err
			}
			return s.exception(ctx, q, "unmatched_callback", pgtype.UUID{}, pgtype.UUID{}, cbID,
				map[string]any{"provider_reference": ev.ProviderReference, "claimed": ev.Claimed})
		}
		if err != nil {
			return err
		}
		// Serialize with every other change to this payment.
		payment, err := q.LockPayment(ctx, attempt.PaymentID)
		if err != nil {
			return err
		}
		cbID, original, err := s.storeCallback(ctx, q, providerName, ev, req, attempt.ID, requestID)
		if err != nil {
			return err
		}
		if !original {
			result = "duplicate"
			return nil
		}

		switch st := Status(payment.Status); {
		case st == StatusPendingProvider:
			// Verify immediately. Without a status query capability the
			// verification step marks the payment UNRESOLVED for reconciliation.
			next := s.now()
			claimed := string(ev.Claimed)
			if err := q.RecordClaimedOutcome(ctx, store.RecordClaimedOutcomeParams{
				ID: attempt.ID, ClaimedOutcome: &claimed, Now: s.now(), NextVerificationAt: &next,
			}); err != nil {
				return err
			}
			_, err := s.transition(ctx, q, payment, change{
				to: StatusPendingVerification, evidence: EvidenceCallback, actor: "provider:" + providerName,
				reason: "callback claims " + string(ev.Claimed), attemptID: attempt.ID, callbackID: cbID, requestID: requestID,
			})
			return err
		case st.IsTerminal() && !agrees(st, ev.Claimed):
			result = "late_conflict"
			return s.exception(ctx, q, "late_callback_conflict", payment.ID, attempt.ID, cbID,
				map[string]any{"payment_status": st, "claimed": ev.Claimed})
		default:
			// Already being verified, unresolved, or a late callback that agrees
			// with the outcome: the stored record is evidence for reconciliation.
			result = "recorded"
			return nil
		}
	})
	if err != nil {
		return fmt.Errorf("payments: ingest callback: %w", err)
	}
	s.metrics.Callback(providerName, result)
	s.log.InfoContext(ctx, "provider callback", "request_id", requestID, "provider", providerName, "result", result)
	return nil
}

// storeCallback inserts the callback as the original for its dedup key, or
// as a duplicate pointing at the original.
func (s *Service) storeCallback(ctx context.Context, q *store.Queries, provider string, ev providers.CallbackEvent, req providers.CallbackRequest, attemptID pgtype.UUID, requestID string) (pgtype.UUID, bool, error) {
	claimed := string(ev.Claimed)
	row, err := q.InsertOriginalCallback(ctx, store.InsertOriginalCallbackParams{
		PublicID: ids.New(ids.Callback), Provider: provider, ParseStatus: "parsed", DedupKey: &ev.DedupKey,
		ProviderReference: &ev.ProviderReference, ClaimedOutcome: &claimed, AttemptID: attemptID,
		RawBody: req.Body, RequestID: requestID,
	})
	if err == nil {
		return row.ID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, err
	}
	dup, err := q.InsertDuplicateCallback(ctx, store.InsertDuplicateCallbackParams{
		PublicID: ids.New(ids.Callback), Provider: provider, DedupKey: ev.DedupKey,
		ProviderReference: &ev.ProviderReference, ClaimedOutcome: &claimed, AttemptID: attemptID,
		RawBody: req.Body, RequestID: requestID,
	})
	return dup.ID, false, err
}

// agrees reports whether a claimed outcome is consistent with a terminal status.
func agrees(st Status, claimed providers.Class) bool {
	switch st {
	case StatusSucceeded, StatusReversed:
		return claimed == providers.ClassSucceeded
	case StatusFailed, StatusCancelled:
		return claimed == providers.ClassDeclined || claimed == providers.ClassRejected
	}
	return false
}

// exception opens a reconciliation exception; repeated detection of the same
// condition is a no-op (unique indexes).
func (s *Service) exception(ctx context.Context, q *store.Queries, kind string, paymentID, attemptID, callbackID pgtype.UUID, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	n, err := q.InsertReconException(ctx, store.InsertReconExceptionParams{
		PublicID: ids.New(ids.ReconException), Type: kind, PaymentID: paymentID, AttemptID: attemptID,
		CallbackID: callbackID, Detail: b,
	})
	if err != nil {
		return err
	}
	if n == 1 {
		s.metrics.ReconException(kind)
		s.log.WarnContext(ctx, "reconciliation exception opened", "type", kind)
	}
	return nil
}
