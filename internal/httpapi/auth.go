package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/platform/httpx"
)

// Authenticator resolves API key secrets; *merchant.Service satisfies it.
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) (merchant.Principal, error)
}

type principalKey struct{}

func principal(ctx context.Context) (merchant.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(merchant.Principal)
	return p, ok
}

// requireAPIKey authenticates "Authorization: Bearer sk_..." and stores the
// principal in the request context. Authorization of individual resources
// happens in the services, which scope every query by merchant.
func requireAPIKey(auth Authenticator, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			secret, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || secret == "" {
				httpx.WriteError(w, r, http.StatusUnauthorized, httpx.APIError{
					Type: httpx.ErrTypeAuthentication, Code: "missing_api_key",
					Message: "Provide your API key as 'Authorization: Bearer <key>'.",
				})
				return
			}
			p, err := auth.Authenticate(r.Context(), secret)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
			case errors.Is(err, merchant.ErrUnauthenticated):
				httpx.WriteError(w, r, http.StatusUnauthorized, httpx.APIError{
					Type: httpx.ErrTypeAuthentication, Code: "invalid_api_key", Message: "The API key is invalid or revoked.",
				})
			case errors.Is(err, merchant.ErrSuspended):
				httpx.WriteError(w, r, http.StatusForbidden, httpx.APIError{
					Type: httpx.ErrTypePermission, Code: "merchant_suspended", Message: "This merchant account is suspended.",
				})
			case errors.Is(err, merchant.ErrNotApproved):
				httpx.WriteError(w, r, http.StatusForbidden, httpx.APIError{
					Type: httpx.ErrTypePermission, Code: "merchant_not_approved", Message: "This merchant is not approved for live processing.",
				})
			default:
				logger.ErrorContext(r.Context(), "authenticate", "request_id", httpx.RequestID(r.Context()), "err", err)
				internalError(w, r)
			}
		})
	}
}

func internalError(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusInternalServerError, httpx.APIError{
		Type: httpx.ErrTypeAPI, Code: "internal_error", Message: "An internal error occurred.",
	})
}
