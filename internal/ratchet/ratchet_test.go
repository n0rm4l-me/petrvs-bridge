package ratchet

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sort"
	"sync"
	"testing"
)

var testAD = []byte("session-1|agent->hub")

// pair returns an initiator (a) and a responder (b) that share a session key.
func pair(t testing.TB) (a, b *Ratchet) {
	t.Helper()
	var sk [32]byte
	if _, err := rand.Read(sk[:]); err != nil {
		t.Fatal(err)
	}
	hubKey, err := x25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err = NewInitiator(sk, hubKey.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a, NewResponder(sk, hubKey)
}

func mustEnc(t testing.TB, r *Ratchet, msg string) []byte {
	t.Helper()
	b, err := r.Encrypt([]byte(msg), testAD)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return b
}

func mustDec(t testing.TB, r *Ratchet, blob []byte, want string) {
	t.Helper()
	got, err := r.Decrypt(blob, testAD)
	if err != nil {
		t.Fatalf("decrypt %q: %v", want, err)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func snapshot(t testing.TB, r *Ratchet) *Ratchet {
	t.Helper()
	data, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func stateBytes(t testing.TB, r *Ratchet) []byte {
	t.Helper()
	b, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTripBothDirections(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "status?"), "status?")
	mustDec(t, a, mustEnc(t, b, "all good"), "all good")
	for i := 0; i < 20; i++ {
		mustDec(t, b, mustEnc(t, a, "ping"), "ping")
		mustDec(t, a, mustEnc(t, b, "pong"), "pong")
	}
}

func TestResponderCannotSendBeforeReceiving(t *testing.T) {
	_, b := pair(t)
	if _, err := b.Encrypt([]byte("too early"), testAD); !errors.Is(err, ErrNoSendChain) {
		t.Fatalf("got %v, want ErrNoSendChain", err)
	}
}

func TestEveryMessageHasItsOwnCiphertext(t *testing.T) {
	a, _ := pair(t)
	first, second := mustEnc(t, a, "same text"), mustEnc(t, a, "same text")
	if bytes.Equal(first, second) || bytes.Equal(first[headerLen:], second[headerLen:]) {
		t.Fatal("two messages with the same plaintext produced the same ciphertext")
	}
}

func TestPlaintextNotVisibleOnTheWire(t *testing.T) {
	a, _ := pair(t)
	secret := "very-recognisable-plaintext-0123456789"
	if bytes.Contains(mustEnc(t, a, secret), []byte(secret)) {
		t.Fatal("plaintext appears in the ciphertext")
	}
}

func TestOutOfOrderWithinAChain(t *testing.T) {
	a, b := pair(t)
	m1, m2, m3, m4 := mustEnc(t, a, "one"), mustEnc(t, a, "two"), mustEnc(t, a, "three"), mustEnc(t, a, "four")
	mustDec(t, b, m4, "four")
	mustDec(t, b, m2, "two")
	mustDec(t, b, m1, "one")
	mustDec(t, b, m3, "three")
	if b.SkippedKeys() != 0 {
		t.Fatalf("skipped keys left over: %d", b.SkippedKeys())
	}
}

func TestOutOfOrderAcrossAKeyStep(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello")
	late := mustEnc(t, a, "late message of the old chain")
	mustDec(t, a, mustEnc(t, b, "reply"), "reply") // a now moves to a new chain
	newer := mustEnc(t, a, "message of the new chain")
	mustDec(t, b, newer, "message of the new chain") // b sees the new chain first
	mustDec(t, b, late, "late message of the old chain")
}

func TestReplayIsRejected(t *testing.T) {
	a, b := pair(t)
	m := mustEnc(t, a, "approve 1")
	mustDec(t, b, m, "approve 1")
	if _, err := b.Decrypt(m, testAD); err == nil {
		t.Fatal("a replayed message was accepted")
	}
	// A replay of a message that was skipped and later received must also fail.
	m2, m3 := mustEnc(t, a, "two"), mustEnc(t, a, "three")
	mustDec(t, b, m3, "three")
	mustDec(t, b, m2, "two")
	if _, err := b.Decrypt(m2, testAD); err == nil {
		t.Fatal("a replayed skipped message was accepted")
	}
}

func TestWrongAssociatedDataIsRejected(t *testing.T) {
	a, b := pair(t)
	m := mustEnc(t, a, "for the hub")
	if _, err := b.Decrypt(m, []byte("session-1|hub->agent")); !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth (a message must not be movable to another direction or session)", err)
	}
	mustDec(t, b, m, "for the hub") // and the failed attempt did not disturb anything
}

func TestEveryByteIsAuthenticated(t *testing.T) {
	a, b := pair(t)
	m := mustEnc(t, a, "integrity")
	for _, pos := range []int{0, 5, 31, 32, 35, 36, 39, 40, 100, len(m) / 2, len(m) - 17, len(m) - 1} {
		t2 := append([]byte(nil), m...)
		t2[pos] ^= 0x01
		if _, err := b.Decrypt(t2, testAD); err == nil {
			t.Fatalf("flipping bit in byte %d was not detected", pos)
		}
	}
	mustDec(t, b, m, "integrity")
}

func TestTruncatedAndEmptyInput(t *testing.T) {
	_, b := pair(t)
	for _, in := range [][]byte{nil, {}, {1}, make([]byte, headerLen), make([]byte, headerLen+tagLen)} {
		if _, err := b.Decrypt(in, testAD); err == nil {
			t.Fatalf("input of %d bytes was accepted", len(in))
		}
	}
}

// Forged packets must neither be accepted nor move the state: the real session has to survive.
func TestForgeriesNeverBreakTheSession(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello")
	before := stateBytes(t, b)
	accepted := 0
	for i := 0; i < 400; i++ {
		forged := make([]byte, headerLen+PadBucket+tagLen)
		rand.Read(forged)
		if i%2 == 0 { // a valid-looking fresh public key in the header forces a key step
			k, _ := x25519.GenerateKey(rand.Reader)
			copy(forged, k.PublicKey().Bytes())
		}
		binary.BigEndian.PutUint32(forged[32:], uint32(i%7))
		binary.BigEndian.PutUint32(forged[36:], uint32(i%11))
		if _, err := b.Decrypt(forged, testAD); err == nil {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("%d of 400 forged messages were accepted", accepted)
	}
	if !bytes.Equal(before, stateBytes(t, b)) {
		t.Fatal("forged messages changed the state")
	}
	mustDec(t, b, mustEnc(t, a, "real command after the attack"), "real command after the attack")
}

func TestLowOrderKeyInHeaderIsRejected(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello")
	before := stateBytes(t, b)
	forged := make([]byte, headerLen+PadBucket+tagLen) // all-zero public key: a low-order point
	if _, err := b.Decrypt(forged, testAD); err == nil {
		t.Fatal("a degenerate public key was accepted")
	}
	if !bytes.Equal(before, stateBytes(t, b)) {
		t.Fatal("state changed")
	}
}

func TestSkipLimit(t *testing.T) {
	a, b := pair(t)
	var msgs [][]byte
	for i := 0; i < MaxSkip+3; i++ {
		msgs = append(msgs, mustEnc(t, a, "m"))
	}
	before := stateBytes(t, b)
	if _, err := b.Decrypt(msgs[MaxSkip+1], testAD); !errors.Is(err, ErrTooManySkipped) {
		t.Fatalf("jumping %d ahead: got %v, want ErrTooManySkipped", MaxSkip+1, err)
	}
	if !bytes.Equal(before, stateBytes(t, b)) {
		t.Fatal("a rejected jump changed the state")
	}
	mustDec(t, b, msgs[MaxSkip], "m") // exactly MaxSkip ahead is still allowed
	if b.SkippedKeys() != MaxSkip {
		t.Fatalf("skipped keys = %d, want %d", b.SkippedKeys(), MaxSkip)
	}
}

func TestStoredSkippedKeysAreBounded(t *testing.T) {
	a, b := pair(t)
	for round := 0; round < 8; round++ {
		var last []byte
		for i := 0; i < MaxSkip; i++ {
			last = mustEnc(t, a, "x")
		}
		mustDec(t, b, last, "x") // leaves MaxSkip-1 skipped keys each round
		mustDec(t, a, mustEnc(t, b, "r"), "r")
		if b.SkippedKeys() > MaxStoredSkipped {
			t.Fatalf("stored skipped keys = %d, limit %d", b.SkippedKeys(), MaxStoredSkipped)
		}
	}
}

func TestPaddingHidesLength(t *testing.T) {
	a, _ := pair(t)
	sizes := map[int]bool{}
	for _, n := range []int{0, 1, 10, 100, 900, PadBucket - 2} {
		sizes[len(mustEnc(t, a, string(make([]byte, n))))] = true
	}
	if len(sizes) != 1 {
		t.Fatalf("messages of different short lengths have %d distinct wire sizes, want 1", len(sizes))
	}
	one := len(mustEnc(t, a, "x"))
	if got := len(mustEnc(t, a, string(bytes.Repeat([]byte("y"), PadBucket)))); got != one+PadBucket {
		t.Fatalf("a message one bucket longer has size %d, want %d", got, one+PadBucket)
	}
}

func TestMessageSizeLimit(t *testing.T) {
	a, _ := pair(t)
	if _, err := a.Encrypt(make([]byte, MaxPlaintext+1), testAD); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if _, err := a.Encrypt(make([]byte, MaxPlaintext), testAD); err != nil {
		t.Fatalf("a message of exactly the limit failed: %v", err)
	}
}

func TestZeroBytesInPlaintextSurvivePadding(t *testing.T) {
	a, b := pair(t)
	want := []byte{0, 0, 1, 0, 0x80, 0, 0}
	m, _ := a.Encrypt(want, testAD)
	got, err := b.Decrypt(m, testAD)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
}

// Forward secrecy: a thief who copies the whole current state cannot open what was sent before.
func TestForwardSecrecy(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello")
	mustDec(t, a, mustEnc(t, b, "hi"), "hi")
	old1, old2 := mustEnc(t, a, "secret one"), mustEnc(t, a, "secret two")
	mustDec(t, b, old1, "secret one")
	mustDec(t, b, old2, "secret two")
	thief := snapshot(t, b) // the theft happens after delivery
	if _, err := thief.Decrypt(old1, testAD); err == nil {
		t.Fatal("stolen state opened an earlier message")
	}
	if _, err := thief.Decrypt(old2, testAD); err == nil {
		t.Fatal("stolen state opened an earlier message")
	}
}

// Post-compromise security: the thief reads until the compromised side has replied with a fresh key, then is locked out.
func TestPostCompromiseSecurity(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello")
	mustDec(t, a, mustEnc(t, b, "hi"), "hi")
	mustDec(t, b, mustEnc(t, a, "warm up"), "warm up")
	thief := snapshot(t, b) // b is compromised here

	direct := mustEnc(t, a, "window still open")
	if got, err := snapshot(t, thief).Decrypt(direct, testAD); err != nil || string(got) != "window still open" {
		t.Fatalf("expected the thief to read this one: %v %q", err, got)
	}
	mustDec(t, b, direct, "window still open")

	back := mustEnc(t, b, "round trip") // still uses the stolen ratchet key
	mustDec(t, a, back, "round trip")
	next := mustEnc(t, a, "after one round trip")
	if got, err := snapshot(t, thief).Decrypt(next, testAD); err != nil || string(got) != "after one round trip" {
		t.Fatalf("not yet healed, the thief should still read: %v %q", err, got)
	}
	mustDec(t, b, next, "after one round trip") // b now creates a fresh key the thief never sees

	heal := mustEnc(t, b, "heal")
	mustDec(t, a, heal, "heal")
	after := mustEnc(t, a, "after healing")
	if _, err := snapshot(t, thief).Decrypt(heal, testAD); err == nil {
		t.Fatal("the thief read the healing message")
	}
	if _, err := snapshot(t, thief).Decrypt(after, testAD); err == nil {
		t.Fatal("the thief read a message after the session healed")
	}
	mustDec(t, b, after, "after healing")
}

func TestMarshalRoundTripContinuesTheConversation(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "one"), "one")
	mustDec(t, a, mustEnc(t, b, "two"), "two")
	late := mustEnc(t, a, "late")
	later := mustEnc(t, a, "later")
	mustDec(t, b, later, "later") // leaves one skipped key
	b2 := snapshot(t, b)
	a2 := snapshot(t, a)
	mustDec(t, b2, late, "late") // the skipped key survived serialisation
	mustDec(t, b2, mustEnc(t, a2, "after restore"), "after restore")
	mustDec(t, a2, mustEnc(t, b2, "and back"), "and back")
}

// Restoring an older state than the peer has seen must not silently work.
func TestRestoringAnOldStateBreaksTheSession(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "one"), "one")
	old := snapshot(t, a)
	for i := 0; i < 3; i++ {
		mustDec(t, b, mustEnc(t, a, "advance"), "advance")
	}
	mustDec(t, a, mustEnc(t, b, "reply"), "reply")
	// 'old' would reuse message keys that b already consumed.
	if _, err := b.Decrypt(mustEnc(t, old, "reused key"), testAD); err == nil {
		t.Fatal("a message from a restored old state was accepted")
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "null", "{}", `{"rk":"AAAA"}`, "not json", `{"rk":"` + string(bytes.Repeat([]byte("A"), 43)) + `="}`} {
		if _, err := Unmarshal([]byte(in)); err == nil {
			t.Fatalf("garbage state %q was accepted", in)
		}
	}
}

