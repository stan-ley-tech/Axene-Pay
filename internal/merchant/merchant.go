// Package merchant owns merchants, their settlement destinations
// (ADR-0008), provider configuration, and API credentials.
package merchant

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Environment separates sandbox and live data for a merchant.
type Environment string

const (
	Sandbox Environment = "sandbox"
	Live    Environment = "live"
)

// ParseEnvironment validates an environment name.
func ParseEnvironment(s string) (Environment, error) {
	switch e := Environment(s); e {
	case Sandbox, Live:
		return e, nil
	}
	return "", fmt.Errorf("%w: environment must be sandbox or live", ErrInvalid)
}

// Status is a merchant's onboarding state (decision D-03).
type Status string

const (
	StatusPendingReview Status = "pending_review"
	StatusApproved      Status = "approved"
	StatusSuspended     Status = "suspended"
)

var (
	// ErrInvalid wraps input validation failures.
	ErrInvalid = errors.New("merchant: invalid input")
	// ErrNotFound means the referenced merchant-owned record does not exist.
	ErrNotFound = errors.New("merchant: not found")
	// ErrConflict means a uniqueness rule was violated.
	ErrConflict = errors.New("merchant: already exists")
	// ErrNotApproved means live access was requested for an unapproved merchant.
	ErrNotApproved = errors.New("merchant: merchant is not approved for live processing")
	// ErrUnauthenticated means the API key is missing, malformed, unknown, or revoked.
	ErrUnauthenticated = errors.New("merchant: invalid API key")
	// ErrSuspended means the merchant may not use the API.
	ErrSuspended = errors.New("merchant: merchant suspended")
)

// Service manages merchants and credentials.
type Service struct {
	q      *store.Queries
	pepper []byte
}

// MinPepperBytes is the minimum length of the API key hashing pepper.
const MinPepperBytes = 32

// NewService returns a Service. pepper keys the HMAC used to store API keys
// and must come from the secret store.
func NewService(pool *pgxpool.Pool, pepper []byte) (*Service, error) {
	if len(pepper) < MinPepperBytes {
		return nil, fmt.Errorf("merchant: API key pepper must be at least %d bytes", MinPepperBytes)
	}
	return &Service{q: store.New(pool), pepper: pepper}, nil
}

// CreateMerchant registers a merchant in pending_review.
func (s *Service) CreateMerchant(ctx context.Context, name string) (store.Merchant, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return store.Merchant{}, fmt.Errorf("%w: name must be 1-200 characters", ErrInvalid)
	}
	return s.q.CreateMerchant(ctx, store.CreateMerchantParams{
		PublicID: ids.New(ids.Merchant), Name: name, Status: string(StatusPendingReview),
	})
}

// SetStatus changes a merchant's onboarding status.
func (s *Service) SetStatus(ctx context.Context, merchantID string, status Status) (store.Merchant, error) {
	switch status {
	case StatusPendingReview, StatusApproved, StatusSuspended:
	default:
		return store.Merchant{}, fmt.Errorf("%w: unknown status %q", ErrInvalid, status)
	}
	m, err := s.q.SetMerchantStatus(ctx, store.SetMerchantStatusParams{PublicID: merchantID, Status: string(status)})
	return m, notFound(err)
}

// DestinationTypes are the settlement destination kinds supported today.
var DestinationTypes = []string{"mpesa_paybill", "mpesa_till"}

var digits = regexp.MustCompile(`^[0-9]{1,20}$`)

// CreateDestination registers a merchant-owned settlement destination.
func (s *Service) CreateDestination(ctx context.Context, merchantID string, env Environment, typ, identifier, label string) (store.SettlementDestination, error) {
	if !oneOf(typ, DestinationTypes) {
		return store.SettlementDestination{}, fmt.Errorf("%w: destination type must be one of %v", ErrInvalid, DestinationTypes)
	}
	if !digits.MatchString(identifier) {
		return store.SettlementDestination{}, fmt.Errorf("%w: identifier must be 1-20 digits", ErrInvalid)
	}
	m, err := s.merchant(ctx, merchantID)
	if err != nil {
		return store.SettlementDestination{}, err
	}
	d, err := s.q.CreateSettlementDestination(ctx, store.CreateSettlementDestinationParams{
		PublicID: ids.New(ids.SettlementDestination), MerchantID: m.ID, Environment: string(env),
		Type: typ, Identifier: identifier, Label: label,
	})
	return d, conflict(err)
}

