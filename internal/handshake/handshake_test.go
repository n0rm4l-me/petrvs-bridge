package handshake

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/mlkem"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"claudebridge/internal/identity"
	"claudebridge/internal/ratchet"
)

var t0 = time.Unix(1_800_000_000, 0)

type world struct {
	agent, hub *identity.Identity
}

func newWorld(t testing.TB) world {
	t.Helper()
	a, err := identity.Generate("laptop")
	if err != nil {
		t.Fatal(err)
	}
	h, err := identity.Generate("hub")
	if err != nil {
		t.Fatal(err)
	}
	return world{agent: a, hub: h}
}

// run performs a full handshake and returns both results.
func (w world) run(t testing.TB) (*Result, *Result) {
	t.Helper()
	ini, hello, err := NewInitiator(w.agent, w.hub.Public(), t0)
	if err != nil {
		t.Fatal(err)
	}
	ack, hubRes, err := Respond(w.hub, w.agent.Public(), hello, t0, nil)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	agentRes, err := ini.Complete(ack, t0)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	return agentRes, hubRes
}

func TestBothSidesDeriveTheSameSession(t *testing.T) {
	w := newWorld(t)
	a, h := w.run(t)
	if a.SessionKey != h.SessionKey {
		t.Fatal("session keys differ")
	}
	if a.SessionID != h.SessionID {
		t.Fatal("session ids differ")
	}
	if !bytes.Equal(a.PeerRatchetPub, h.OwnRatchetKey.PublicKey().Bytes()) {
		t.Fatal("the initiator does not hold the responder's ratchet public key")
	}
	if a.SessionKey == ([32]byte{}) {
		t.Fatal("empty session key")
	}
}

func TestEachHandshakeGivesFreshKeys(t *testing.T) {
	w := newWorld(t)
	a1, _ := w.run(t)
	a2, _ := w.run(t)
	if a1.SessionKey == a2.SessionKey || a1.SessionID == a2.SessionID {
		t.Fatal("two handshakes between the same parties produced the same session")
	}
}

func TestSessionWorksWithTheRatchet(t *testing.T) {
	w := newWorld(t)
	a, h := w.run(t)
	agent, err := ratchet.NewInitiator(a.SessionKey, a.PeerRatchetPub)
	if err != nil {
		t.Fatal(err)
	}
	hub := ratchet.NewResponder(h.SessionKey, h.OwnRatchetKey)
	ad := []byte("ad")
	m, _ := agent.Encrypt([]byte("ready"), ad)
	got, err := hub.Decrypt(m, ad)
	if err != nil || string(got) != "ready" {
		t.Fatalf("%v %q", err, got)
	}
	r, _ := hub.Encrypt([]byte("ok"), ad)
	if got, err := agent.Decrypt(r, ad); err != nil || string(got) != "ok" {
		t.Fatalf("%v %q", err, got)
	}
}

func TestUnpinnedSenderIsRejected(t *testing.T) {
	w := newWorld(t)
	eve, _ := identity.Generate("laptop") // same name, different keys
	_, hello, _ := NewInitiator(eve, w.hub.Public(), t0)
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0, nil); !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v, want ErrSignature", err)
	}
	eve2, _ := identity.Generate("intruder")
	_, hello, _ = NewInitiator(eve2, w.hub.Public(), t0)
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0, nil); !errors.Is(err, ErrPeer) {
		t.Fatalf("got %v, want ErrPeer", err)
	}
}

func TestHelloForSomeoneElseIsRejected(t *testing.T) {
	w := newWorld(t)
	other, _ := identity.Generate("other-hub")
	_, hello, _ := NewInitiator(w.agent, other.Public(), t0) // addressed to other-hub
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0, nil); !errors.Is(err, ErrPeer) {
		t.Fatalf("got %v, want ErrPeer", err)
	}
}

