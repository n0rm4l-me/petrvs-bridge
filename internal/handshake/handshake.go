// Package handshake authenticates two parties that already pin each other's
// identity and derives the session key for the ratchet.
//
// The initiator sends a signed hello carrying a fresh X25519 key and an
// ML-KEM-768 encapsulation key. The responder answers with a signed ack
// carrying its own fresh X25519 key and an ML-KEM ciphertext. The session key
// mixes three X25519 results (ephemeral-ephemeral and both ephemeral-static)
// with the ML-KEM secret, bound to the whole transcript. The ML-KEM part makes
// the key safe against an attacker who records traffic today and gets a
// quantum computer later; the X25519 part keeps the key safe if ML-KEM turns
// out to be weak. Both private ephemeral halves live only in memory.
package handshake

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
)

const (
	Version   = 1
	KindHello = "hello"
	KindAck   = "ack"
	// MaxSkew is how far a hello's timestamp may differ from the local clock.
	MaxSkew = 2 * time.Minute
	// MaxMessageSize bounds what Decode will parse.
	MaxMessageSize = 8 << 10

	nonceLen = 16
	ephLen   = 32
)

var (
	ErrFormat    = errors.New("handshake: malformed message")
	ErrSignature = errors.New("handshake: bad signature")
	ErrStale     = errors.New("handshake: timestamp outside the allowed window")
	ErrReplay    = errors.New("handshake: hello already seen")
	ErrRef       = errors.New("handshake: ack does not answer our hello")
	ErrPeer      = errors.New("handshake: message is not from the pinned peer or not for us")
	ErrTooMany   = errors.New("handshake: too many recent hellos")
)

// Message is a hello or an ack. It travels in the clear; the signature covers every field.
type Message struct {
	V     int    `json:"v"`
	Kind  string `json:"kind"`
	From  string `json:"from"`
	To    string `json:"to"`
	Eph   []byte `json:"eph"`
	KEM   []byte `json:"kem"` // hello: encapsulation key, ack: ciphertext
	Nonce []byte `json:"nonce"`
	TS    int64  `json:"ts"`
	Ref   []byte `json:"ref,omitempty"` // ack: SHA-256 of the signed bytes of the hello it answers
	Sig   []byte `json:"sig"`
}

// Encode serialises a message.
func (m *Message) Encode() ([]byte, error) { return json.Marshal(m) }

// Decode parses a message strictly and bounds its size.
func Decode(data []byte) (*Message, error) {
	if len(data) == 0 || len(data) > MaxMessageSize {
		return nil, ErrFormat
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Message
	if err := dec.Decode(&m); err != nil {
		return nil, ErrFormat
	}
	if dec.More() {
		return nil, ErrFormat
	}
	return &m, nil
}

func lp(buf *bytes.Buffer, b []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	buf.Write(l[:])
	buf.Write(b)
}

func (m *Message) signedBytes() []byte {
	var buf bytes.Buffer
	buf.WriteString("petrvs-bridge handshake v1\x00")
	buf.WriteByte(byte(m.V))
	lp(&buf, []byte(m.Kind))
	lp(&buf, []byte(m.From))
	lp(&buf, []byte(m.To))
	lp(&buf, m.Eph)
	lp(&buf, m.KEM)
	lp(&buf, m.Nonce)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(m.TS))
	buf.Write(ts[:])
	lp(&buf, m.Ref)
	return buf.Bytes()
}

func (m *Message) digest() []byte {
	h := sha256.Sum256(m.signedBytes())
	return h[:]
}

func (m *Message) sign(id *identity.Identity) {
	m.Sig = ed25519.Sign(id.Sign, m.signedBytes())
}

