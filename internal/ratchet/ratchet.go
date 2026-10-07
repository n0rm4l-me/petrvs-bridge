// Package ratchet implements a double ratchet for a pair of parties that have
// already agreed on a shared session key (see package handshake).
//
// Every message is encrypted under its own one-time key. The key chain moves
// forward with each message (per-message forward secrecy), and each time a
// party replies it mixes in a fresh Diffie-Hellman key created and sent only
// inside the channel (post-compromise security). Only the standard library is
// used: X25519, HKDF-SHA256, HMAC-SHA256 and AES-256-GCM.
//
// Decrypt is atomic. It works on a copy of the state and keeps the copy only
// if the message authenticates. Without that, a single forged packet carrying
// a fresh key in its header would advance the state and cut the real session.
package ratchet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// MaxSkip is how many message keys may be skipped inside one chain when a
	// sender jumps ahead. It bounds the work and memory an attacker can force.
	MaxSkip = 50
	// MaxStoredSkipped bounds the skipped keys kept in total; the oldest are dropped.
	MaxStoredSkipped = 200
	// PadBucket is the size unit messages are padded to, so short messages look alike.
	PadBucket = 1024
	// MaxPlaintext is the largest message accepted by Encrypt.
	MaxPlaintext = 1 << 20
	// DHStepBurst and DHStepPerSecond are the default budget of new peer keys that Decrypt will try. A new key costs
	// two Diffie-Hellman operations, a key generation and a copy of the state, and anyone who can publish to the bus
	// can offer one in every forged packet; a real peer offers one per round trip.
	DHStepBurst     = 20
	DHStepPerSecond = 5.0

	keyLen    = 32
	headerLen = keyLen + 4 + 4 // ratchet public key, previous chain length, message number
	tagLen    = 16
)

var (
	ErrNoSendChain    = errors.New("ratchet: nothing received yet, no sending chain")
	ErrAuth           = errors.New("ratchet: message did not authenticate")
	ErrReplay         = errors.New("ratchet: message key already used or too old")
	ErrTooManySkipped = errors.New("ratchet: too many skipped messages")
	ErrMalformed      = errors.New("ratchet: malformed message")
	ErrTooLarge       = errors.New("ratchet: message too large")
	ErrRateLimited    = errors.New("ratchet: too many new keys offered, slow down")
)

type skipID struct {
	dh [keyLen]byte
	n  uint32
}

type state struct {
	rk             [keyLen]byte
	dhs            *ecdh.PrivateKey
	dhr            []byte // peer's current ratchet public key, nil until known
	cks, ckr       [keyLen]byte
	hasCKs, hasCKr bool
	ns, nr, pn     uint32
	skipped        map[skipID][keyLen]byte
	order          []skipID
}

func (s *state) clone() *state {
	c := *s
	if s.dhr != nil {
		c.dhr = append([]byte(nil), s.dhr...)
	}
	c.skipped = make(map[skipID][keyLen]byte, len(s.skipped))
	for k, v := range s.skipped {
		c.skipped[k] = v
	}
	c.order = append([]skipID(nil), s.order...)
	return &c
}

// Ratchet is one party's end of a session. It is safe for concurrent use.
type Ratchet struct {
	mu sync.Mutex
	st *state

	// budget of DH steps, a token bucket
	tokens, burst, rate float64
	last                time.Time
	now                 func() time.Time
}

func newRatchet(s *state) *Ratchet {
	return &Ratchet{st: s, tokens: DHStepBurst, burst: DHStepBurst, rate: DHStepPerSecond, now: time.Now}
}

// SetDHStepLimit changes the budget of new peer keys Decrypt will try: a burst, refilled at perSecond. A nil clock
// means the real one. It exists for tests and for unusual deployments.
func (r *Ratchet) SetDHStepLimit(burst int, perSecond float64, clock func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens, r.burst, r.rate = float64(burst), float64(burst), perSecond
	r.last = time.Time{}
	if clock == nil {
		clock = time.Now
	}
	r.now = clock
}

// takeStep spends one token; the caller holds r.mu.
func (r *Ratchet) takeStep() bool {
	now := r.now()
	if !r.last.IsZero() {
		r.tokens += now.Sub(r.last).Seconds() * r.rate
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
	}
	r.last = now
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}

// wouldStep reports whether blob offers a peer key that is neither the current one nor covered by a stored skipped
// key, so that decrypting it would start a DH step.
func (s *state) wouldStep(blob []byte) bool {
	if len(blob) < headerLen {
		return false
	}
	var id skipID
	copy(id.dh[:], blob[:keyLen])
	id.n = binary.BigEndian.Uint32(blob[keyLen+4:])
	if _, ok := s.skipped[id]; ok {
		return false
	}
	return s.dhr == nil || string(blob[:keyLen]) != string(s.dhr)
}

var x25519 = ecdh.X25519()

