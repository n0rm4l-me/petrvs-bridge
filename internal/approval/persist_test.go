package approval

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stateFile(t *testing.T) string { return filepath.Join(t.TempDir(), "code-state") }

func TestAUsedCodeStaysUsedAfterARestart(t *testing.T) {
	path := stateFile(t)
	v, secret := newV(t)
	if err := v.Persist(path, fixed()); err != nil {
		t.Fatal(err)
	}
	code := Code(secret, fixed())
	if err := v.Verify(code, fixed()); err != nil {
		t.Fatal(err)
	}
	v2 := NewVerifier(secret) // the agent restarted
	if err := v2.Persist(path, fixed().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := v2.Verify(code, fixed().Add(time.Second)); !errors.Is(err, ErrReused) {
		t.Fatalf("a used code worked again after a restart: %v", err)
	}
	next := fixed().Add(2 * Period)
	if err := v2.Verify(Code(secret, next), next); err != nil {
		t.Fatalf("a fresh code was refused: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", st, err)
	}
}

func TestALockoutSurvivesARestart(t *testing.T) {
	path := stateFile(t)
	v, secret := newV(t)
	v.Persist(path, fixed())
	for i := 0; i < v.MaxFails; i++ {
		v.Verify("000000", fixed())
	}
	v2 := NewVerifier(secret)
	v2.Persist(path, fixed())
	if err := v2.Verify(Code(secret, fixed()), fixed().Add(time.Minute)); !errors.Is(err, ErrLocked) {
		t.Fatalf("a restart cleared the lockout: %v", err)
	}
	later := fixed().Add(6 * time.Minute)
	if err := v2.Verify(Code(secret, later), later); err != nil {
		t.Fatalf("still locked after the lockout ended: %v", err)
	}
}

func TestAnUnreadableRecordRefusesRecentCodesInsteadOfForgetting(t *testing.T) {
	path := stateFile(t)
	os.WriteFile(path, []byte("not json"), 0o600)
	v, secret := newV(t)
	if err := v.Persist(path, fixed()); err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(Code(secret, fixed()), fixed()); !errors.Is(err, ErrReused) {
		t.Fatalf("a code of the current period was accepted after the record was lost: %v", err)
	}
	later := fixed().Add(3 * Period)
	if err := v.Verify(Code(secret, later), later); err != nil {
		t.Fatalf("the verifier never recovered: %v", err)
	}
}

func TestACodeWhoseUseCannotBeRecordedIsRefused(t *testing.T) {
	dir := t.TempDir()
	v, secret := newV(t)
	v.Persist(filepath.Join(dir, "gone", "state"), fixed()) // the directory does not exist
	if err := v.Verify(Code(secret, fixed()), fixed()); !errors.Is(err, ErrState) {
		t.Fatalf("got %v: a code that could be replayed after a restart was accepted", err)
	}
	os.Mkdir(filepath.Join(dir, "gone"), 0o700) // once it can be recorded, the same code works
	if err := v.Verify(Code(secret, fixed()), fixed()); err != nil {
		t.Fatal(err)
	}
}
