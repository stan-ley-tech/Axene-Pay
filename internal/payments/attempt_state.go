package payments

import "fmt"

// AttemptStatus is the provider-execution state of one payment attempt.
type AttemptStatus string

const (
	AttemptPendingSubmission AttemptStatus = "PENDING_SUBMISSION"
	AttemptSubmitted         AttemptStatus = "SUBMITTED"
	AttemptSubmissionUnknown AttemptStatus = "SUBMISSION_UNKNOWN"
	AttemptRejected          AttemptStatus = "REJECTED"
	AttemptConfirmedSuccess  AttemptStatus = "CONFIRMED_SUCCESS"
	AttemptConfirmedFailure  AttemptStatus = "CONFIRMED_FAILURE"
	AttemptUnresolved        AttemptStatus = "UNRESOLVED"
	// AttemptAbandoned: never sent to the provider because the payment was
	// cancelled first.
	AttemptAbandoned AttemptStatus = "ABANDONED"
)

// AllAttemptStatuses lists every attempt status.
var AllAttemptStatuses = []AttemptStatus{
	AttemptPendingSubmission, AttemptSubmitted, AttemptSubmissionUnknown, AttemptRejected,
	AttemptConfirmedSuccess, AttemptConfirmedFailure, AttemptUnresolved, AttemptAbandoned,
}

// Valid reports whether s is a known attempt status.
func (s AttemptStatus) Valid() bool {
	for _, v := range AllAttemptStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// IsOpen reports whether the attempt still awaits a provider outcome. While a
// payment has an open attempt, no other attempt may be created for it.
func (s AttemptStatus) IsOpen() bool {
	switch s {
	case AttemptPendingSubmission, AttemptSubmitted, AttemptSubmissionUnknown:
		return true
	}
	return false
}

var attemptTransitions = map[AttemptStatus][]AttemptStatus{
	AttemptPendingSubmission: {AttemptSubmitted, AttemptSubmissionUnknown, AttemptRejected, AttemptAbandoned},
	// A status query can reveal that an in-doubt submission was received.
	AttemptSubmissionUnknown: {AttemptSubmitted, AttemptConfirmedSuccess, AttemptConfirmedFailure, AttemptUnresolved},
	AttemptSubmitted:         {AttemptConfirmedSuccess, AttemptConfirmedFailure, AttemptUnresolved},
	AttemptUnresolved:        {AttemptConfirmedSuccess, AttemptConfirmedFailure},
}

// CheckAttemptTransition validates an attempt status change. Re-applying the
// current status is a no-op.
func CheckAttemptTransition(from, to AttemptStatus) (noop bool, err error) {
	if !from.Valid() || !to.Valid() {
		return false, fmt.Errorf("%w: unknown attempt status %q -> %q", ErrInvalidTransition, from, to)
	}
	if from == to {
		return true, nil
	}
	for _, a := range attemptTransitions[from] {
		if a == to {
			return false, nil
		}
	}
	return false, fmt.Errorf("%w: attempt %s -> %s", ErrInvalidTransition, from, to)
}