// verify checks shape, direction, freshness and signature.
func (m *Message) verify(kind, self string, peer identity.Public, now time.Time) error {
	if m.V != Version || m.Kind != kind {
		return ErrFormat
	}
	if len(m.Eph) != ephLen || len(m.Nonce) != nonceLen {
		return ErrFormat
	}
	switch kind {
	case KindHello:
		if len(m.KEM) != mlkem.EncapsulationKeySize768 || len(m.Ref) != 0 {
			return ErrFormat
		}
	case KindAck:
		if len(m.KEM) != mlkem.CiphertextSize768 || len(m.Ref) != sha256.Size {
			return ErrFormat
		}
	}
	if m.To != self || m.From != peer.Name {
		return ErrPeer
	}
	if len(m.Sig) != ed25519.SignatureSize || !ed25519.Verify(peer.Sign, m.signedBytes(), m.Sig) {
		return ErrSignature
	}
	if d := now.Sub(time.Unix(m.TS, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrStale
	}
	return nil
}

// Result is what a finished handshake gives a party.
type Result struct {
	SessionKey [32]byte
	SessionID  [8]byte
	// Initiator: PeerRatchetPub is the responder's initial ratchet public key.
	PeerRatchetPub []byte
	// Responder: OwnRatchetKey is the private half of the ratchet key it offered.
	OwnRatchetKey *ecdh.PrivateKey
}

func deriveKey(secrets [][]byte, kemSecret []byte, hello, ack *Message) (key [32]byte, sid [8]byte, err error) {
	transcript := append(append([]byte(nil), hello.signedBytes()...), ack.signedBytes()...)
	salt := sha256.Sum256(transcript)
	var ikm []byte
	for _, s := range secrets {
		ikm = append(ikm, s...)
	}
	ikm = append(ikm, kemSecret...)
	out, err := hkdf.Key(sha256.New, ikm, salt[:], "petrvs-bridge session v1", 32)
	if err != nil {
		return key, sid, err
	}
	copy(key[:], out)
	h := sha256.Sum256(append([]byte("petrvs-bridge session id v1"), transcript...))
	copy(sid[:], h[:8])
	return key, sid, nil
}

// Initiator holds what is needed between sending a hello and receiving the ack.
type Initiator struct {
	id    *identity.Identity
	peer  identity.Public
	eph   *ecdh.PrivateKey
	dk    *mlkem.DecapsulationKey768
	hello *Message
}

// NewInitiator creates the signed hello to send to peer.
func NewInitiator(id *identity.Identity, peer identity.Public, now time.Time) (*Initiator, *Message, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	m := &Message{V: Version, Kind: KindHello, From: id.Name, To: peer.Name, Eph: eph.PublicKey().Bytes(),
		KEM: dk.EncapsulationKey().Bytes(), Nonce: nonce, TS: now.Unix()}
	m.sign(id)
	return &Initiator{id: id, peer: peer, eph: eph, dk: dk, hello: m}, m, nil
}

// ID identifies this handshake: the value an ack carries in Ref.
func (i *Initiator) ID() string { return string(i.hello.digest()) }

// AckRef returns the handshake id an ack answers.
func (m *Message) AckRef() string { return string(m.Ref) }

// Complete verifies the ack and derives the session.
func (i *Initiator) Complete(ack *Message, now time.Time) (*Result, error) {
	if err := ack.verify(KindAck, i.id.Name, i.peer, now); err != nil {
		return nil, err
	}
	if !bytes.Equal(ack.Ref, i.hello.digest()) {
		return nil, ErrRef
	}
	peerEph, err := ecdh.X25519().NewPublicKey(ack.Eph)
	if err != nil {
		return nil, ErrFormat
	}
	peerStatic, err := ecdh.X25519().NewPublicKey(i.peer.Static)
	if err != nil {
		return nil, ErrFormat
	}
	a, err := i.eph.ECDH(peerEph)
	if err != nil {
		return nil, ErrFormat
	}
	b, err := i.eph.ECDH(peerStatic)
	if err != nil {
		return nil, ErrFormat
	}
	c, err := i.id.Static.ECDH(peerEph)
	if err != nil {
		return nil, ErrFormat
	}
	ss, err := i.dk.Decapsulate(ack.KEM)
	if err != nil {
		return nil, ErrFormat
	}
	key, sid, err := deriveKey([][]byte{a, b, c}, ss, i.hello, ack)
	if err != nil {
		return nil, err
	}
	return &Result{SessionKey: key, SessionID: sid, PeerRatchetPub: append([]byte(nil), ack.Eph...)}, nil
}

// Respond verifies a hello and returns the ack to send back plus the session.
// If guard is not nil, a hello whose nonce was already seen is refused; the
// nonce is recorded only after the signature and freshness checks pass, so
// only the pinned peer can fill the guard.
func Respond(id *identity.Identity, peer identity.Public, hello *Message, now time.Time, guard *Guard) (*Message, *Result, error) {
	if err := hello.verify(KindHello, id.Name, peer, now); err != nil {
		return nil, nil, err
	}
	if guard != nil {
		if err := guard.Check(hello, now); err != nil {
			return nil, nil, err
		}
	}
	peerEph, err := ecdh.X25519().NewPublicKey(hello.Eph)
	if err != nil {
		return nil, nil, ErrFormat
	}
	peerStatic, err := ecdh.X25519().NewPublicKey(peer.Static)
	if err != nil {
		return nil, nil, ErrFormat
	}
	ek, err := mlkem.NewEncapsulationKey768(hello.KEM)
	if err != nil {
		return nil, nil, ErrFormat
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	a, err := eph.ECDH(peerEph)
	if err != nil {
		return nil, nil, ErrFormat
	}
	b, err := id.Static.ECDH(peerEph)
	if err != nil {
		return nil, nil, ErrFormat
	}
	c, err := eph.ECDH(peerStatic)
	if err != nil {
		return nil, nil, ErrFormat
	}
	ss, ct := ek.Encapsulate()
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ack := &Message{V: Version, Kind: KindAck, From: id.Name, To: peer.Name, Eph: eph.PublicKey().Bytes(),
		KEM: ct, Nonce: nonce, TS: now.Unix(), Ref: hello.digest()}
	ack.sign(id)
	key, sid, err := deriveKey([][]byte{a, b, c}, ss, hello, ack)
	if err != nil {
		return nil, nil, err
	}
	return ack, &Result{SessionKey: key, SessionID: sid, OwnRatchetKey: eph}, nil
}

// VerifyHello checks a hello's shape, direction, freshness and signature without recording anything. It is the cheap
// test to run before rate limiting and before the expensive part of Respond.
func VerifyHello(id *identity.Identity, peer identity.Public, hello *Message, now time.Time) error {
	return hello.verify(KindHello, id.Name, peer, now)
}

// Guard remembers recent hello nonces so a captured hello cannot be replayed
// to reset a session. It is bounded and forgets entries older than the skew window.
type Guard struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	max       int
	perSender int
}

// NewGuard returns a guard holding at most max nonces.
func NewGuard(max int) *Guard { return &Guard{seen: map[string]time.Time{}, max: max, perSender: max} }

// NewGuardPerSender also caps how many nonces one sender may hold, so one pinned party that misbehaves (or whose key
// leaked) cannot fill the guard and lock the others out.
func NewGuardPerSender(max, perSender int) *Guard {
	return &Guard{seen: map[string]time.Time{}, max: max, perSender: perSender}
}

// Forget removes the nonce of a hello that Check accepted but that could not be answered, so that the bus delivering the
// same hello again is not refused as a replay. Only a caller that has just accepted the hello should use it: the nonce
// was signed by the pinned peer, so forgetting it lets at most that one hello be answered a second time.
func (g *Guard) Forget(m *Message) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, m.From+"|"+string(m.Nonce))
}

// Check records the hello's nonce and reports ErrReplay if it was already seen.
func (g *Guard) Check(m *Message, now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	own := 0
	prefix := m.From + "|"
	for k, t := range g.seen {
		if now.Sub(t) > 2*MaxSkew {
			delete(g.seen, k)
		} else if strings.HasPrefix(k, prefix) {
			own++
		}
	}
	key := prefix + string(m.Nonce)
	if _, dup := g.seen[key]; dup {
		return ErrReplay
	}
	if len(g.seen) >= g.max || own >= g.perSender {
		return ErrTooMany
	}
	g.seen[key] = now
	return nil
}
