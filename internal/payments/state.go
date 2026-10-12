// Package payments owns the payment resource: its lifecycle state machine,
// provider attempts, callback ingestion, and verification workflow.
//
// The state machine is documented in docs/payment-lifecycle.md. Every
// transition must name the evidence that justifies it; terminal outcomes
// require verified evidence, so a provider callback alone can never mark a
// payment succeeded or failed (ADR-0009).
package payments

import (
	"errors"
	"fmt"
)

// Status is the merchant-visible payment state.
type Status string

const (
	StatusCreated             Status = "CREATED"
	StatusPendingProvider     Status = "PENDING_PROVIDER"
	StatusPendingVerification Status = "PENDING_VERIFICATION"
	StatusUnresolved          Status = "UNRESOLVED"
	StatusSucceeded           Status = "SUCCEEDED"
	StatusFailed              Status = "FAILED"
	StatusCancelled           Status = "CANCELLED"
	StatusReversed            Status = "REVERSED"
)

// AllStatuses lists every payment status, in lifecycle order.
var AllStatuses = []Status{
	StatusCreated, StatusPendingProvider, StatusPendingVerification, StatusUnresolved,
	StatusSucceeded, StatusFailed, StatusCancelled, StatusReversed,
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, v := range AllStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s is a final outcome. SUCCEEDED is terminal for
// the payment flow but may still be reversed.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusReversed:
		return true
	}
	return false
}

// Evidence is the justification for a transition.
type Evidence string

const (
	// EvidenceSubmission: a provider request was sent, acknowledged, or its
	// delivery is in doubt.
	EvidenceSubmission Evidence = "submission"
	// EvidenceSubmissionRejected: the provider's authenticated synchronous
	// response refused the request before execution.
	EvidenceSubmissionRejected Evidence = "submission_rejected"
	// EvidenceRouting: no eligible provider could be selected.
	EvidenceRouting Evidence = "routing"
	// EvidenceMerchant: an authenticated merchant action (cancel).
	EvidenceMerchant Evidence = "merchant"
	// EvidenceCallback: an unauthenticated provider callback. Never sufficient
	// for a terminal state.
	EvidenceCallback Evidence = "callback"
	// EvidenceStatusQuery: a matching, definitive authenticated status query.
	EvidenceStatusQuery Evidence = "status_query"
	// EvidenceReconciliation: an authorized reconciliation resolution.
	EvidenceReconciliation Evidence = "reconciliation"
	// EvidenceVerificationExhausted: automatic verification gave up.
	EvidenceVerificationExhausted Evidence = "verification_exhausted"
	// EvidenceConflict: callback and status query (or amounts/references) disagree.
	EvidenceConflict Evidence = "evidence_conflict"
	// EvidenceReversal: a confirmed provider reversal (Phase 3+).
	EvidenceReversal Evidence = "reversal"
)

// AllEvidence lists every evidence kind.
var AllEvidence = []Evidence{
	EvidenceSubmission, EvidenceSubmissionRejected, EvidenceRouting, EvidenceMerchant,
	EvidenceCallback, EvidenceStatusQuery, EvidenceReconciliation,
	EvidenceVerificationExhausted, EvidenceConflict, EvidenceReversal,
}

type edge struct{ from, to Status }

// transitions is the complete table from docs/payment-lifecycle.md §2.
var transitions = map[edge][]Evidence{
	{StatusCreated, StatusPendingProvider}: {EvidenceSubmission},
	{StatusCreated, StatusFailed}:          {EvidenceSubmissionRejected, EvidenceRouting},
	{StatusCreated, StatusCancelled}:       {EvidenceMerchant},

	{StatusPendingProvider, StatusPendingVerification}: {EvidenceCallback},
	{StatusPendingProvider, StatusSucceeded}:           {EvidenceStatusQuery, EvidenceReconciliation},
	{StatusPendingProvider, StatusFailed}:              {EvidenceStatusQuery, EvidenceReconciliation},
	{StatusPendingProvider, StatusUnresolved}:          {EvidenceVerificationExhausted, EvidenceConflict},

	{StatusPendingVerification, StatusSucceeded}:  {EvidenceStatusQuery, EvidenceReconciliation},
	{StatusPendingVerification, StatusFailed}:     {EvidenceStatusQuery, EvidenceReconciliation},
	{StatusPendingVerification, StatusUnresolved}: {EvidenceVerificationExhausted, EvidenceConflict},

	{StatusUnresolved, StatusSucceeded}: {EvidenceReconciliation},
	{StatusUnresolved, StatusFailed}:    {EvidenceReconciliation},

	{StatusSucceeded, StatusReversed}: {EvidenceReconciliation, EvidenceReversal},
}

var (
	// ErrInvalidTransition means the (from, to) pair is not in the lifecycle.
	ErrInvalidTransition = errors.New("payments: invalid status transition")
	// ErrEvidenceNotAccepted means the pair exists but the evidence cannot justify it.
	ErrEvidenceNotAccepted = errors.New("payments: evidence not accepted for transition")
)

// CheckTransition validates moving from -> to on the given evidence.
//
// It returns noop=true when from == to: re-applying the current state (a
// duplicate callback, a repeated status query) is not an error and must not
// produce side effects.
func CheckTransition(from, to Status, ev Evidence) (noop bool, err error) {
	if !from.Valid() || !to.Valid() {
		return false, fmt.Errorf("%w: unknown status %q -> %q", ErrInvalidTransition, from, to)
	}
	if from == to {
		return true, nil
	}
	allowed, ok := transitions[edge{from, to}]
	if !ok {
		return false, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	for _, a := range allowed {
		if a == ev {
			return false, nil
		}
	}
	return false, fmt.Errorf("%w: %s -> %s on %q", ErrEvidenceNotAccepted, from, to, ev)
}
