package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/stan-ley-tech/axene-pay/internal/outbox"
)

// Queue is the durable queue the fan-out consumer reads.
const Queue = "axene.webhooks.fanout"

// RunConsumer consumes payment events from RabbitMQ and fans each out until
// ctx is cancelled, reconnecting with backoff. A message is acknowledged
// only after its fan-out committed, so a crash causes redelivery, which
// FanOut tolerates. The database sweep covers anything the broker loses.
func RunConsumer(ctx context.Context, url string, svc *Service, log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := consume(ctx, url, svc, log)
		if ctx.Err() != nil {
			return
		}
		log.Warn("webhook consumer disconnected; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func consume(ctx context.Context, url string, svc *Service, log *slog.Logger) error {
	conn, err := amqp.Dial(url)
	if err != nil {
		return errors.New("cannot connect to RabbitMQ") // error text may contain credentials
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(outbox.Exchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(Queue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(Queue, "payment.#", outbox.Exchange, false, nil); err != nil {
		return err
	}
	if err := ch.Qos(20, 0, false); err != nil {
		return err
	}
	msgs, err := ch.Consume(Queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Info("webhook consumer connected", "queue", Queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-msgs:
			if !ok {
				return errors.New("delivery channel closed")
			}
			handle(ctx, svc, log, m)
		}
	}
}

func handle(ctx context.Context, svc *Service, log *slog.Logger, m amqp.Delivery) {
	_, err := svc.FanOut(ctx, m.MessageId)
	switch {
	case err == nil:
		_ = m.Ack(false)
	case errors.Is(err, ErrUnknownEvent):
		// Not ours or malformed: requeueing would loop forever.
		log.Error("discarding message for unknown event", "message_id", m.MessageId)
		_ = m.Ack(false)
	default:
		log.Error("webhook fan-out failed; requeueing", "event_id", m.MessageId, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
		_ = m.Nack(false, true)
	}
}
