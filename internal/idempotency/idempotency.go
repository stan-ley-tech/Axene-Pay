// Package idempotency implements ADR-0007 for operations that can create
// financial effects.
//
// A key is claimed inside the same database transaction that creates the
// resource, so concurrent requests with one key produce exactly one
// resource: the losers wait on the key's row lock and then observe the
// winner's in-progress or completed record.
package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Key identifies an idempotent request.
type Key struct {
	MerchantID  pgtype.UUID
	Environment string
	Value       string // client-supplied Idempotency-Key header
	Method      string
	Path        string
	Fingerprint [32]byte
}

// Fingerprint hashes a canonical request representation.
func Fingerprint(canonical []byte) [32]byte { return sha256.Sum256(canonical) }

// ValidateValue checks the client-supplied key (1-255 printable ASCII).
func ValidateValue(v string) error {
	if len(v) == 0 || len(v) > 255 {
		return errors.New("idempotency key must be 1-255 characters")
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return errors.New("idempotency key must be printable ASCII without spaces")
		}
	}
	return nil
}

// State is what Lookup or Claim found.
type State int

const (
	// StateNew: the key was unused; with Claim, the caller now owns it.
	StateNew State = iota
	// StateCompleted: replay Record.ResponseStatus / ResponseBody.
	StateCompleted
	// StateInProgress: another request owns the key and has not finished.
	StateInProgress
	// StateMismatch: the key was used for a different request.
	StateMismatch
)

// Result describes an existing key.
type Result struct {
	State  State
	Record store.IdempotencyKey
}

// Lookup inspects a key without claiming it.
func Lookup(ctx context.Context, q *store.Queries, k Key, now time.Time) (Result, error) {
	rec, err := q.GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{MerchantID: k.MerchantID, Environment: k.Environment, Key: k.Value})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{State: StateNew}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("idempotency: lookup: %w", err)
	}
	if rec.ExpiresAt.Before(now) {
		return Result{State: StateNew}, nil
	}
	return classify(k, rec), nil
}

// Claim takes ownership of the key within q's transaction. It returns
// StateNew when the caller now owns the key; otherwise it describes the
// existing record, which the caller must not overwrite.
func Claim(ctx context.Context, q *store.Queries, k Key, now time.Time, ttl time.Duration) (Result, error) {
	if err := q.DeleteExpiredIdempotencyKey(ctx, store.DeleteExpiredIdempotencyKeyParams{
		MerchantID: k.MerchantID, Environment: k.Environment, Key: k.Value, Now: now,
	}); err != nil {
		return Result{}, fmt.Errorf("idempotency: expire: %w", err)
	}
	n, err := q.InsertIdempotencyKey(ctx, store.InsertIdempotencyKeyParams{
		MerchantID: k.MerchantID, Environment: k.Environment, Key: k.Value,
		RequestMethod: k.Method, RequestPath: k.Path, RequestFingerprint: k.Fingerprint[:],
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		return Result{}, fmt.Errorf("idempotency: claim: %w", err)
	}
	if n == 1 {
		return Result{State: StateNew}, nil
	}
	rec, err := q.GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{MerchantID: k.MerchantID, Environment: k.Environment, Key: k.Value})
	if err != nil {
		return Result{}, fmt.Errorf("idempotency: read existing: %w", err)
	}
	return classify(k, rec), nil
}

// SetResource records the public ID of the resource created under the key.
func SetResource(ctx context.Context, q *store.Queries, k Key, resourceID string) error {
	return q.SetIdempotencyResource(ctx, store.SetIdempotencyResourceParams{
		MerchantID: k.MerchantID, Environment: k.Environment, Key: k.Value, ResourceID: &resourceID,
	})
}

// Complete stores the response to replay for later requests with the key.
func Complete(ctx context.Context, q *store.Queries, k Key, status int, body []byte) error {
	st := int32(status)
	_, err := q.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
		MerchantID: k.MerchantID, Environment: k.Environment, Key: k.Value,
		ResponseStatus: &st, ResponseBody: body,
	})
	return err
}

func classify(k Key, rec store.IdempotencyKey) Result {
	if rec.RequestMethod != k.Method || rec.RequestPath != k.Path || string(rec.RequestFingerprint) != string(k.Fingerprint[:]) {
		return Result{State: StateMismatch, Record: rec}
	}
	if rec.Status == "completed" {
		return Result{State: StateCompleted, Record: rec}
	}
	return Result{State: StateInProgress, Record: rec}
}
