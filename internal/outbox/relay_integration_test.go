//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/outbox"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var ctx = context.Background()

type recorder struct {
	mu     sync.Mutex
	got    []outbox.Event
	failOn string
}

func (r *recorder) Publish(_ context.Context, e outbox.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.ID == r.failOn {
		return errors.New("broker down")
	}
	r.got = append(r.got, e)
	return nil
}

func insertEvents(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	q := store.New(pool)
	var out []string
	for i := 0; i < n; i++ {
		id := ids.New(ids.Event)
		if _, err := q.InsertOutboxEvent(ctx, store.InsertOutboxEventParams{
			PublicID: id, EventType: "payment.succeeded", AggregateType: "payment", AggregateID: "pay_x",
			Payload: []byte(`{"n":1}`), RequestID: "req",
		}); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRelayPublishesInOrderAndMarksPublished(t *testing.T) {
	pool := testdb.New(t)
	want := insertEvents(t, pool, 5)
	rec := &recorder{}
	relay := outbox.NewRelay(pool, rec, logger(), 3)

	n, err := relay.RunOnce(ctx)
	if err != nil || n != 3 {
		t.Fatalf("first batch = %d, %v", n, err)
	}
	n, err = relay.RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("second batch = %d, %v", n, err)
	}
	if n, _ := relay.RunOnce(ctx); n != 0 {
		t.Fatalf("republished %d", n)
	}
	for i, e := range rec.got {
		if e.ID != want[i] {
			t.Fatalf("order: got %s at %d, want %s", e.ID, i, want[i])
		}
	}
	if count, age, err := relay.Backlog(ctx); err != nil || count != 0 || age != 0 {
		t.Fatalf("backlog = %d %v %v", count, age, err)
	}
}

func TestRelayStopsAtFailureAndRetries(t *testing.T) {
	pool := testdb.New(t)
	ids_ := insertEvents(t, pool, 3)
	rec := &recorder{failOn: ids_[1]}
	relay := outbox.NewRelay(pool, rec, logger(), 10)

	n, err := relay.RunOnce(ctx)
	if err == nil || n != 1 {
		t.Fatalf("run with failure = %d, %v", n, err)
	}
	if count, _, _ := relay.Backlog(ctx); count != 2 {
		t.Fatalf("backlog after failure = %d, want 2", count)
	}
	var attempts int32
	var lastErr *string
	if err := pool.QueryRow(ctx, `SELECT publish_attempts, last_error FROM outbox_events WHERE public_id = $1`, ids_[1]).Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr == nil || *lastErr != "broker down" {
		t.Fatalf("failure not recorded: %d %v", attempts, lastErr)
	}
	// Broker recovers: the rest drains in order.
	rec.failOn = ""
	if n, err := relay.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("recovery = %d, %v", n, err)
	}
	if len(rec.got) != 3 || rec.got[1].ID != ids_[1] || rec.got[2].ID != ids_[2] {
		t.Fatalf("published %+v", rec.got)
	}
}

func TestConcurrentRelaysDoNotDoublePublish(t *testing.T) {
	pool := testdb.New(t)
	insertEvents(t, pool, 40)
	rec := &recorder{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relay := outbox.NewRelay(pool, rec, logger(), 5)
			for {
				n, err := relay.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, e := range rec.got {
		if seen[e.ID] {
			t.Fatalf("%s published twice", e.ID)
		}
		seen[e.ID] = true
	}
	if len(seen) != 40 {
		t.Fatalf("published %d of 40", len(seen))
	}
}
