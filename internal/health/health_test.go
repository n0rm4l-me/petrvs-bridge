package health

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAFreshBeaconPassesBothChecks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "h")
	b, err := NewBeacon(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Alive(); err != nil {
		t.Fatal(err)
	}
	if err := b.Ready(true); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{Live, Ready} {
		if err := Check(dir, k, 30*time.Second, time.Now()); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestNothingIsLiveOrReadyBeforeTheFirstBeat(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{Live, Ready} {
		err := Check(dir, k, time.Hour, time.Now())
		if err == nil || !strings.Contains(err.Error(), "no "+k+" file") {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestAFileThatIsTooOldFails(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewBeacon(dir)
	if err := b.Alive(); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(90 * time.Second)
	err := Check(dir, Live, 30*time.Second, later)
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("%v", err)
	}
	if err := Check(dir, Live, 2*time.Minute, later); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
}

func TestNotReadyRemovesTheFileAtOnce(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewBeacon(dir)
	_ = b.Ready(true)
	if err := Check(dir, Ready, time.Minute, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.Ready(false); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir, Ready, time.Minute, time.Now()); err == nil {
		t.Fatal("still ready right after Ready(false)")
	}
	if err := b.Ready(false); err != nil {
		t.Fatalf("not ready twice: %v", err)
	}
}

func TestOnlyTheTwoKindsAreKnownAndThePathIsAbsolute(t *testing.T) {
	if err := Check(t.TempDir(), "started", time.Minute, time.Now()); err == nil {
		t.Fatal("an unknown kind passed")
	}
	if _, err := NewBeacon("relative/dir"); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func TestTheFilesAreNotReadableByOthers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "h")
	b, _ := NewBeacon(dir)
	_ = b.Alive()
	for _, p := range []string{dir, filepath.Join(dir, Live)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is %v", p, fi.Mode().Perm())
		}
	}
}
