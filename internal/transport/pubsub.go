package transport

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
)

// PubSub is a Transport on Google Cloud Pub/Sub (or its emulator, selected by
// PUBSUB_EMULATOR_HOST). It talks gRPC to the service.
type PubSub struct {
	client  *pubsub.Client
	project string

	mu   sync.Mutex
	pubs map[string]*pubsub.Publisher
}

// NewPubSub connects to the project. Credentials come from the environment (application default credentials).
func NewPubSub(ctx context.Context, project string, opts ...option.ClientOption) (*PubSub, error) {
	c, err := pubsub.NewClient(ctx, project, opts...)
	if err != nil {
		return nil, err
	}
	return &PubSub{client: c, project: project, pubs: map[string]*pubsub.Publisher{}}, nil
}

func (p *PubSub) publisher(topic string) *pubsub.Publisher {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pub, ok := p.pubs[topic]; ok {
		return pub
	}
	pub := p.client.Publisher(topic)
	p.pubs[topic] = pub
	return pub
}

// Publish sends one message and waits for the service to accept it.
func (p *PubSub) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	res := p.publisher(topic).Publish(ctx, &pubsub.Message{Data: data, Attributes: attrs})
	_, err := res.Get(ctx)
	return err
}

// Receive delivers messages from the subscription to handler until ctx ends.
func (p *PubSub) Receive(ctx context.Context, subscription string, handler func(context.Context, *Message)) error {
	s := p.client.Subscriber(subscription)
	// Bound what a flood on the topic can make this process hold and work on at once.
	s.ReceiveSettings.MaxOutstandingMessages = 32
	s.ReceiveSettings.MaxOutstandingBytes = 16 << 20
	s.ReceiveSettings.NumGoroutines = 1
	return s.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		attempt := 1
		if m.DeliveryAttempt != nil {
			attempt = *m.DeliveryAttempt
		}
		handler(ctx, &Message{ID: m.ID, Data: m.Data, Attrs: m.Attributes, PublishedAt: m.PublishTime, Attempt: attempt,
			ack: m.Ack, nack: m.Nack})
	})
}

// Close stops publishers and closes the connection.
func (p *PubSub) Close() error {
	p.mu.Lock()
	for _, pub := range p.pubs {
		pub.Stop()
	}
	p.mu.Unlock()
	return p.client.Close()
}

// EnsureTopic creates the topic if it does not exist. Production topics are
// created by infrastructure code; this is for development and tests.
func (p *PubSub) EnsureTopic(ctx context.Context, topic string) error {
	_, err := p.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: fmt.Sprintf("projects/%s/topics/%s", p.project, topic)})
	if err != nil && !isAlreadyExists(err) {
		return err
	}
	return nil
}

// EnsureSubscription creates a pull subscription on the topic if it does not exist.
func (p *PubSub) EnsureSubscription(ctx context.Context, topic, sub string, ackDeadline time.Duration) error {
	_, err := p.client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:               fmt.Sprintf("projects/%s/subscriptions/%s", p.project, sub),
		Topic:              fmt.Sprintf("projects/%s/topics/%s", p.project, topic),
		AckDeadlineSeconds: int32(ackDeadline / time.Second),
	})
	if err != nil && !isAlreadyExists(err) {
		return err
	}
	return nil
}

// Delete removes a topic and subscriptions, ignoring ones that are already gone.
func (p *PubSub) Delete(ctx context.Context, topic string, subs ...string) {
	for _, s := range subs {
		_ = p.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: fmt.Sprintf("projects/%s/subscriptions/%s", p.project, s)})
	}
	if topic != "" {
		_ = p.client.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: fmt.Sprintf("projects/%s/topics/%s", p.project, topic)})
	}
}