func TestEveryFieldIsSigned(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	mutate := map[string]func(*Message){
		"eph":   func(m *Message) { m.Eph[0] ^= 1 },
		"kem":   func(m *Message) { m.KEM[10] ^= 1 },
		"nonce": func(m *Message) { m.Nonce[0] ^= 1 },
		"ts":    func(m *Message) { m.TS++ },
		"from":  func(m *Message) { m.From = "laptoq" },
		"to":    func(m *Message) { m.To = "hub2" },
		"ref":   func(m *Message) { m.Ref = []byte("x") },
		"v":     func(m *Message) { m.V = 2 },
	}
	for name, f := range mutate {
		m := *hello
		m.Eph, m.KEM, m.Nonce, m.Ref = append([]byte(nil), hello.Eph...), append([]byte(nil), hello.KEM...), append([]byte(nil), hello.Nonce...), append([]byte(nil), hello.Ref...)
		f(&m)
		if _, _, err := Respond(w.hub, w.agent.Public(), &m, t0, nil); err == nil {
			t.Errorf("tampering with %s was not detected", name)
		}
	}
}

func TestStaleAndFutureHellosAreRejected(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	for name, now := range map[string]time.Time{"old": t0.Add(MaxSkew + time.Second), "future": t0.Add(-MaxSkew - time.Second)} {
		if _, _, err := Respond(w.hub, w.agent.Public(), hello, now, nil); !errors.Is(err, ErrStale) {
			t.Errorf("%s: got %v, want ErrStale", name, err)
		}
	}
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0.Add(MaxSkew-time.Second), nil); err != nil {
		t.Errorf("inside the window: %v", err)
	}
}

func TestReplayedHelloIsRejected(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	g := NewGuard(100)
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0, g); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0.Add(time.Second), g); !errors.Is(err, ErrReplay) {
		t.Fatalf("got %v, want ErrReplay", err)
	}
}

func TestGuardIsBoundedAndForgets(t *testing.T) {
	w := newWorld(t)
	g := NewGuard(3)
	for i := 0; i < 3; i++ {
		_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
		if err := g.Check(hello, t0); err != nil {
			t.Fatal(err)
		}
	}
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	if err := g.Check(hello, t0); err == nil {
		t.Fatal("the guard grew past its limit")
	}
	if err := g.Check(hello, t0.Add(3*MaxSkew)); err != nil {
		t.Fatalf("old entries were not forgotten: %v", err)
	}
}

func TestGuardOnlyRecordsAuthenticatedHellos(t *testing.T) {
	w := newWorld(t)
	g := NewGuard(2)
	eve, _ := identity.Generate("laptop")
	for i := 0; i < 10; i++ { // a flood of forged hellos must not fill the guard
		_, hello, _ := NewInitiator(eve, w.hub.Public(), t0)
		Respond(w.hub, w.agent.Public(), hello, t0, g)
	}
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	if _, _, err := Respond(w.hub, w.agent.Public(), hello, t0, g); err != nil {
		t.Fatalf("the real hello was refused after a flood of forged ones: %v", err)
	}
}