func TestSessionsAreIndependent(t *testing.T) {
	a1, _ := pair(t)
	_, b2 := pair(t)
	if _, err := b2.Decrypt(mustEnc(t, a1, "cross session"), testAD); err == nil {
		t.Fatal("a message from a different session was accepted")
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello")
	const n = 40
	blobs := make([][]byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := a.Encrypt([]byte("concurrent"), testAD)
			if err != nil {
				t.Error(err)
				return
			}
			blobs[i] = m
		}(i)
	}
	wg.Wait()
	sort.Slice(blobs, func(i, j int) bool {
		return binary.BigEndian.Uint32(blobs[i][36:]) < binary.BigEndian.Uint32(blobs[j][36:])
	})
	for _, m := range blobs {
		mustDec(t, b, m, "concurrent")
	}
	// decrypting concurrently with encrypting on the same object must not race
	var wg2 sync.WaitGroup
	wg2.Add(2)
	go func() {
		defer wg2.Done()
		for i := 0; i < 50; i++ {
			b.Encrypt([]byte("x"), testAD)
		}
	}()
	go func() {
		defer wg2.Done()
		for i := 0; i < 50; i++ {
			b.Decrypt(make([]byte, headerLen+PadBucket+tagLen), testAD)
		}
	}()
	wg2.Wait()
}

func FuzzDecrypt(f *testing.F) {
	a, b := pair(f)
	good, _ := a.Encrypt([]byte("seed"), testAD)
	f.Add(good)
	f.Add([]byte{})
	f.Add(make([]byte, headerLen+PadBucket+tagLen))
	f.Fuzz(func(t *testing.T, in []byte) {
		before, _ := b.Marshal()
		if _, err := b.Decrypt(in, testAD); err == nil && !bytes.Equal(in, good) {
			t.Fatalf("fuzzed input accepted: %x", in[:min(len(in), 16)])
		}
		after, _ := b.Marshal()
		if !bytes.Equal(before, after) && !bytes.Equal(in, good) {
			t.Fatal("a rejected input changed the state")
		}
	})
}
