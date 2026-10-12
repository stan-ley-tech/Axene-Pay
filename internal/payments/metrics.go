package payments

import "time"

// Metrics receives operational measurements. Implementations must not use
// personal or payment-identifying data as labels.
type Metrics interface {
	Transition(from, to Status, evidence Evidence)
	ProviderCall(provider, operation, class string, d time.Duration)
	Verification(provider, result string)
	Callback(provider, result string)
	ReconException(kind string)
}

type noMetrics struct{}

func (noMetrics) Transition(Status, Status, Evidence)                {}
func (noMetrics) ProviderCall(string, string, string, time.Duration) {}
func (noMetrics) Verification(string, string)                        {}
func (noMetrics) Callback(string, string)                            {}
func (noMetrics) ReconException(string)                              {}
