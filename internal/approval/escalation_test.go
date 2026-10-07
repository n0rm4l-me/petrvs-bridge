package approval

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// wrongCode is a code that is certainly not one of the three the verifier accepts at that time.
func wrongCode(secret []byte, now time.Time) string {
	cur := step(now)
	for n := 0; ; n++ {
		c := fmt.Sprintf("%06d", n)
		if c != codeForStep(secret, cur-1, Digits) && c != codeForStep(secret, cur, Digits) && c != codeForStep(secret, cur+1, Digits) {
			return c
		}
	}
}

// failUntilLocked makes MaxFails wrong attempts at the given time and returns how long the resulting lockout is.
func failUntilLocked(t *testing.T, v *Verifier, secret []byte, now time.Time) time.Duration {
	t.Helper()
	for i := 1; i <= v.MaxFails; i++ {
		err := v.Verify(wrongCode(secret, now), now)
		want := ErrBadCode
		if i == v.MaxFails {
			want = ErrLocked
		}
		if !errors.Is(err, want) {
			t.Fatalf("attempt %d at %v: got %v, want %v", i, now, err, want)
		}
	}
	return v.lockedUntil.Sub(now)
}

// Five wrong codes every five minutes is 1440 guesses a day; each lockout that follows another without a success must
// last twice as long, up to a day.
func TestEachConsecutiveLockoutIsTwiceAsLong(t *testing.T) {
	v, secret := newV(t)
	now := fixed()
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute,
		320 * time.Minute, 640 * time.Minute, 1280 * time.Minute, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, d := range want {
		got := failUntilLocked(t, v, secret, now)
		if got != d {
			t.Fatalf("lockout %d lasts %v, want %v", i+1, got, d)
		}
		// Right up to the end of the lockout even the right code is refused, and a wrong one is not counted.
		almost := now.Add(d - time.Second)
		if err := v.Verify(Code(secret, almost), almost); !errors.Is(err, ErrLocked) {
			t.Fatalf("lockout %d: the right code was taken a second before the end: %v", i+1, err)
		}
		now = now.Add(d + time.Second)
	}
}

func TestASuccessfulCodeStartsTheLockoutsOverAtTheBase(t *testing.T) {
	v, secret := newV(t)
	now := fixed()
	for _, d := range []time.Duration{5 * time.Minute, 10 * time.Minute} {
		if got := failUntilLocked(t, v, secret, now); got != d {
			t.Fatalf("lockout lasts %v, want %v", got, d)
		}
		now = now.Add(d + time.Second)
	}
	if err := v.Verify(Code(secret, now), now); err != nil {
		t.Fatalf("the right code after the lockout: %v", err)
	}
	now = now.Add(2 * Period)
	if got := failUntilLocked(t, v, secret, now); got != 5*time.Minute {
		t.Fatalf("after a success the next lockout lasts %v, want 5m0s", got)
	}
}

func TestTheLockoutLevelSurvivesARestartAndAFileWithoutOneIsLevelZero(t *testing.T) {
	path := stateFile(t)
	v, secret := newV(t)
	if err := v.Persist(path, fixed()); err != nil {
		t.Fatal(err)
	}
	now := fixed()
	for _, d := range []time.Duration{5 * time.Minute, 10 * time.Minute} {
		if got := failUntilLocked(t, v, secret, now); got != d {
			t.Fatalf("lockout lasts %v, want %v", got, d)
		}
		now = now.Add(d + time.Second)
	}
	v2 := NewVerifier(secret) // the agent restarted
	if err := v2.Persist(path, now); err != nil {
		t.Fatal(err)
	}
	if got := failUntilLocked(t, v2, secret, now); got != 20*time.Minute {
		t.Fatalf("after a restart the third lockout lasts %v, want 20m0s", got)
	}

	// A file written before the level existed has no such field: it loads as level 0.
	old := stateFile(t)
	os.WriteFile(old, []byte(`{"last_step":5,"locked_until":1}`), 0o600)
	v3 := NewVerifier(secret)
	if err := v3.Persist(old, now); err != nil {
		t.Fatal(err)
	}
	if got := failUntilLocked(t, v3, secret, now); got != 5*time.Minute {
		t.Fatalf("a file without a level gives a first lockout of %v, want 5m0s", got)
	}
}
