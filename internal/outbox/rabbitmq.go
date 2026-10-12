package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Exchange is the durable topic exchange events are published to. The
// routing key is the event type (e.g. "payment.succeeded").
const Exchange = "axene.events"

// RabbitPublisher publishes events with publisher confirms. It reconnects
// lazily after a connection or channel failure.
type RabbitPublisher struct {
	url string

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

// NewRabbitPublisher connects to RabbitMQ and declares the exchange.
func NewRabbitPublisher(url string) (*RabbitPublisher, error) {
	p := &RabbitPublisher{url: url}
	if err := p.connect(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *RabbitPublisher) connect() error {
	conn, err := amqp.Dial(p.url)
	if err != nil {
		// amqp errors can contain the URL with credentials; keep them out.
		return errors.New("outbox: cannot connect to RabbitMQ")
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("outbox: open channel: %w", err)
	}
	if err := ch.ExchangeDeclare(Exchange, "topic", true, false, false, false, nil); err != nil {
		conn.Close()
		return fmt.Errorf("outbox: declare exchange: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		conn.Close()
		return fmt.Errorf("outbox: enable confirms: %w", err)
	}
	p.conn, p.ch = conn, ch
	return nil
}

// Publish sends e and waits for the broker's confirmation.
func (p *RabbitPublisher) Publish(ctx context.Context, e Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == nil || p.ch.IsClosed() || p.conn.IsClosed() {
		p.closeLocked()
		if err := p.connect(); err != nil {
			return err
		}
	}
	dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, Exchange, e.Type, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    e.ID,
		Type:         e.Type,
		Timestamp:    e.CreatedAt,
		Headers: amqp.Table{
			"aggregate_type": e.AggregateType,
			"aggregate_id":   e.AggregateID,
			"request_id":     e.RequestID,
		},
		Body: e.Payload,
	})
	if err != nil {
		p.closeLocked()
		return fmt.Errorf("outbox: publish: %w", err)
	}
	ok, err := dc.WaitContext(ctx)
	if err != nil {
		p.closeLocked()
		return fmt.Errorf("outbox: await confirm: %w", err)
	}
	if !ok {
		return ErrNotConfirmed
	}
	return nil
}

// Close closes the connection.
func (p *RabbitPublisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
}

func (p *RabbitPublisher) closeLocked() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}
