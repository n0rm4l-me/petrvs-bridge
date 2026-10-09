package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PullMode is how Receive reads a subscription.
type PullMode string

const (
	// PullStream keeps one streaming call open and lets the library do the flow control and the leases. It is what the hub
	// uses in the cluster.
	PullStream PullMode = "stream"
	// PullUnary asks for messages with short calls, each with a deadline. A tunnel or a TLS-inspecting proxy between the
	// laptop and Google can hold a long stream back and let it out in lumps (heartbeats arrive in groups, and the server
	// sends each message more than once); a short call has an end that the network cannot hold back, and a call that hangs
	// is given up after a deadline.
	PullUnary PullMode = "unary"
)

// SetPullMode chooses how Receive reads. Call it before Receive.
func (p *PubSub) SetPullMode(m PullMode) { p.pull = m }

const (
	unaryPullDeadline = 25 * time.Second // one Pull call; a call that hangs longer is given up and made again
	unaryEmptyPause   = 250 * time.Millisecond
	unaryMaxInFlight  = 32 // what a flood on the topic can make this process hold at once, as MaxOutstandingMessages does
	unaryLease        = 30 * time.Second
	unaryLeaseEvery   = 5 * time.Second
	unaryMaxLease     = 10 * time.Minute // a handler that has not answered by then loses its lease; the message comes again
	unaryAckEvery     = 200 * time.Millisecond
	unaryAckTimeout   = 15 * time.Second
	unaryAckGiveUp    = time.Minute // an acknowledgement that still fails after this long is dropped: its lease is gone
	unaryAckBatch     = 500
)

// receiveUnary delivers messages with Pull calls until ctx ends. Like the streaming Receive it returns nil when ctx ends
// and waits for the handlers that are running; any other error is returned for the caller to wait and call again.
func (p *PubSub) receiveUnary(ctx context.Context, subscription string, handler func(context.Context, *Message)) error {
	name := subscription
	if !strings.HasPrefix(name, "projects/") {
		name = fmt.Sprintf("projects/%s/subscriptions/%s", p.project, subscription)
	}
	admin := p.client.SubscriptionAdminClient
	l := &leases{inflight: map[string]time.Time{}}

	// The leases and the acknowledgements have their own loop, so that a handler that waits does not hold them back.
	bgCtx, stopBg := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		l.run(bgCtx, func(ctx context.Context, ids []string) error {
			return admin.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: name, AckIds: ids})
		}, func(ctx context.Context, ids []string, deadline time.Duration) error {
			return admin.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{Subscription: name, AckIds: ids, AckDeadlineSeconds: int32(deadline / time.Second)})
		})
	}()

	var handlers sync.WaitGroup
	finish := func() {
		handlers.Wait()
		stopBg() // the loop sends what is left once more before it ends
		bg.Wait()
	}

	for {
		if ctx.Err() != nil {
			finish()
			return nil
		}
		room := unaryMaxInFlight - l.count()
		if room <= 0 {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
			}
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, unaryPullDeadline)
		began := time.Now()
		resp, err := admin.Pull(callCtx, &pubsubpb.PullRequest{Subscription: name, MaxMessages: int32(room)})
		ranOut := errors.Is(callCtx.Err(), context.DeadlineExceeded) // read before cancel, which sets the error itself
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				finish()
				return nil
			}
			if ranOut || status.Code(err) == codes.DeadlineExceeded {
				continue // the call hung or the network held it: make it again
			}
			finish()
			return err
		}
		for _, rm := range resp.GetReceivedMessages() {
			m := rm.GetMessage()
			attempt := int(rm.GetDeliveryAttempt())
			if attempt < 1 {
				attempt = 1
			}
			id := rm.GetAckId()
			l.add(id)
			var once sync.Once
			msg := &Message{ID: m.GetMessageId(), Data: m.GetData(), Attrs: m.GetAttributes(), Attempt: attempt,
				ack:  func() { once.Do(func() { l.ack(id) }) },
				nack: func() { once.Do(func() { l.nack(id) }) },
			}
			if t := m.GetPublishTime(); t != nil {
				msg.PublishedAt = t.AsTime()
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				handler(ctx, msg)
			}()
		}
		if len(resp.GetReceivedMessages()) == 0 && time.Since(began) < time.Second {
			// The service answered at once with nothing; do not ask in a tight loop.
			select {
			case <-time.After(unaryEmptyPause):
			case <-ctx.Done():
			}
		}
	}
}

