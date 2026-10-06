package approval

import (
	"errors"
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors for HMAC-SHA1 (the 8-digit codes, reduced to 6 digits here).
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, c := range []struct {
		unix int64
		want string // last six digits of the published 8-digit code
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		if got := Code(secret, time.Unix(c.unix, 0)); got != c.want {
			t.Errorf("t=%d: got %s, want %s", c.unix, got, c.want)
		}
	}
}

func fixed() time.Time { return time.Unix(1_800_000_000, 0) }

func newV(t *testing.T) (*Verifier, []byte) {
	t.Helper()
	s, _, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	return NewVerifier(s), s
}

func TestVerifyAcceptsCurrentAndAdjacentSteps(t *testing.T) {
	v, s := newV(t)
	now := fixed()
	if err := v.Verify(Code(s, now), now); err != nil {
		t.Fatal(err)
	}
	v2, _ := newV(t)
	v2.secret = s
	if err := v2.Verify(Code(s, now.Add(-Period)), now); err != nil {
		t.Fatalf("previous step: %v", err)
	}
	v3, _ := newV(t)
	v3.secret = s
	if err := v3.Verify(Code(s, now.Add(Period)), now); err != nil {
		t.Fatalf("next step: %v", err)
	}
	v4, _ := newV(t)
	v4.secret = s
	if err := v4.Verify(Code(s, now.Add(3*Period)), now); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a code three steps away: %v", err)
	}
}

func TestACodeWorksOnlyOnce(t *testing.T) {
	v, s := newV(t)
	now := fixed()
	code := Code(s, now)
	if err := v.Verify(code, now); err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(code, now.Add(time.Second)); !errors.Is(err, ErrReused) {
		t.Fatalf("got %v, want ErrReused", err)
	}
	// an older step than the last accepted one is also refused
	if err := v.Verify(Code(s, now.Add(-Period)), now); !errors.Is(err, ErrReused) {
		t.Fatalf("an older code after a newer one: %v", err)
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	v, s := newV(t)
	now := fixed()
	for i := 0; i < 4; i++ {
		if err := v.Verify("000000", now); !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := v.Verify("000000", now); !errors.Is(err, ErrLocked) {
		t.Fatalf("5th failure should lock: %v", err)
	}
	if err := v.Verify(Code(s, now), now.Add(time.Minute)); !errors.Is(err, ErrLocked) {
		t.Fatalf("even the right code must be refused while locked: %v", err)
	}
	if err := v.Verify(Code(s, now.Add(6*time.Minute)), now.Add(6*time.Minute)); err != nil {
		t.Fatalf("after the lockout: %v", err)
	}
}

func TestSuccessResetsTheFailureCount(t *testing.T) {
	v, s := newV(t)
	now := fixed()
	for i := 0; i < 4; i++ {
		v.Verify("111111", now)
	}
	if err := v.Verify(Code(s, now), now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := v.Verify("111111", now.Add(time.Duration(i+1)*Period)); errors.Is(err, ErrLocked) {
			t.Fatal("failures were not reset by a success")
		}
	}
}

func TestMalformedCodes(t *testing.T) {
	v, _ := newV(t)
	for _, c := range []string{"", "12345", "1234567", "abcdef", "12 34", "１２３４５６"} {
		if err := v.Verify(c, fixed()); err == nil {
			t.Errorf("%q accepted", c)
		}
		v.fails = 0
	}
}

func TestSpacesInACodeAreIgnored(t *testing.T) {
	v, s := newV(t)
	code := Code(s, fixed())
	if err := v.Verify(code[:3]+" "+code[3:], fixed()); err != nil {
		t.Fatal(err)
	}
}

func TestSecretEncoding(t *testing.T) {
	s, b32, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeSecret(b32)
	if err != nil || string(back) != string(s) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := DecodeSecret("not base32!!"); err == nil {
		t.Fatal("bad secret accepted")
	}
	if _, err := DecodeSecret("AAAA"); err == nil {
		t.Fatal("a too short secret was accepted")
	}
}

func TestGateBindsApprovalToTheCommand(t *testing.T) {
	v, s := newV(t)
	g := NewGate(v)
	now := fixed()
	ch := g.Issue("hash-of-command-A", "edit file", now)
	hash, err := g.Approve(ch.ID, Code(s, now), now)
	if err != nil || hash != "hash-of-command-A" {
		t.Fatalf("%q %v", hash, err)
	}
	if err := Check(hash, "hash-of-command-B"); !errors.Is(err, ErrNotForYou) {
		t.Fatalf("approval for A was usable for B: %v", err)
	}
	if err := Check(hash, "hash-of-command-A"); err != nil {
		t.Fatal(err)
	}
}

func TestGateChallengeIsSingleUseAndExpires(t *testing.T) {
	v, s := newV(t)
	g := NewGate(v)
	now := fixed()
	ch := g.Issue("h", "x", now)
	if _, err := g.Approve(ch.ID, Code(s, now), now); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Approve(ch.ID, Code(s, now.Add(Period)), now.Add(Period)); !errors.Is(err, ErrNoSuch) {
		t.Fatalf("a challenge was approved twice: %v", err)
	}
	ch2 := g.Issue("h2", "y", now)
	if _, err := g.Approve(ch2.ID, Code(s, now.Add(2*time.Minute)), now.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v, want ErrExpired", err)
	}
	if _, err := g.Approve("unknown", "123456", now); !errors.Is(err, ErrNoSuch) {
		t.Fatalf("got %v", err)
	}
}

func TestWrongCodeDoesNotConsumeTheChallenge(t *testing.T) {
	v, s := newV(t)
	g := NewGate(v)
	now := fixed()
	ch := g.Issue("h", "x", now)
	if _, err := g.Approve(ch.ID, "000000", now); !errors.Is(err, ErrBadCode) {
		t.Fatalf("got %v", err)
	}
	if _, err := g.Approve(ch.ID, Code(s, now), now); err != nil {
		t.Fatalf("the challenge was lost after one wrong code: %v", err)
	}
}

func TestGateDropsExpiredChallengesWhenIssuing(t *testing.T) {
	v, _ := newV(t)
	g := NewGate(v)
	now := fixed()
	for i := 0; i < 100; i++ {
		g.Issue("h", "x", now)
	}
	g.Issue("h", "x", now.Add(10*time.Minute))
	if len(g.pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(g.pending))
	}
}
