// Package wire defines what travels on the message bus: the attributes that
// route a message, and the payload that is sealed inside a ratchet message.
//
// The attributes are plaintext and are set by whoever publishes, so they are
// routing hints only. Nothing is trusted because of an attribute; a message is
// trusted only if it authenticates under the session's ratchet.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"claudebridge/internal/ratchet"
)

const (
	Version = 1

	AttrVersion = "v"
	AttrKind    = "kind"
	AttrAgent   = "agent"

	KindHello = "hello"
	KindAck   = "ack"
	KindMsg   = "msg"

	// MetaSeq is the Meta key under which the hub numbers the commands (text and keys) of one session, from 1. The
	// agent runs them in that order and reports a gap instead of running a later command first.
	MetaSeq = "n"

	// MaxBody bounds the text carried in one payload.
	MaxBody = 256 << 10
	// MaxData bounds one bus message.
	MaxData = ratchet.MaxPlaintext + 64<<10
)

// Type says what a payload asks for or reports.
type Type string

const (
	// hub to agent
	TypeText     Type = "text"     // text typed into the session
	TypeKey      Type = "key"      // one key: Enter, Escape, Up, Down, 1..9
	TypeLockdown Type = "lockdown" // stop acting on text and keys until unlocked locally
	TypePing     Type = "ping"
	TypeAlarm    Type = "alarm" // arm or disarm the alarm, or ask for its status: Body is "on", "off [code]" or "status"
	TypeHBAck    Type = "hback" // the hub saw the heartbeat
	TypeReceipt  Type = "rcpt"  // the hub took a reply or notice; Body is the payload id
	// agent to hub
	TypeReady     Type = "ready"   // first message of a session
	TypeReply     Type = "reply"   // output to show the user
	TypeNotice    Type = "notice"  // an event the user should see
	TypeHeartbeat Type = "hb"      // liveness, with a few facts in Meta
	TypeDropped   Type = "dropped" // a command was refused or expired; Body says why
	TypeAlert     Type = "alert"   // someone touched the machine while the alarm was armed
	TypePong      Type = "pong"
)

var (
	toAgent = map[Type]bool{TypeText: true, TypeKey: true, TypeLockdown: true, TypePing: true, TypeHBAck: true, TypeReceipt: true, TypeAlarm: true}
	toHub   = map[Type]bool{TypeReady: true, TypeReply: true, TypeNotice: true, TypeHeartbeat: true, TypeDropped: true, TypePong: true, TypeAlert: true}
)

// ErrPayload is returned for a payload that decrypts but is not acceptable.
var ErrPayload = errors.New("wire: unacceptable payload")

// Payload is the content of one message.
type Payload struct {
	ID   string            `json:"id"`
	Type Type              `json:"type"`
	Body string            `json:"body,omitempty"`
	TS   int64             `json:"ts"`
	Exp  int64             `json:"exp"`
	Meta map[string]string `json:"meta,omitempty"`
}

// Expired reports whether the payload's expiry has passed, allowing for clock skew.
func (p *Payload) Expired(now time.Time, skew time.Duration) bool {
	return now.After(time.Unix(p.Exp, 0).Add(skew))
}

// Validate checks a payload received by a party for messages travelling toHub or toAgent.
func (p *Payload) Validate(toHubDirection bool) error {
	allowed := toAgent
	if toHubDirection {
		allowed = toHub
	}
	switch {
	case p.ID == "" || len(p.ID) > 64:
		return fmt.Errorf("%w: id", ErrPayload)
	case !allowed[p.Type]:
		return fmt.Errorf("%w: type %q not allowed in this direction", ErrPayload, p.Type)
	case len(p.Body) > MaxBody:
		return fmt.Errorf("%w: body too large", ErrPayload)
	case p.Exp < p.TS:
		return fmt.Errorf("%w: expiry before timestamp", ErrPayload)
	case len(p.Meta) > 16:
		return fmt.Errorf("%w: too many meta entries", ErrPayload)
	}
	for k, v := range p.Meta {
		if len(k) > 32 || len(v) > 256 {
			return fmt.Errorf("%w: meta entry too large", ErrPayload)
		}
	}
	return nil
}

// AD is the associated data bound to every message: direction and session.
func AD(from, to string, sid [8]byte) []byte {
	var b bytes.Buffer
	b.WriteString("claudebridge msg v1\x00")
	b.WriteString(from)
	b.WriteByte(0)
	b.WriteString(to)
	b.WriteByte(0)
	b.Write(sid[:])
	return b.Bytes()
}

// Attrs builds the routing attributes for a message of the given kind.
func Attrs(kind, agent string) map[string]string {
	return map[string]string{AttrVersion: fmt.Sprint(Version), AttrKind: kind, AttrAgent: agent}
}

// Seal encrypts a payload for the other end. The result is [version][session id][ratchet message].
func Seal(r *ratchet.Ratchet, from, to string, sid [8]byte, p *Payload) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	blob, err := r.Encrypt(raw, AD(from, to, sid))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(sid)+len(blob))
	out = append(out, Version)
	out = append(out, sid[:]...)
	return append(out, blob...), nil
}

// SessionID reads the session id from a sealed message without decrypting it.
func SessionID(data []byte) ([8]byte, error) {
	var sid [8]byte
	if len(data) < 1+len(sid) || len(data) > MaxData || data[0] != Version {
		return sid, ErrPayload
	}
	copy(sid[:], data[1:9])
	return sid, nil
}

// Open decrypts and validates a sealed message. The caller says which direction it expects.
func Open(r *ratchet.Ratchet, from, to string, sid [8]byte, data []byte, toHubDirection bool) (*Payload, error) {
	got, err := SessionID(data)
	if err != nil || got != sid {
		return nil, ErrPayload
	}
	raw, err := r.Decrypt(data[9:], AD(from, to, sid))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Payload
	if err := dec.Decode(&p); err != nil || dec.More() {
		return nil, fmt.Errorf("%w: json", ErrPayload)
	}
	if err := p.Validate(toHubDirection); err != nil {
		return nil, err
	}
	return &p, nil
}

// Names builds the topic and subscription names for a deployment.
type Names struct{ Prefix string }

// DefaultNames uses the prefix "cb".
func DefaultNames() Names { return Names{Prefix: "cb"} }

// ToHub is the topic every agent publishes to.
func (n Names) ToHub() string { return n.Prefix + "-to-hub" }

// HubSub is the hub's subscription on ToHub.
func (n Names) HubSub() string { return n.Prefix + "-hub" }

// ToAgent is the topic the hub publishes to for one agent.
func (n Names) ToAgent(agent string) string { return n.Prefix + "-to-agent-" + agent }

// AgentSub is one agent's subscription on its own topic.
func (n Names) AgentSub(agent string) string { return n.Prefix + "-agent-" + agent }
