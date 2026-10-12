// Package outbox relays committed outbox events to the message broker
// (ADR-0004). Delivery is at least once: an event is marked published only
// after the broker confirms it, so a crash between publish and mark causes a
// re-publish, never a loss. Consumers deduplicate by event ID.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Event is an outbox event as handed to a Publisher.
type Event struct {
	ID            string
	Type          string
	AggregateType string
	AggregateID   string
	Payload       []byte
	RequestID     string
	CreatedAt     time.Time
}

// Publisher delivers events to the broker. Publish must return only after
// the broker has durably accepted the event.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// Relay moves events from the outbox table to a Publisher.
type Relay struct {
	pool  *pgxpool.Pool
	q     *store.Queries
	pub   Publisher
	log   *slog.Logger
	batch int32
}

// NewRelay returns a Relay that publishes up to batch events per run.
func NewRelay(pool *pgxpool.Pool, pub Publisher, log *slog.Logger, batch int32) *Relay {
	return &Relay{pool: pool, q: store.New(pool), pub: pub, log: log, batch: batch}
}

// RunOnce publishes one batch in creation order and returns how many events
// were published. It stops at the first failure so later events are not
// published ahead of an earlier one; the failure is recorded on the event.
// Concurrent relays skip each other's locked rows.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	published := 0
	var publishErr error
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		events, err := q.ClaimUnpublishedEvents(ctx, r.batch)
		if err != nil {
			return err
		}
		for _, e := range events {
			if err := r.pub.Publish(ctx, Event{
				ID: e.PublicID, Type: e.EventType, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
				Payload: e.Payload, RequestID: e.RequestID, CreatedAt: e.CreatedAt,
			}); err != nil {
				publishErr = fmt.Errorf("publish %s: %w", e.PublicID, err)
				return q.MarkEventPublishFailed(ctx, store.MarkEventPublishFailedParams{ID: e.ID, LastError: ptr(truncate(err.Error(), 500))})
			}
			if err := q.MarkEventPublished(ctx, e.ID); err != nil {
				return err
			}
			published++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("outbox: relay: %w", err)
	}
	return published, publishErr
}

// Backlog reports unpublished events and the age of the oldest one.
func (r *Relay) Backlog(ctx context.Context) (int64, time.Duration, error) {
	b, err := r.q.OutboxBacklog(ctx)
	if err != nil {
		return 0, 0, err
	}
	return b.Unpublished, time.Duration(b.OldestAgeSeconds * float64(time.Second)), nil
}

// ErrNotConfirmed means the broker refused (nacked) a message.
var ErrNotConfirmed = errors.New("outbox: broker did not confirm message")

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ptr(s string) *string { return &s }
