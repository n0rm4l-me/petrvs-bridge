package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Published is a record of a message that went through Mem.Publish.
type Published struct {
	Topic string
	Data  []byte
	Attrs map[string]string
}

type memQueue struct {
	ch   chan *Message
	held []*Message
	hold bool
}

// Mem is an in-process bus for tests. Besides normal delivery it offers the
// things an attacker or a bad network can do: inject messages, drop them,
// hold and reorder them, and record everything for replay.
type Mem struct {
	mu        sync.Mutex
	bindings  map[string][]string
	queues    map[string]*memQueue
	published []Published
	drop      func(Published) bool
	seq       int
	closed    bool
}

// ErrNoTopic is returned when publishing to a topic nobody bound.
var ErrNoTopic = errors.New("transport: unknown topic")

// NewMem returns an empty bus.
func NewMem() *Mem {
	return &Mem{bindings: map[string][]string{}, queues: map[string]*memQueue{}}
}

// Bind creates the subscription and attaches it to the topic.
func (m *Mem) Bind(topic, sub string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.queues[sub]; !ok {
		m.queues[sub] = &memQueue{ch: make(chan *Message, 1024)}
	}
	m.bindings[topic] = append(m.bindings[topic], sub)
}

// SetDrop installs a filter: messages for which it returns true are silently lost.
func (m *Mem) SetDrop(f func(Published) bool) {
	m.mu.Lock()
	m.drop = f
	m.mu.Unlock()
}

// Published returns a copy of everything published to the topic, for replay tests.
func (m *Mem) Published(topic string) []Published {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Published
	for _, p := range m.published {
		if p.Topic == topic {
			out = append(out, p)
		}
	}
	return out
}

func (m *Mem) deliver(sub string, data []byte, attrs map[string]string, attempt int) {
	q := m.queues[sub]
	m.seq++
	id := fmt.Sprintf("mem-%d", m.seq)
	msg := &Message{ID: id, Data: append([]byte(nil), data...), Attrs: copyAttrs(attrs), PublishedAt: time.Now(), Attempt: attempt}
	msg.ack = func() {}
	msg.nack = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if attempt < 5 && !m.closed {
			m.deliver(sub, data, attrs, attempt+1)
		}
	}
	if q.hold {
		q.held = append(q.held, msg)
		return
	}
	select {
	case q.ch <- msg:
	default: // queue full: behave like a bus that sheds load
	}
}

func copyAttrs(a map[string]string) map[string]string {
	out := make(map[string]string, len(a))
	for k, v := range a {
		out[k] = v
	}
	return out
}

// Publish fans the message out to every subscription bound to the topic.
func (m *Mem) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	subs, ok := m.bindings[topic]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTopic, topic)
	}
	rec := Published{Topic: topic, Data: append([]byte(nil), data...), Attrs: copyAttrs(attrs)}
	m.published = append(m.published, rec)
	if m.drop != nil && m.drop(rec) {
		return nil
	}
	for _, s := range subs {
		m.deliver(s, data, attrs, 1)
	}
	return nil
}

// Inject puts a message straight into a subscription, as an attacker with publish rights would.
func (m *Mem) Inject(sub string, data []byte, attrs map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliver(sub, data, attrs, 1)
}

// Hold keeps messages for the subscription back until Release.
func (m *Mem) Hold(sub string) {
	m.mu.Lock()
	m.queues[sub].hold = true
	m.mu.Unlock()
}

// Release delivers the held messages in the order order() returns (indexes into the held list).
func (m *Mem) Release(sub string, order func(n int) []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[sub]
	q.hold = false
	held := q.held
	q.held = nil
	idx := make([]int, len(held))
	for i := range idx {
		idx[i] = i
	}
	if order != nil {
		idx = order(len(held))
	}
	for _, i := range idx {
		q.ch <- held[i]
	}
}

// Receive calls the handler sequentially for each message.
func (m *Mem) Receive(ctx context.Context, sub string, handler func(context.Context, *Message)) error {
	m.mu.Lock()
	q, ok := m.queues[sub]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("transport: unknown subscription %s", sub)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-q.ch:
			handler(ctx, msg)
		}
	}
}

// Close stops redelivery.
func (m *Mem) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}
