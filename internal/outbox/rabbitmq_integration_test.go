//go:build rabbitmq

package outbox_test

import (
	"context"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/stan-ley-tech/axene-pay/internal/outbox"
)

// Runs with -tags=rabbitmq against RABBITMQ_TEST_URL (CI provides a broker).
func TestRabbitPublisherDeliversConfirmedMessages(t *testing.T) {
	url := os.Getenv("RABBITMQ_TEST_URL")
	if url == "" {
		t.Fatal("RABBITMQ_TEST_URL is not set")
	}
	pub, err := outbox.NewRabbitPublisher(url)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.QueueBind(q.Name, "payment.*", outbox.Exchange, false, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev := outbox.Event{ID: "evt_test", Type: "payment.succeeded", AggregateType: "payment", AggregateID: "pay_x",
		Payload: []byte(`{"payment_id":"pay_x"}`), RequestID: "req-1", CreatedAt: time.Now()}
	if err := pub.Publish(ctx, ev); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-msgs:
		if m.MessageId != "evt_test" || m.RoutingKey != "payment.succeeded" || string(m.Body) != `{"payment_id":"pay_x"}` ||
			m.DeliveryMode != amqp.Persistent || m.Headers["request_id"] != "req-1" {
			t.Fatalf("message %+v", m)
		}
	case <-ctx.Done():
		t.Fatal("message not delivered")
	}

	// After the connection drops, the publisher reconnects.
	pub.Close()
	if err := pub.Publish(ctx, ev); err != nil {
		t.Fatalf("publish after reconnect: %v", err)
	}
}
