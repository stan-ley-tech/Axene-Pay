//go:build integration && rabbitmq

package webhooks_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stan-ley-tech/axene-pay/internal/outbox"
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

// End to end through a real broker: payment event -> outbox relay ->
// RabbitMQ -> consumer -> fan-out -> delivery to the merchant endpoint.
// Runs with -tags=integration,rabbitmq (CI provides PostgreSQL and RabbitMQ).
func TestConsumerFansOutEventsFromTheBroker(t *testing.T) {
	url := os.Getenv("RABBITMQ_TEST_URL")
	if url == "" {
		t.Fatal("RABBITMQ_TEST_URL is not set")
	}
	e := setup(t)
	r := newReceiver(t)
	e.endpoint(r, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go webhooks.RunConsumer(ctx, url, e.wh, logger)
	time.Sleep(2 * time.Second) // let the consumer declare and bind its queue

	_, events := e.payment(e.pr)
	pub, err := outbox.NewRabbitPublisher(url)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if n, err := outbox.NewRelay(e.pool, pub, logger, 10).RunOnce(ctx); err != nil || n != len(events) {
		t.Fatalf("relay published %d, %v", n, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var fanned int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_event_fanouts`).Scan(&fanned); err != nil {
			t.Fatal(err)
		}
		if fanned == len(events) {
			if n, err := e.wh.DeliverDue(ctx, 10); err != nil || n != 1 {
				t.Fatalf("delivered %d, %v", n, err)
			}
			if r.count() != 1 || r.badSig != 0 {
				t.Fatalf("receiver got %d (bad signatures %d)", r.count(), r.badSig)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("consumer did not fan out the event")
}
