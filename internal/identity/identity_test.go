package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"hub", "work-laptop", "a", "mac_mini2", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Hub", "-x", "a b", "a/b", "../x", "x\n", strings.Repeat("a", 33), "имя"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestGenerateSaveLoadRoundTrip(t *testing.T) {
	id, err := Generate("laptop")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "laptop.identity.json")
	if err := id.Save(path); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("private identity saved with mode %v, want 0600", st.Mode().Perm())
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "laptop" || string(back.Static.Bytes()) != string(id.Static.Bytes()) || string(back.Sign) != string(id.Sign) {
		t.Fatal("identity did not survive a save and load")
	}
	msg := []byte("signed")
	if !ed25519.Verify(id.Public().Sign, msg, ed25519.Sign(back.Sign, msg)) {
		t.Fatal("restored signing key does not match the public bundle")
	}
}

func TestLoadRefusesWorldReadableFile(t *testing.T) {
	id, _ := Generate("hub")
	path := filepath.Join(t.TempDir(), "hub.json")
	id.Save(path)
	os.Chmod(path, 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("a world-readable private identity was loaded")
	}
	os.Chmod(path, 0o640)
	if _, err := Load(path); err == nil {
		t.Fatal("a group-readable private identity was loaded")
	}
}

func TestLoadRejectsBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"empty": "", "garbage": "not json", "no-keys": `{"name":"hub"}`, "bad-name": `{"name":"Bad Name","static_private":"AA==","sign_private":"AA=="}`,
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: a broken identity file was loaded", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was loaded")
	}
}

func TestPublicBundleRoundTripAndValidation(t *testing.T) {
	id, _ := Generate("agent")
	path := filepath.Join(t.TempDir(), "agent.pub.json")
	if err := SavePublic(path, id.Public()); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPublic(path)
	if err != nil || got.Name != "agent" {
		t.Fatalf("LoadPublic: %v %+v", err, got)
	}
	bad := id.Public()
	bad.Static = bad.Static[:16]
	if bad.Validate() == nil {
		t.Error("a short static key was accepted")
	}
	bad = id.Public()
	bad.Sign = nil
	if bad.Validate() == nil {
		t.Error("a missing signing key was accepted")
	}
	bad = id.Public()
	bad.Name = "Not Valid"
	if bad.Validate() == nil {
		t.Error("an invalid name was accepted")
	}
}

func TestFingerprint(t *testing.T) {
	a, _ := Generate("agent")
	b, _ := Generate("agent")
	fa, fb := a.Public().Fingerprint(), b.Public().Fingerprint()
	if fa == fb {
		t.Fatal("two different identities share a fingerprint")
	}
	if fa != a.Public().Fingerprint() {
		t.Fatal("fingerprint is not stable")
	}
	if len(strings.Fields(fa)) != 10 || len(strings.ReplaceAll(fa, " ", "")) != 40 {
		t.Fatalf("unexpected fingerprint shape %q", fa)
	}
	renamed := a.Public()
	renamed.Name = "other"
	if renamed.Fingerprint() == fa {
		t.Fatal("the fingerprint does not cover the name")
	}
}

func TestGenerateRejectsBadName(t *testing.T) {
	if _, err := Generate("Bad Name"); err == nil {
		t.Fatal("an invalid name was accepted")
	}
}