// leases holds the messages that a handler has and the answers (acknowledge, give back) that are waiting to be sent.
type leases struct {
	mu       sync.Mutex
	inflight map[string]time.Time // ack id -> when it was taken
	acks     []pendingID
	nacks    []pendingID
}

type pendingID struct {
	id    string
	since time.Time
}

func (l *leases) add(id string) {
	l.mu.Lock()
	l.inflight[id] = time.Now()
	l.mu.Unlock()
}

func (l *leases) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.inflight)
}

func (l *leases) ack(id string) {
	l.mu.Lock()
	delete(l.inflight, id)
	l.acks = append(l.acks, pendingID{id, time.Now()})
	l.mu.Unlock()
}

func (l *leases) nack(id string) {
	l.mu.Lock()
	delete(l.inflight, id)
	l.nacks = append(l.nacks, pendingID{id, time.Now()})
	l.mu.Unlock()
}

// run sends the answers in batches and keeps the leases of the messages that are still being handled, until ctx ends; then
// it sends the answers that are left once more.
func (l *leases) run(ctx context.Context, ack func(context.Context, []string) error, modify func(context.Context, []string, time.Duration) error) {
	flush := time.NewTicker(unaryAckEvery)
	defer flush.Stop()
	extend := time.NewTicker(unaryLeaseEvery)
	defer extend.Stop()
	for {
		select {
		case <-ctx.Done():
			l.send(context.Background(), ack, modify)
			return
		case <-flush.C:
			l.send(ctx, ack, modify)
		case <-extend.C:
			l.extend(ctx, modify)
		}
	}
}

// send makes the calls for what is waiting. A batch that fails stays for the next turn until it is old enough that the
// service has taken the lease back anyway.
func (l *leases) send(ctx context.Context, ack func(context.Context, []string) error, modify func(context.Context, []string, time.Duration) error) {
	l.mu.Lock()
	acks, nacks := l.acks, l.nacks
	l.acks, l.nacks = nil, nil
	l.mu.Unlock()
	keep := func(p []pendingID) []pendingID {
		var out []pendingID
		for _, x := range p {
			if time.Since(x.since) < unaryAckGiveUp {
				out = append(out, x)
			}
		}
		return out
	}
	each := func(p []pendingID, call func(context.Context, []string) error) []pendingID {
		var failed []pendingID
		for len(p) > 0 {
			n := len(p)
			if n > unaryAckBatch {
				n = unaryAckBatch
			}
			ids := make([]string, n)
			for i := range ids {
				ids[i] = p[i].id
			}
			callCtx, cancel := context.WithTimeout(ctx, unaryAckTimeout)
			err := call(callCtx, ids)
			cancel()
			if err != nil {
				failed = append(failed, p[:n]...)
			}
			p = p[n:]
		}
		return keep(failed)
	}
	failedAcks := each(acks, ack)
	failedNacks := each(nacks, func(c context.Context, ids []string) error { return modify(c, ids, 0) })
	if len(failedAcks)+len(failedNacks) > 0 {
		l.mu.Lock()
		l.acks = append(failedAcks, l.acks...)
		l.nacks = append(failedNacks, l.nacks...)
		l.mu.Unlock()
	}
}

// extend keeps the lease of every message that a handler still has, up to unaryMaxLease.
func (l *leases) extend(ctx context.Context, modify func(context.Context, []string, time.Duration) error) {
	l.mu.Lock()
	var ids []string
	for id, since := range l.inflight {
		if time.Since(since) < unaryMaxLease {
			ids = append(ids, id)
		}
	}
	l.mu.Unlock()
	for len(ids) > 0 {
		n := len(ids)
		if n > unaryAckBatch {
			n = unaryAckBatch
		}
		callCtx, cancel := context.WithTimeout(ctx, unaryAckTimeout)
		_ = modify(callCtx, ids[:n], unaryLease)
		cancel()
		ids = ids[n:]
	}
}
