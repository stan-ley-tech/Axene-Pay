package fees

import (
	"errors"
	"fmt"
)

// Status is the lifecycle state of an Axene fee (docs/ledger.md §4).
type Status string

const (
	StatusAccrued           Status = "accrued"
	StatusCollectionPending Status = "collection_pending"
	StatusCollected         Status = "collected"
	StatusCollectionFailed  Status = "collection_failed"
	StatusWaived            Status = "waived"
	StatusRefunded          Status = "refunded"
	StatusReviewRequired    Status = "review_required"
)

// AllStatuses lists every fee status.
var AllStatuses = []Status{
	StatusAccrued, StatusCollectionPending, StatusCollected, StatusCollectionFailed,
	StatusWaived, StatusRefunded, StatusReviewRequired,
}

// IsTerminal reports whether no further transition is allowed.
func (s Status) IsTerminal() bool { return s == StatusWaived || s == StatusRefunded }

var transitions = map[Status][]Status{
	// Settlement evidence can show a fee collected without an active
	// collection step (e.g. a provider-reported split).
	StatusAccrued:           {StatusCollectionPending, StatusCollected, StatusWaived, StatusRefunded, StatusReviewRequired},
	StatusCollectionPending: {StatusCollected, StatusCollectionFailed, StatusReviewRequired},
	StatusCollectionFailed:  {StatusCollectionPending, StatusCollected, StatusWaived, StatusReviewRequired},
	// A collected fee affected by a reversal needs a billing adjustment:
	// never silently "refunded".
	StatusCollected:      {StatusReviewRequired},
	StatusReviewRequired: {StatusAccrued, StatusCollected, StatusWaived, StatusRefunded},
}

// ErrInvalidTransition means the fee cannot move between the two states.
var ErrInvalidTransition = errors.New("fees: invalid fee status transition")

// CheckTransition validates a fee status change; re-applying the current
// status is a no-op.
func CheckTransition(from, to Status) (noop bool, err error) {
	if from == to {
		return true, nil
	}
	for _, t := range transitions[from] {
		if t == to {
			return false, nil
		}
	}
	return false, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}
