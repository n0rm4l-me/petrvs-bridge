package ratchet

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// forged builds a packet with a fresh ratchet key in its header, as anyone who can publish to the bus could.
func forged(t testing.TB) []byte {
	t.Helper()
	k, err := x25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, headerLen+tagLen+PadBucket)
	rand.Read(blob)
	copy(blob, k.PublicKey().Bytes())
	for i := keyLen; i < headerLen; i++ { // previous chain length 0, message number 0
		blob[i] = 0
	}
	return blob
}

func TestAFloodOfFreshKeysIsCutOffBeforeItCostsAnything(t *testing.T) {
	a, b := pair(t)
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	b.SetDHStepLimit(5, 1, clk.now)
	mustDec(t, b, mustEnc(t, a, "hello"), "hello") // the real first message is one step

	auth, limited := 0, 0
	for i := 0; i < 200; i++ {
		_, err := b.Decrypt(forged(t), testAD)
		switch {
		case errors.Is(err, ErrAuth):
			auth++
		case errors.Is(err, ErrRateLimited):
			limited++
		default:
			t.Fatalf("unexpected %v", err)
		}
	}
	if auth > 5 || limited < 195 {
		t.Fatalf("%d forged packets reached the key agreement, %d were cut off", auth, limited)
	}
	// the real conversation on the current key needs no new step, so the flood does not touch it
	mustDec(t, b, mustEnc(t, a, "still here"), "still here")
}

func TestARealKeyStepWaitsForTheBudgetAndThenWorks(t *testing.T) {
	a, b := pair(t)
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	a.SetDHStepLimit(2, 1, clk.now)
	mustDec(t, b, mustEnc(t, a, "q"), "q")
	reply := mustEnc(t, b, "a") // b's reply carries a new key: a has to take a step to read it
	for i := 0; i < 50; i++ {
		a.Decrypt(forged(t), testAD)
	}
	if _, err := a.Decrypt(reply, testAD); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want the step to be refused while the budget is empty", err)
	}
	clk.t = clk.t.Add(10 * time.Second)
	mustDec(t, a, reply, "a") // refused before any state change, so the same packet works once the budget is back
}

func TestANormalConversationNeverHitsTheLimit(t *testing.T) {
	a, b := pair(t) // default limits, real clock
	for i := 0; i < DHStepBurst-2; i++ {
		mustDec(t, b, mustEnc(t, a, "ping"), "ping")
		mustDec(t, a, mustEnc(t, b, "pong"), "pong")
	}
}

func TestBudgetSurvivesMarshal(t *testing.T) {
	a, b := pair(t)
	mustDec(t, b, mustEnc(t, a, "x"), "x")
	c := snapshot(t, b)
	for i := 0; i < DHStepBurst+5; i++ {
		c.Decrypt(forged(t), testAD)
	}
	if _, err := c.Decrypt(forged(t), testAD); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a restored ratchet has no limiter: %v", err)
	}
}