func TestAckMustAnswerOurHello(t *testing.T) {
	w := newWorld(t)
	ini1, hello1, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	_, hello2, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	ack2, _, err := Respond(w.hub, w.agent.Public(), hello2, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ini1.Complete(ack2, t0); !errors.Is(err, ErrRef) {
		t.Fatalf("an ack for another hello was accepted: %v", err)
	}
	_ = hello1
}

func TestAckFromAnImpostorIsRejected(t *testing.T) {
	w := newWorld(t)
	ini, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	eve, _ := identity.Generate("hub")
	ack, _, err := Respond(eve, w.agent.Public(), hello, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ini.Complete(ack, t0); !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v, want ErrSignature", err)
	}
}

func TestKindConfusionAndReflection(t *testing.T) {
	w := newWorld(t)
	ini, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	if _, err := ini.Complete(hello, t0); err == nil { // a hello presented as an ack
		t.Fatal("a hello was accepted as an ack")
	}
	ack, _, _ := Respond(w.hub, w.agent.Public(), hello, t0, nil)
	if _, _, err := Respond(w.hub, w.agent.Public(), ack, t0, nil); err == nil { // an ack presented as a hello
		t.Fatal("an ack was accepted as a hello")
	}
	// reflecting the agent's own hello back to the agent
	if _, _, err := Respond(w.agent, w.hub.Public(), hello, t0, nil); err == nil {
		t.Fatal("a reflected hello was accepted")
	}
}

// An attacker who stole the hub's signing key (but not its static key) can forge a validly signed ack,
// yet cannot derive the session key, so nothing it sends is accepted.
func TestStolenSigningKeyAloneDoesNotGiveTheSession(t *testing.T) {
	w := newWorld(t)
	ini, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	thief, _ := identity.Generate("hub")
	thief.Sign = w.hub.Sign // has the real signing key, but its own static key
	ack, thiefRes, err := Respond(thief, w.agent.Public(), hello, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ack.Sig = nil
	ack.sign(w.hub) // signed with the stolen key
	agentRes, err := ini.Complete(ack, t0)
	if err != nil {
		t.Fatalf("the forged ack should verify (the signing key is genuine): %v", err)
	}
	if agentRes.SessionKey == thiefRes.SessionKey {
		t.Fatal("the thief derived the same session key without the hub's static key")
	}
	agent, _ := ratchet.NewInitiator(agentRes.SessionKey, agentRes.PeerRatchetPub)
	imp := ratchet.NewResponder(thiefRes.SessionKey, thiefRes.OwnRatchetKey)
	m, _ := agent.Encrypt([]byte("secret"), nil)
	if _, err := imp.Decrypt(m, nil); err == nil {
		t.Fatal("the impostor read the agent's message")
	}
}

// Replacing the ML-KEM ciphertext in a correctly re-signed ack changes the key, so the KEM really is part of the secret.
func TestKEMSecretContributesToTheKey(t *testing.T) {
	w := newWorld(t)
	ini, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	ack, hubRes, _ := Respond(w.hub, w.agent.Public(), hello, t0, nil)
	otherDK, _ := mlkem.GenerateKey768()
	_, otherCT := otherDK.EncapsulationKey().Encapsulate()
	forged := *ack
	forged.KEM = otherCT
	forged.Sig = nil
	forged.sign(w.hub)
	res, err := ini.Complete(&forged, t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionKey == hubRes.SessionKey {
		t.Fatal("changing the ML-KEM ciphertext did not change the session key")
	}
}

func TestTranscriptBindingChangesTheKey(t *testing.T) {
	w := newWorld(t)
	_, h1 := w.run(t)
	_, h2 := w.run(t)
	if h1.SessionKey == h2.SessionKey {
		t.Fatal("identical keys for different transcripts")
	}
}

func TestDecodeIsStrict(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	enc, _ := hello.Encode()
	if _, err := Decode(enc); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"empty": nil, "garbage": []byte("nope"), "trailing": append(append([]byte(nil), enc...), []byte(` {"v":1}`)...),
		"unknown field": []byte(`{"v":1,"kind":"hello","extra":true}`), "oversize": bytes.Repeat([]byte("a"), MaxMessageSize+1),
	} {
		if _, err := Decode(in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestWrongSizedFieldsAreRejectedBeforeAnyCrypto(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	m := *hello
	m.KEM = m.KEM[:100]
	m.Sig = nil
	m.sign(w.agent)
	if _, _, err := Respond(w.hub, w.agent.Public(), &m, t0, nil); !errors.Is(err, ErrFormat) {
		t.Fatalf("got %v, want ErrFormat", err)
	}
	m = *hello
	m.Eph = make([]byte, 31)
	if _, _, err := Respond(w.hub, w.agent.Public(), &m, t0, nil); !errors.Is(err, ErrFormat) {
		t.Fatalf("got %v, want ErrFormat", err)
	}
}

func TestDegenerateEphemeralKeyIsRejected(t *testing.T) {
	w := newWorld(t)
	_, hello, _ := NewInitiator(w.agent, w.hub.Public(), t0)
	m := *hello
	m.Eph = make([]byte, 32) // all-zero: a low-order point
	m.Sig = nil
	m.sign(w.agent) // even correctly signed by the real peer
	if _, _, err := Respond(w.hub, w.agent.Public(), &m, t0, nil); err == nil {
		t.Fatal("a low-order ephemeral key was accepted")
	}
}

var _ = ecdh.X25519
var _ = ed25519.PublicKeySize
var _ = rand.Reader
