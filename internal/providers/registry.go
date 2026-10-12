package providers

import (
	"errors"
	"fmt"
	"sort"
)

var (
	// ErrUnknownProvider is returned when no provider is registered under a name.
	ErrUnknownProvider = errors.New("providers: unknown provider")
	// ErrSimulatedInProduction is returned when a test double is registered
	// in production.
	ErrSimulatedInProduction = errors.New("providers: simulated provider refused in production")
)

// Registry holds the provider adapters available to a process.
type Registry struct {
	production bool
	byName     map[string]Provider
}

// NewRegistry returns an empty registry. In production, simulated providers
// cannot be registered.
func NewRegistry(production bool) *Registry {
	return &Registry{production: production, byName: map[string]Provider{}}
}

// Register adds p. Names must be unique.
func (r *Registry) Register(p Provider) error {
	if r.production && p.Simulated() {
		return fmt.Errorf("%w: %s", ErrSimulatedInProduction, p.Name())
	}
	if _, dup := r.byName[p.Name()]; dup {
		return fmt.Errorf("providers: %s registered twice", p.Name())
	}
	r.byName[p.Name()] = p
	return nil
}

// Get returns the provider registered under name.
func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	return p, nil
}

// Names returns registered provider names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
