package payments

import (
	"errors"
	"fmt"
	"testing"
)

// allowedPaymentTransitions restates docs/payment-lifecycle.md §2 as literal
// strings, independently of the implementation table, so that a change to
// either one without the other fails this test.
var allowedPaymentTransitions = map[string]bool{
	"CREATED>PENDING_PROVIDER:submission":                    true,
	"CREATED>FAILED:submission_rejected":                     true,
	"CREATED>FAILED:routing":                                 true,
	"CREATED>CANCELLED:merchant":                             true,
	"PENDING_PROVIDER>PENDING_VERIFICATION:callback":         true,
	"PENDING_PROVIDER>SUCCEEDED:status_query":                true,
	"PENDING_PROVIDER>SUCCEEDED:reconciliation":              true,
	"PENDING_PROVIDER>FAILED:status_query":                   true,
	"PENDING_PROVIDER>FAILED:reconciliation":                 true,
	"PENDING_PROVIDER>UNRESOLVED:verification_exhausted":     true,
	"PENDING_VERIFICATION>SUCCEEDED:status_query":            true,
	"PENDING_VERIFICATION>SUCCEEDED:reconciliation":          true,
	"PENDING_VERIFICATION>FAILED:status_query":               true,
	"PENDING_VERIFICATION>FAILED:reconciliation":             true,
	"PENDING_VERIFICATION>UNRESOLVED:verification_exhausted": true,
	"PENDING_VERIFICATION>UNRESOLVED:evidence_conflict":      true,
	"UNRESOLVED>SUCCEEDED:reconciliation":                    true,
	"UNRESOLVED>FAILED:reconciliation":                       true,
	"SUCCEEDED>REVERSED:reconciliation":                      true,
	"SUCCEEDED>REVERSED:reversal":                            true,
}

func TestCheckTransitionExhaustive(t *testing.T) {
	t.Parallel()
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			for _, ev := range AllEvidence {
				key := fmt.Sprintf("%s>%s:%s", from, to, ev)
				noop, err := CheckTransition(from, to, ev)
				switch {
				case from == to:
					if !noop || err != nil {
						t.Errorf("%s: want noop, got noop=%v err=%v", key, noop, err)
					}
				case allowedPaymentTransitions[key]:
					if noop || err != nil {
						t.Errorf("%s: want allowed, got noop=%v err=%v", key, noop, err)
					}
				default:
					if err == nil {
						t.Errorf("%s: want rejection, got allowed", key)
					}
				}
			}
		}
	}
}

func TestCallbackNeverProducesTerminalState(t *testing.T) {
	t.Parallel()
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			if !to.IsTerminal() || from == to {
				continue
			}
			if _, err := CheckTransition(from, to, EvidenceCallback); err == nil {
				t.Errorf("callback allowed %s -> %s", from, to)
			}
		}
	}
}

func TestTerminalStatesAreFinalExceptReversal(t *testing.T) {
	t.Parallel()
	for _, from := range AllStatuses {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range AllStatuses {
			if from == to {
				continue
			}
			for _, ev := range AllEvidence {
				_, err := CheckTransition(from, to, ev)
				if err == nil && !(from == StatusSucceeded && to == StatusReversed) {
					t.Errorf("terminal %s left for %s on %s", from, to, ev)
				}
			}
		}
	}
}

func TestCheckTransitionErrorKinds(t *testing.T) {
	t.Parallel()
	if _, err := CheckTransition(StatusCreated, StatusSucceeded, EvidenceStatusQuery); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unlisted pair: err = %v", err)
	}
	if _, err := CheckTransition(StatusPendingProvider, StatusSucceeded, EvidenceCallback); !errors.Is(err, ErrEvidenceNotAccepted) {
		t.Errorf("wrong evidence: err = %v", err)
	}
	if _, err := CheckTransition("BOGUS", StatusSucceeded, EvidenceStatusQuery); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown status: err = %v", err)
	}
	if _, err := CheckTransition(StatusCreated, "BOGUS", EvidenceStatusQuery); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown target: err = %v", err)
	}
}

func TestStatusPredicates(t *testing.T) {
	t.Parallel()
	terminal := map[Status]bool{StatusSucceeded: true, StatusFailed: true, StatusCancelled: true, StatusReversed: true}
	for _, s := range AllStatuses {
		if !s.Valid() {
			t.Errorf("%s not valid", s)
		}
		if s.IsTerminal() != terminal[s] {
			t.Errorf("%s IsTerminal = %v", s, s.IsTerminal())
		}
	}
	if Status("succeeded").Valid() {
		t.Error("lower-case status accepted")
	}
}

var allowedAttemptTransitions = map[string]bool{
	"PENDING_SUBMISSION>SUBMITTED":          true,
	"PENDING_SUBMISSION>SUBMISSION_UNKNOWN": true,
	"PENDING_SUBMISSION>REJECTED":           true,
	"SUBMISSION_UNKNOWN>SUBMITTED":          true,
	"SUBMISSION_UNKNOWN>CONFIRMED_SUCCESS":  true,
	"SUBMISSION_UNKNOWN>CONFIRMED_FAILURE":  true,
	"SUBMISSION_UNKNOWN>UNRESOLVED":         true,
	"SUBMITTED>CONFIRMED_SUCCESS":           true,
	"SUBMITTED>CONFIRMED_FAILURE":           true,
	"SUBMITTED>UNRESOLVED":                  true,
	"UNRESOLVED>CONFIRMED_SUCCESS":          true,
	"UNRESOLVED>CONFIRMED_FAILURE":          true,
}

func TestCheckAttemptTransitionExhaustive(t *testing.T) {
	t.Parallel()
	for _, from := range AllAttemptStatuses {
		for _, to := range AllAttemptStatuses {
			key := fmt.Sprintf("%s>%s", from, to)
			noop, err := CheckAttemptTransition(from, to)
			switch {
			case from == to:
				if !noop || err != nil {
					t.Errorf("%s: want noop, got %v %v", key, noop, err)
				}
			case allowedAttemptTransitions[key]:
				if noop || err != nil {
					t.Errorf("%s: want allowed, got %v %v", key, noop, err)
				}
			default:
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s: want ErrInvalidTransition, got %v", key, err)
				}
			}
		}
	}
	if _, err := CheckAttemptTransition("BOGUS", AttemptSubmitted); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown attempt status: %v", err)
	}
}

func TestAttemptIsOpen(t *testing.T) {
	t.Parallel()
	open := map[AttemptStatus]bool{AttemptPendingSubmission: true, AttemptSubmitted: true, AttemptSubmissionUnknown: true}
	for _, s := range AllAttemptStatuses {
		if s.IsOpen() != open[s] {
			t.Errorf("%s IsOpen = %v", s, s.IsOpen())
		}
	}
}
