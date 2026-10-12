package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// VerifyDue claims up to max attempts whose verification is due and
// resolves each with an authenticated status query. It returns how many
// attempts were processed. Errors for individual attempts are logged; their
// lease expires and they are retried.
func (s *Service) VerifyDue(ctx context.Context, max int32) (int, error) {
	now := s.now()
	due, err := s.q.ClaimDueVerifications(ctx, store.ClaimDueVerificationsParams{
		LeaseUntil: now.Add(s.policy.VerificationLease), Now: now, MaxRows: max,
	})
	if err != nil {
		return 0, fmt.Errorf("payments: claim verifications: %w", err)
	}
	for _, a := range due {
		if err := s.verifyAttempt(ctx, a); err != nil {
			s.log.ErrorContext(ctx, "verify attempt", "attempt_id", a.PublicID, "err", err)
		}
	}
	return len(due), nil
}

// verdict is what a status query result means for an attempt.
type verdict struct {
	attempt  AttemptStatus
	payment  Status // empty: payment unchanged
	evidence Evidence
	reason   string
	// exception, when set, opens a reconciliation exception of this type.
	exception string
	retryAt   *time.Time
	receipt   *string
}

func (s *Service) verifyAttempt(ctx context.Context, a store.PaymentAttempt) error {
	requestID := fmt.Sprintf("verify-%s-%d", a.PublicID, a.VerificationCount+1)
	payment, err := s.q.GetPaymentByID(ctx, a.PaymentID)
	if err != nil {
		return err
	}
	provider, perr := s.providers.Get(a.Provider)

	var out providers.StatusOutcome
	var v verdict
	switch {
	case perr != nil || !provider.Capabilities().Has(providers.CapStatusQuery):
		// ADR-0009: without a status query there is no verified evidence;
		// reconciliation against provider records must resolve it.
		v = verdict{attempt: AttemptUnresolved, payment: StatusUnresolved, evidence: EvidenceVerificationExhausted,
			reason: "provider cannot be queried for status", exception: "verification_exhausted"}
	default:
		dest, err := s.q.GetSettlementDestinationByID(ctx, payment.SettlementDestinationID)
		if err != nil {
			return err
		}
		ref := ""
		if a.ProviderReference != nil {
			ref = *a.ProviderReference
		}
		callCtx, cancel := context.WithTimeout(ctx, s.policy.ProviderTimeout)
		started := s.now()
		var qerr error
		out, qerr = provider.QueryStatus(callCtx, providers.AttemptRef{
			AttemptID: a.PublicID, ProviderReference: ref,
			Destination: providers.Destination{Type: dest.Type, Identifier: dest.Identifier},
		})
		cancel()
		if qerr != nil {
			s.log.ErrorContext(ctx, "status query failed", "attempt_id", a.PublicID, "err", qerr)
			out = providers.StatusOutcome{Class: providers.ClassUnknown, RawMessage: qerr.Error()}
		}
		s.metrics.ProviderCall(a.Provider, "status_query", string(out.Class), s.now().Sub(started))
		v = s.judge(a, payment, out)
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		payment, err := q.LockPayment(ctx, a.PaymentID)
		if err != nil {
			return err
		}
		cur, err := q.GetAttempt(ctx, a.ID)
		if err != nil {
			return err
		}
		st := Status(payment.Status)
		awaiting := st == StatusPendingProvider || st == StatusPendingVerification
		if !awaiting || (cur.Status != string(AttemptSubmitted) && cur.Status != string(AttemptSubmissionUnknown)) {
			// Resolved meanwhile (e.g. by reconciliation): stop verifying.
			return s.setAttempt(ctx, q, cur, AttemptStatus(cur.Status), attemptUpdate{})
		}
		// The verdict was computed from a snapshot; re-judge if a callback
		// changed the claimed outcome since.
		if ptrStr(cur.ClaimedOutcome) != ptrStr(a.ClaimedOutcome) && perr == nil && provider.Capabilities().Has(providers.CapStatusQuery) {
			v = s.judge(cur, payment, out)
		}
		now := s.now()
		upd := attemptUpdate{rawCode: nonEmpty(out.RawCode), rawMessage: nonEmpty(out.RawMessage), receipt: v.receipt, verificationIncrement: 1}
		if v.payment == "" { // inconclusive: retry later
			upd.nextVerification = v.retryAt
			s.metrics.Verification(cur.Provider, "retry")
			return s.setAttempt(ctx, q, cur, v.attempt, upd)
		}
		if v.attempt == AttemptConfirmedSuccess || v.attempt == AttemptConfirmedFailure {
			upd.resolvedAt = &now
		}
		if err := s.setAttempt(ctx, q, cur, v.attempt, upd); err != nil {
			return err
		}
		if _, err := s.transition(ctx, q, payment, change{to: v.payment, evidence: v.evidence, actor: "worker:verification",
			reason: v.reason, attemptID: cur.ID, requestID: requestID}); err != nil {
			return err
		}
		if v.exception != "" {
			if err := s.exception(ctx, q, v.exception, payment.ID, cur.ID, pgtype.UUID{},
				map[string]any{"reason": v.reason, "claimed": ptrStr(cur.ClaimedOutcome), "status_class": out.Class,
					"raw_code": out.RawCode, "verification_count": cur.VerificationCount + 1}); err != nil {
				return err
			}
		}
		s.metrics.Verification(cur.Provider, string(v.payment))
		return nil
	})
}