// NewInitiator starts the party that sends first. peerRatchetPub is the other
// side's initial ratchet public key, learned during the handshake.
func NewInitiator(sessionKey [keyLen]byte, peerRatchetPub []byte) (*Ratchet, error) {
	pub, err := x25519.NewPublicKey(peerRatchetPub)
	if err != nil {
		return nil, fmt.Errorf("ratchet: bad peer key: %w", err)
	}
	dhs, err := x25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	dh, err := dhs.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("ratchet: bad peer key: %w", err)
	}
	s := &state{dhs: dhs, dhr: append([]byte(nil), peerRatchetPub...), skipped: map[skipID][keyLen]byte{}}
	s.rk, s.cks = kdfRK(sessionKey, dh)
	s.hasCKs = true
	return newRatchet(s), nil
}

// NewResponder starts the party that waits for the first message. ownRatchetKey
// is the private half of the ratchet public key it gave the initiator.
func NewResponder(sessionKey [keyLen]byte, ownRatchetKey *ecdh.PrivateKey) *Ratchet {
	return newRatchet(&state{rk: sessionKey, dhs: ownRatchetKey, skipped: map[skipID][keyLen]byte{}})
}

func kdfRK(rk [keyLen]byte, dh []byte) (newRK, ck [keyLen]byte) {
	out, err := hkdf.Key(sha256.New, dh, rk[:], "petrvs-bridge ratchet root v1", 2*keyLen)
	if err != nil {
		panic(err) // cannot happen: the output length is within the HKDF limit
	}
	copy(newRK[:], out[:keyLen])
	copy(ck[:], out[keyLen:])
	return
}

func kdfCK(ck [keyLen]byte) (next, mk [keyLen]byte) {
	m := hmac.New(sha256.New, ck[:])
	m.Write([]byte{2})
	copy(next[:], m.Sum(nil))
	m = hmac.New(sha256.New, ck[:])
	m.Write([]byte{1})
	copy(mk[:], m.Sum(nil))
	return
}

func aeadFor(mk [keyLen]byte) (cipher.AEAD, []byte) {
	out, err := hkdf.Key(sha256.New, mk[:], nil, "petrvs-bridge message v1", keyLen+12)
	if err != nil {
		panic(err)
	}
	block, err := aes.NewCipher(out[:keyLen])
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return gcm, out[keyLen:]
}

func pad(b []byte) []byte {
	n := len(b) + 1
	total := (n + PadBucket - 1) / PadBucket * PadBucket
	out := make([]byte, total)
	copy(out, b)
	out[len(b)] = 0x80
	return out
}

func unpad(b []byte) ([]byte, error) {
	i := len(b) - 1
	for i >= 0 && b[i] == 0 {
		i--
	}
	if i < 0 || b[i] != 0x80 {
		return nil, ErrMalformed
	}
	return b[:i], nil
}

// Encrypt seals plaintext. ad is authenticated but not encrypted; both sides
// must pass the same value. It fails with ErrNoSendChain for a responder that
// has not yet received a message.
func (r *Ratchet) Encrypt(plaintext, ad []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, ErrTooLarge
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st
	if !s.hasCKs {
		return nil, ErrNoSendChain
	}
	var mk [keyLen]byte
	s.cks, mk = kdfCK(s.cks)
	header := make([]byte, headerLen)
	copy(header, s.dhs.PublicKey().Bytes())
	binary.BigEndian.PutUint32(header[keyLen:], s.pn)
	binary.BigEndian.PutUint32(header[keyLen+4:], s.ns)
	s.ns++
	gcm, nonce := aeadFor(mk)
	return append(header, gcm.Seal(nil, nonce, pad(plaintext), joinAD(header, ad))...), nil
}

func joinAD(header, ad []byte) []byte {
	out := make([]byte, 0, len(header)+len(ad))
	return append(append(out, header...), ad...)
}

// Decrypt opens a message. On any error the state is left exactly as it was.
func (r *Ratchet) Decrypt(blob, ad []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.wouldStep(blob) && !r.takeStep() {
		return nil, ErrRateLimited
	}
	work := r.st.clone()
	plain, err := work.decrypt(blob, ad)
	if err != nil {
		return nil, err
	}
	r.st = work
	return plain, nil
}