// CreateProviderConfig allows provider to collect method for the merchant
// into destinationID. Lower priority values are tried first.
func (s *Service) CreateProviderConfig(ctx context.Context, merchantID string, env Environment, method, provider, destinationID string, priority int32) (store.MerchantProviderConfig, error) {
	if method != "mpesa" {
		return store.MerchantProviderConfig{}, fmt.Errorf("%w: method must be mpesa", ErrInvalid)
	}
	if priority < 0 || priority > 10000 {
		return store.MerchantProviderConfig{}, fmt.Errorf("%w: priority must be 0-10000", ErrInvalid)
	}
	m, err := s.merchant(ctx, merchantID)
	if err != nil {
		return store.MerchantProviderConfig{}, err
	}
	d, err := s.q.GetSettlementDestination(ctx, store.GetSettlementDestinationParams{PublicID: destinationID, MerchantID: m.ID})
	if err != nil {
		return store.MerchantProviderConfig{}, notFound(err)
	}
	if d.Environment != string(env) {
		return store.MerchantProviderConfig{}, fmt.Errorf("%w: destination is for %s, not %s", ErrInvalid, d.Environment, env)
	}
	c, err := s.q.CreateProviderConfig(ctx, store.CreateProviderConfigParams{
		PublicID: ids.New(ids.ProviderConfig), MerchantID: m.ID, Environment: string(env),
		Method: method, Provider: provider, SettlementDestinationID: d.ID, Priority: priority,
	})
	if pgCode(err) == "23514" { // provider name format
		return store.MerchantProviderConfig{}, fmt.Errorf("%w: invalid provider name", ErrInvalid)
	}
	return c, conflict(err)
}

// IssuedKey is a newly created API key. Secret is shown exactly once.
type IssuedKey struct {
	ID          string
	Secret      string
	DisplayHint string
	Environment Environment
}

// IssueAPIKey creates an API key. Live keys require an approved merchant.
func (s *Service) IssueAPIKey(ctx context.Context, merchantID string, env Environment) (IssuedKey, error) {
	m, err := s.merchant(ctx, merchantID)
	if err != nil {
		return IssuedKey{}, err
	}
	if Status(m.Status) == StatusSuspended {
		return IssuedKey{}, ErrSuspended
	}
	if env == Live && Status(m.Status) != StatusApproved {
		return IssuedKey{}, ErrNotApproved
	}
	secret := newSecret(env)
	row, err := s.q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		PublicID: ids.New(ids.APIKey), MerchantID: m.ID, Environment: string(env),
		DisplayHint: secret[:12], SecretHash: s.hash(secret),
	})
	if err != nil {
		return IssuedKey{}, err
	}
	return IssuedKey{ID: row.PublicID, Secret: secret, DisplayHint: row.DisplayHint, Environment: env}, nil
}

// RevokeAPIKey revokes a key immediately.
func (s *Service) RevokeAPIKey(ctx context.Context, keyID string) error {
	n, err := s.q.RevokeAPIKey(ctx, keyID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Principal is the authenticated caller of the merchant API.
type Principal struct {
	MerchantID       pgtype.UUID
	MerchantPublicID string
	Environment      Environment
	KeyID            string
}

// Authenticate resolves an API key secret to its merchant and environment.
func (s *Service) Authenticate(ctx context.Context, secret string) (Principal, error) {
	env, ok := keyEnvironment(secret)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	row, err := s.q.GetAPIKeyForAuth(ctx, s.hash(secret))
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("merchant: look up API key: %w", err)
	}
	if row.RevokedAt != nil || Environment(row.Environment) != env {
		return Principal{}, ErrUnauthenticated
	}
	switch Status(row.MerchantStatus) {
	case StatusSuspended:
		return Principal{}, ErrSuspended
	case StatusPendingReview:
		if env == Live {
			return Principal{}, ErrNotApproved
		}
	}
	if err := s.q.TouchAPIKey(ctx, row.ID); err != nil {
		return Principal{}, fmt.Errorf("merchant: record API key use: %w", err)
	}
	return Principal{
		MerchantID: row.MerchantID, MerchantPublicID: row.MerchantPublicID,
		Environment: env, KeyID: row.PublicID,
	}, nil
}

// Key format: sk_test_ or sk_live_ followed by 43 base64url characters
// (32 random bytes).
const secretBodyLen = 43

func newSecret(env Environment) string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	prefix := "sk_test_"
	if env == Live {
		prefix = "sk_live_"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}

func keyEnvironment(secret string) (Environment, bool) {
	var env Environment
	var body string
	switch {
	case strings.HasPrefix(secret, "sk_test_"):
		env, body = Sandbox, secret[len("sk_test_"):]
	case strings.HasPrefix(secret, "sk_live_"):
		env, body = Live, secret[len("sk_live_"):]
	default:
		return "", false
	}
	if len(body) != secretBodyLen {
		return "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(body); err != nil {
		return "", false
	}
	return env, true
}

func (s *Service) hash(secret string) []byte {
	m := hmac.New(sha256.New, s.pepper)
	m.Write([]byte(secret))
	return m.Sum(nil)
}

func (s *Service) merchant(ctx context.Context, merchantID string) (store.Merchant, error) {
	if _, err := ids.Parse(ids.Merchant, merchantID); err != nil {
		return store.Merchant{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	m, err := s.q.GetMerchantByPublicID(ctx, merchantID)
	return m, notFound(err)
}

func oneOf(s string, set []string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func conflict(err error) error {
	if pgCode(err) == "23505" {
		return ErrConflict
	}
	return err
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
