// Package orchestration decides which provider processes a payment attempt.
//
// Routing is deterministic: candidates are evaluated in the order the
// merchant configured (priority, then provider name) and the first eligible
// one wins. The decision always carries a human-readable reason listing why
// earlier candidates were skipped, which is persisted on the attempt for
// audit and investigation.
package orchestration

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
)

// Candidate is one merchant provider configuration with its destination.
type Candidate struct {
	ConfigID          string
	Provider          string
	Priority          int32
	Enabled           bool
	DestinationID     string // public ID
	Destination       providers.Destination
	DestinationActive bool
}

// Request describes what must be collected.
type Request struct {
	Method providers.Method
	Amount money.Amount
}

// Decision is the routing outcome.
type Decision struct {
	Provider  providers.Provider
	Candidate Candidate
	Reason    string
}

// ErrNoEligibleProvider means no configured provider can take the payment.
var ErrNoEligibleProvider = errors.New("orchestration: no eligible provider")

// Lookup resolves provider names; *providers.Registry satisfies it.
type Lookup interface {
	Get(name string) (providers.Provider, error)
}

// Route returns the first eligible candidate. candidates must already be in
// routing order.
func Route(req Request, candidates []Candidate, reg Lookup) (Decision, error) {
	var skipped []string
	for _, c := range candidates {
		p, why := eligible(req, c, reg)
		if why != "" {
			skipped = append(skipped, fmt.Sprintf("%s (%s): %s", c.ConfigID, c.Provider, why))
			continue
		}
		reason := fmt.Sprintf("selected %s via %s (priority %d) for %s %s",
			c.Provider, c.ConfigID, c.Priority, req.Method, req.Amount.Currency())
		if len(skipped) > 0 {
			reason += "; skipped " + strings.Join(skipped, "; ")
		}
		return Decision{Provider: p, Candidate: c, Reason: reason}, nil
	}
	if len(candidates) == 0 {
		return Decision{}, fmt.Errorf("%w: no provider configured for %s", ErrNoEligibleProvider, req.Method)
	}
	return Decision{}, fmt.Errorf("%w: %s", ErrNoEligibleProvider, strings.Join(skipped, "; "))
}

func eligible(req Request, c Candidate, reg Lookup) (providers.Provider, string) {
	if !c.Enabled {
		return nil, "configuration disabled"
	}
	if !c.DestinationActive {
		return nil, "settlement destination disabled"
	}
	p, err := reg.Get(c.Provider)
	if err != nil {
		return nil, "provider not available in this deployment"
	}
	if !p.Capabilities().Has(providers.CapCollect) {
		return nil, "provider cannot collect payments"
	}
	limits := p.Limits()
	if !limits.Supports(req.Method, req.Amount.Currency()) {
		return nil, fmt.Sprintf("provider does not support %s in %s", req.Method, req.Amount.Currency())
	}
	if limits.WholeUnitsOnly {
		if _, err := req.Amount.WholeUnits(); err != nil {
			return nil, "provider only accepts whole currency units"
		}
	}
	return p, ""
}