func (s *state) decrypt(blob, ad []byte) ([]byte, error) {
	if len(blob) < headerLen+tagLen+PadBucket {
		return nil, ErrMalformed
	}
	header, ct := blob[:headerLen], blob[headerLen:]
	var dh [keyLen]byte
	copy(dh[:], header[:keyLen])
	pn := binary.BigEndian.Uint32(header[keyLen:])
	n := binary.BigEndian.Uint32(header[keyLen+4:])

	if mk, ok := s.skipped[skipID{dh, n}]; ok {
		plain, err := open(mk, header, ad, ct)
		if err != nil {
			return nil, err
		}
		s.dropSkipped(skipID{dh, n})
		return plain, nil
	}
	if s.dhr == nil || string(dh[:]) != string(s.dhr) {
		if err := s.skip(pn); err != nil {
			return nil, err
		}
		pub, err := x25519.NewPublicKey(dh[:])
		if err != nil {
			return nil, ErrMalformed
		}
		secret, err := s.dhs.ECDH(pub)
		if err != nil {
			return nil, ErrAuth // a degenerate (low-order) key
		}
		s.pn, s.ns, s.nr = s.ns, 0, 0
		s.dhr = append([]byte(nil), dh[:]...)
		s.rk, s.ckr = kdfRK(s.rk, secret)
		s.hasCKr = true
		fresh, err := x25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		s.dhs = fresh
		secret2, err := fresh.ECDH(pub)
		if err != nil {
			return nil, ErrAuth
		}
		s.rk, s.cks = kdfRK(s.rk, secret2)
		s.hasCKs = true
	} else if n < s.nr {
		return nil, ErrReplay
	}
	if err := s.skip(n); err != nil {
		return nil, err
	}
	var mk [keyLen]byte
	s.ckr, mk = kdfCK(s.ckr)
	s.nr++
	return open(mk, header, ad, ct)
}

func open(mk [keyLen]byte, header, ad, ct []byte) ([]byte, error) {
	gcm, nonce := aeadFor(mk)
	padded, err := gcm.Open(nil, nonce, ct, joinAD(header, ad))
	if err != nil {
		return nil, ErrAuth
	}
	return unpad(padded)
}

// skip stores the keys of messages that were not received, up to message number until.
func (s *state) skip(until uint32) error {
	if !s.hasCKr || until <= s.nr {
		return nil
	}
	if until-s.nr > MaxSkip {
		return ErrTooManySkipped
	}
	for s.nr < until {
		var mk [keyLen]byte
		s.ckr, mk = kdfCK(s.ckr)
		id := skipID{n: s.nr}
		copy(id.dh[:], s.dhr)
		s.skipped[id] = mk
		s.order = append(s.order, id)
		s.nr++
	}
	for len(s.skipped) > MaxStoredSkipped {
		s.dropSkipped(s.order[0])
	}
	return nil
}

func (s *state) dropSkipped(id skipID) {
	delete(s.skipped, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// SkippedKeys reports how many skipped message keys are being held.
func (r *Ratchet) SkippedKeys() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.st.skipped)
}

type skippedJSON struct {
	DH  []byte `json:"dh"`
	N   uint32 `json:"n"`
	Key []byte `json:"key"`
}

type stateJSON struct {
	RK      []byte        `json:"rk"`
	DHs     []byte        `json:"dhs"`
	DHr     []byte        `json:"dhr,omitempty"`
	CKs     []byte        `json:"cks,omitempty"`
	CKr     []byte        `json:"ckr,omitempty"`
	Ns      uint32        `json:"ns"`
	Nr      uint32        `json:"nr"`
	PN      uint32        `json:"pn"`
	Skipped []skippedJSON `json:"skipped,omitempty"`
}

// Marshal serialises the state so it can be stored (encrypted by the caller).
// Restoring an older copy than the one the peer has seen breaks the session.
func (r *Ratchet) Marshal() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st
	j := stateJSON{RK: s.rk[:], DHs: s.dhs.Bytes(), DHr: s.dhr, Ns: s.ns, Nr: s.nr, PN: s.pn}
	if s.hasCKs {
		j.CKs = s.cks[:]
	}
	if s.hasCKr {
		j.CKr = s.ckr[:]
	}
	for _, id := range s.order {
		mk := s.skipped[id]
		j.Skipped = append(j.Skipped, skippedJSON{DH: id.dh[:], N: id.n, Key: mk[:]})
	}
	return json.Marshal(j)
}

// Unmarshal restores a state produced by Marshal.
func Unmarshal(data []byte) (*Ratchet, error) {
	var j stateJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("ratchet: bad state: %w", err)
	}
	dhs, err := x25519.NewPrivateKey(j.DHs)
	if err != nil || len(j.RK) != keyLen {
		return nil, errors.New("ratchet: bad state")
	}
	s := &state{dhs: dhs, dhr: j.DHr, ns: j.Ns, nr: j.Nr, pn: j.PN, skipped: map[skipID][keyLen]byte{}}
	copy(s.rk[:], j.RK)
	if len(j.CKs) == keyLen {
		copy(s.cks[:], j.CKs)
		s.hasCKs = true
	}
	if len(j.CKr) == keyLen {
		copy(s.ckr[:], j.CKr)
		s.hasCKr = true
	}
	for _, k := range j.Skipped {
		if len(k.DH) != keyLen || len(k.Key) != keyLen {
			return nil, errors.New("ratchet: bad state")
		}
		id := skipID{n: k.N}
		copy(id.dh[:], k.DH)
		var mk [keyLen]byte
		copy(mk[:], k.Key)
		s.skipped[id] = mk
		s.order = append(s.order, id)
	}
	return newRatchet(s), nil
}
