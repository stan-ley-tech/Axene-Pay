// Package httpx holds HTTP plumbing shared by every handler: the error
// envelope, request IDs, access logging, and panic recovery. It contains no
// domain logic.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorType is the broad category of an API error. Clients branch on Type
// and Code; Message is for humans and may change.
type ErrorType string

const (
	ErrTypeInvalidRequest ErrorType = "invalid_request_error"
	ErrTypeAuthentication ErrorType = "authentication_error"
	ErrTypePermission     ErrorType = "permission_error"
	ErrTypeNotFound       ErrorType = "not_found_error"
	ErrTypeConflict       ErrorType = "conflict_error"
	ErrTypeRateLimit      ErrorType = "rate_limit_error"
	ErrTypeAPI            ErrorType = "api_error"
)

// APIError is the body of every error response; see api-spec/openapi.yaml.
type APIError struct {
	Type      ErrorType `json:"type"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Param     string    `json:"param,omitempty"`
	RequestID string    `json:"request_id"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

// WriteError writes the standard error envelope. The request ID is taken
// from the request context so that every error is traceable.
func WriteError(w http.ResponseWriter, r *http.Request, status int, e APIError) {
	e.RequestID = RequestID(r.Context())
	WriteJSON(w, r, status, errorEnvelope{Error: e})
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already sent; all we can do is record it.
		slog.ErrorContext(r.Context(), "write json response", "err", err)
	}
}

// NotFound is the router's fallback for unknown routes.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, APIError{
		Type: ErrTypeNotFound, Code: "route_not_found", Message: "No such route.",
	})
}

// MethodNotAllowed is the router's fallback for a known path with the wrong method.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, APIError{
		Type: ErrTypeInvalidRequest, Code: "method_not_allowed", Message: "Method not allowed for this route.",
	})
}
