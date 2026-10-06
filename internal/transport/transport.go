// Package transport moves opaque messages between the hub and the agents.
// It knows nothing about keys or payloads.
package transport

import (
	"context"
	"time"
)

// Message is one delivered bus message. The handler must call Ack or Nack.
type Message struct {
	ID          string
	Data        []byte
	Attrs       map[string]string
	PublishedAt time.Time
	// Attempt is the delivery attempt, starting at 1, when the bus reports it.
	Attempt int

	ack, nack func()
}

// NewMessage builds a message with the given acknowledgement callbacks (for tests and adapters).
func NewMessage(id string, data []byte, attrs map[string]string, ack, nack func()) *Message {
	return &Message{ID: id, Data: data, Attrs: attrs, Attempt: 1, ack: ack, nack: nack}
}

// Ack tells the bus the message is done and must not be delivered again.
func (m *Message) Ack() {
	if m.ack != nil {
		m.ack()
	}
}

// Nack asks the bus to deliver the message again.
func (m *Message) Nack() {
	if m.nack != nil {
		m.nack()
	}
}

// Transport publishes to topics and receives from subscriptions.
type Transport interface {
	// Publish returns once the bus has accepted the message.
	Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error
	// Receive calls handler for each message until ctx is cancelled or a fatal error occurs.
	Receive(ctx context.Context, subscription string, handler func(context.Context, *Message)) error
	Close() error
}