// judge maps a status query result to a verdict (docs/payment-lifecycle.md §4).
func (s *Service) judge(a store.PaymentAttempt, p store.Payment, out providers.StatusOutcome) verdict {
	claimed := ptrStr(a.ClaimedOutcome)
	switch out.Class {
	case providers.ClassSucceeded:
		if out.Amount != nil && (out.Amount.Minor() != p.AmountMinor || out.Amount.Currency().Code() != p.Currency) {
			return verdict{attempt: AttemptUnresolved, payment: StatusUnresolved, evidence: EvidenceConflict,
				reason:    fmt.Sprintf("status query reports %s, payment is %d %s", out.Amount, p.AmountMinor, p.Currency),
				exception: "evidence_mismatch"}
		}
		if claimed == string(providers.ClassDeclined) || claimed == string(providers.ClassRejected) {
			return verdict{attempt: AttemptUnresolved, payment: StatusUnresolved, evidence: EvidenceConflict,
				reason: "callback claimed failure but status query reports success", exception: "evidence_conflict"}
		}
		return verdict{attempt: AttemptConfirmedSuccess, payment: StatusSucceeded, evidence: EvidenceStatusQuery,
			reason: "confirmed by status query", receipt: nonEmpty(out.ProviderReceipt)}
	case providers.ClassDeclined, providers.ClassRejected:
		if claimed == string(providers.ClassSucceeded) {
			return verdict{attempt: AttemptUnresolved, payment: StatusUnresolved, evidence: EvidenceConflict,
				reason: "callback claimed success but status query reports failure", exception: "evidence_conflict"}
		}
		return verdict{attempt: AttemptConfirmedFailure, payment: StatusFailed, evidence: EvidenceStatusQuery,
			reason: "provider reports " + string(out.Class) + ": " + out.RawMessage}
	}
	// Accepted (still in progress) or unknown: retry with backoff, never fail.
	n := int(a.VerificationCount) + 1
	next := AttemptStatus(a.Status)
	if out.Class == providers.ClassAccepted && next == AttemptSubmissionUnknown {
		next = AttemptSubmitted // the provider has the request
	}
	if n >= s.policy.MaxVerificationAttempts {
		return verdict{attempt: AttemptUnresolved, payment: StatusUnresolved, evidence: EvidenceVerificationExhausted,
			reason: fmt.Sprintf("no definitive status after %d queries", n), exception: "verification_exhausted"}
	}
	at := s.now().Add(s.policy.backoff(n - 1))
	return verdict{attempt: next, retryAt: &at}
}

// SweepSubmissions finishes submissions interrupted by a crash: attempts
// never claimed are submitted; attempts claimed by a process that stopped
// before recording an outcome become SUBMISSION_UNKNOWN (never resubmitted).
func (s *Service) SweepSubmissions(ctx context.Context, max int32) error {
	now := s.now()
	unclaimed, err := s.q.ListUnclaimedSubmissions(ctx, store.ListUnclaimedSubmissionsParams{
		CreatedBefore: now.Add(-s.policy.UnclaimedSubmissionGrace), MaxRows: max,
	})
	if err != nil {
		return err
	}
	for _, a := range unclaimed {
		if err := s.SubmitAttempt(ctx, a.ID, "sweep-"+a.PublicID); err != nil {
			s.log.ErrorContext(ctx, "sweep submit", "attempt_id", a.PublicID, "err", err)
		}
	}
	expired, err := s.q.ListExpiredSubmissionClaims(ctx, store.ListExpiredSubmissionClaimsParams{
		ClaimedBefore: now.Add(-s.policy.SubmissionLease), MaxRows: max,
	})
	if err != nil {
		return err
	}
	for _, a := range expired {
		if err := s.markSubmissionUnknown(ctx, a); err != nil {
			s.log.ErrorContext(ctx, "mark submission unknown", "attempt_id", a.PublicID, "err", err)
		}
	}
	return nil
}

func (s *Service) markSubmissionUnknown(ctx context.Context, a store.PaymentAttempt) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		payment, err := q.LockPayment(ctx, a.PaymentID)
		if err != nil {
			return err
		}
		cur, err := q.GetAttempt(ctx, a.ID)
		if err != nil || AttemptStatus(cur.Status) != AttemptPendingSubmission {
			return err
		}
		upd := attemptUpdate{rawMessage: nonEmpty("submission interrupted before an outcome was recorded")}
		if p, err := s.providers.Get(cur.Provider); err == nil && p.Capabilities().Has(providers.CapStatusQuery) {
			now := s.now()
			upd.nextVerification = &now
		}
		if err := s.setAttempt(ctx, q, cur, AttemptSubmissionUnknown, upd); err != nil {
			return err
		}
		_, err = s.transition(ctx, q, payment, change{to: StatusPendingProvider, evidence: EvidenceSubmission,
			actor: "worker:submission", reason: "submission interrupted; outcome unknown", attemptID: cur.ID,
			requestID: "sweep-" + cur.PublicID})
		return err
	})
}

// PruneIdempotencyKeys deletes up to max expired idempotency keys.
func (s *Service) PruneIdempotencyKeys(ctx context.Context, max int32) (int64, error) {
	return s.q.PruneIdempotencyKeys(ctx, store.PruneIdempotencyKeysParams{Now: s.now(), MaxRows: max})
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
