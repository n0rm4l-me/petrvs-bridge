package filetx

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

var t0 = time.Unix(1_800_000_000, 0)

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestChunksCutAtTheMessageLimitAndJoinBack(t *testing.T) {
	for _, n := range []int{1, wire.MaxChunk - 1, wire.MaxChunk, wire.MaxChunk + 1, 5*wire.MaxChunk + 7} {
		data := randomBytes(n)
		cs := Chunks(data)
		if len(cs) != (n+wire.MaxChunk-1)/wire.MaxChunk {
			t.Errorf("%d bytes: %d chunks", n, len(cs))
		}
		var joined []byte
		for _, c := range cs {
			if len(c) == 0 || len(c) > wire.MaxChunk {
				t.Fatalf("a chunk of %d bytes", len(c))
			}
			joined = append(joined, c...)
		}
		if !bytes.Equal(joined, data) {
			t.Errorf("%d bytes do not come back", n)
		}
	}
	if len(Chunks(nil)) != 0 {
		t.Error("nothing gave a chunk")
	}
}

// An encoded chunk has to fit in the body of a message.
func TestAnEncodedChunkFitsInTheBodyOfAMessage(t *testing.T) {
	body := EncodePart(randomBytes(wire.MaxChunk))
	if len(body) > wire.MaxBody {
		t.Fatalf("%d bytes of body for a full chunk, the limit is %d", len(body), wire.MaxBody)
	}
	if _, err := DecodePart(body); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePart(EncodePart(randomBytes(wire.MaxChunk + 1))); err == nil {
		t.Error("a chunk over the limit was decoded")
	}
	if _, err := DecodePart("not base64 !!"); err == nil {
		t.Error("garbage was decoded")
	}
}

func TestSafeNameKeepsOnlyAHarmlessLastElement(t *testing.T) {
	for in, want := range map[string]string{
		"photo.png":                       "photo.png",
		"/etc/passwd":                     "passwd",
		"../../../etc/shadow":             "shadow",
		`C:\Users\me\report (1).pdf`:      "report (1).pdf",
		".bashrc":                         "bashrc",
		"...":                             "file",
		"":                                "file",
		"a\x00b\nc.txt":                   "a_b_c.txt",
		"скриншот 2026.png":               "скриншот 2026.png",
		"evil;rm -rf *.sh":                "evil_rm -rf _.sh",
		"name with trailing space  ":      "name with trailing space",
		"$(touch x).txt":                  "_(touch x).txt",
		strings.Repeat("ж", 80):           strings.Repeat("ж", 50),
		strings.Repeat("a", 200) + ".pdf": strings.Repeat("a", 96) + ".pdf",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"a/b", "a\\b", "x\ny", strings.Repeat("я", 300)} {
		got := SafeName(in)
		if strings.ContainsAny(got, "/\\\n") || len(got) > maxNameBytes || got == "" {
			t.Errorf("SafeName(%q) = %q", in, got)
		}
	}
}

func TestKindIsPhotoOnlyForPicturesTelegramCanShow(t *testing.T) {
	for _, c := range []struct {
		name string
		size int64
		want string
	}{
		{"shot.png", 1 << 20, "photo"}, {"a.JPG", 100, "photo"}, {"a.jpeg", 100, "photo"}, {"a.webp", 100, "photo"},
		{"big.png", PhotoMaxBytes + 1, "document"}, {"anim.gif", 100, "document"}, {"doc.pdf", 100, "document"},
		{"noext", 100, "document"}, {"zero.png", 0, "document"},
	} {
		if got := Kind(c.name, c.size); got != c.want {
			t.Errorf("Kind(%q, %d) = %q, want %q", c.name, c.size, got, c.want)
		}
	}
}

func TestSecretsAndTheirPlacesAreRefused(t *testing.T) {
	for _, p := range []string{
		"/home/me/.env", "/home/me/app/.env.production", "/home/me/server.pem", "/home/me/x/private.KEY", "/home/me/cert.p12",
		"/home/me/.ssh/config", "/home/me/.ssh/id_ed25519", "/home/me/id_rsa.pub.bak", "/home/me/.aws/credentials",
		"/home/me/.config/gcloud/application_default_credentials.json", "/home/me/credentials.csv", "/home/me/my-secrets.txt",
		"/home/me/laptop.identity.json", "/home/me/.netrc", "/home/me/.docker/config.json", "/Users/me/Library/Keychains/login.keychain-db",
		"/tmp/service-account-prod.json", "/home/me/api.token", "/home/me/.kube/config",
	} {
		if err := Denied(p); err == nil {
			t.Errorf("%s was allowed", p)
		}
	}
	for _, p := range []string{
		"/home/me/screenshot.png", "/home/me/report.pdf", "/tmp/output.log", "/home/me/project/main.go", "/home/me/notes/keyboard.md",
		"/home/me/monkey.png", "/home/me/tokenizer.go",
	} {
		if err := Denied(p); err != nil {
			t.Errorf("%s was refused: %v", p, err)
		}
	}
}

func TestTheBridgesOwnDirectoriesAreRefused(t *testing.T) {
	if err := Denied("/home/me/.local/state/petrvs-bridge/spool/x.json", "/home/me/.local/state/petrvs-bridge"); err == nil {
		t.Error("a file in the state directory was allowed")
	}
	if err := Denied("/home/me/.local/state/petrvs-bridge-other/x.png", "/home/me/.local/state/petrvs-bridge"); err != nil {
		t.Errorf("a neighbour with the same prefix was refused: %v", err)
	}
	if err := Denied("/home/me/x.png", ""); err != nil {
		t.Errorf("an empty extra directory refused a file: %v", err)
	}
}

// APFS and NTFS treat names that differ in case as one name, and Unicode case folding goes beyond ToLower: the long s
// (U+017F) is the letter s and the Kelvin sign (U+212A) is the letter k. Each of these paths is the secret.
func TestNamesThatAFileSystemFoldsToASecretAreRefused(t *testing.T) {
	for _, p := range []string{
		"/home/me/.awſ/cli/cache/x.json", "/home/me/.AWS/credentials.csv.png", "/home/me/.Ssh/known.png", "/home/me/.ſsh/x.png",
		"/home/me/private.Key", "/home/me/PRIVATE.KEY", "/home/me/My.PEM", "/home/me/.ENV", "/home/me/ID_RSA.png",
		"/home/me/my-ſecret.png", "/home/me/Credentials.PNG", "/home/me/.Git-Credentials", "/home/me/x/GCLOUD/a.png",
		"/home/me/laptop.Identity.JSON", "/home/me/login.KeyChain-db", "/home/me/.Kube/config.png",
	} {
		if err := Denied(p); err == nil {
			t.Errorf("%s was allowed", p)
		}
	}
	// the state directory and the alarm secret are refused under any case too
	state := "/home/me/.local/state/petrvs-bridge"
	for _, p := range []string{
		state + "/x", "/home/me/.local/state/PETRVS-BRIDGE/spool/a.json", "/home/me/.local/STATE/petrvs-bridge/alarm.txt",
		"/home/me/.local/state/petrvs-bridge/totp.txt", "/home/me/.local/state/petrvſ-bridge/totp.txt",
	} {
		if err := Denied(p, state); err == nil {
			t.Errorf("%s was allowed", p)
		}
	}
	if err := Denied("/root/STATE/totp.txt", "/root/state"); err == nil {
		t.Error("STATE was not the state directory")
	}
	if err := Denied("/root/State/totp.txt", "/root/state"); err == nil {
		t.Error("State was not the state directory")
	}
	if err := Denied("/root/alarm.txt", "/ROOT/Alarm.TXT"); err == nil {
		t.Error("an own file named in another case was allowed")
	}
	// folding must not refuse what is only similar
	for _, p := range []string{"/home/me/.awsome/x.png", "/home/me/ſtatus.png", "/home/me/Monkey.PNG", "/home/me/Keyboard.MD"} {
		if err := Denied(p); err != nil {
			t.Errorf("%s was refused: %v", p, err)
		}
	}
	if err := Denied("/home/me/.local/state/PETRVS-BRIDGE-other/x.png", state); err != nil {
		t.Errorf("a neighbour with the same prefix was refused: %v", err)
	}
}

func TestFoldSendsEveryMemberOfACaseOrbitToTheSameRune(t *testing.T) {
	for _, orbit := range []string{"sSſ", "kKK", "aA", "éÉ", "σςΣ", "θϑϴΘ", "µμΜ"} {
		rs := []rune(orbit)
		for _, r := range rs {
			if Fold(string(r)) != Fold(string(rs[0])) {
				t.Errorf("%q folds to %q, %q folds to %q", r, Fold(string(r)), rs[0], Fold(string(rs[0])))
			}
		}
	}
	// nothing but letters change
	if got := Fold("a*b?[c]/d.e-f_1"); got != Fold("A*B?[C]/D.E-F_1") || !strings.ContainsAny(got, "*?[]/.-_1") {
		t.Errorf("%q", got)
	}
}

func parts(t *testing.T, a *Assembler, id string, data []byte) (n int) {
	t.Helper()
	for i, c := range Chunks(data) {
		if err := a.AddPart(id, i, c, t0); err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		n++
	}
	return n
}

func TestAPutTogetherFileComesBackWhole(t *testing.T) {
	a := NewAssembler(DefaultMaxBytes)
	data := randomBytes(3*wire.MaxChunk + 123)
	n := parts(t, a, "abc", data)
	got, err := a.Complete("abc", n, int64(len(data)), SHA(data), t0)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("err %v, equal %v", err, bytes.Equal(got, data))
	}
	if tr, by := a.Held(); tr != 0 || by != 0 {
		t.Errorf("still holding %d transfers, %d bytes", tr, by)
	}
	if _, err := a.Complete("abc", n, int64(len(data)), SHA(data), t0); !errors.Is(err, ErrUnknown) {
		t.Errorf("a second Complete: %v", err)
	}
}

func TestPartsMayArriveInAnyOrderAndTwice(t *testing.T) {
	a := NewAssembler(DefaultMaxBytes)
	data := randomBytes(4 * wire.MaxChunk)
	cs := Chunks(data)
	for _, i := range []int{2, 0, 3, 3, 1, 0} {
		if err := a.AddPart("x", i, cs[i], t0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := a.Complete("x", 4, int64(len(data)), SHA(data), t0)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%v", err)
	}
}

func TestMissingPartsAreReportedAndLateOnesStillCount(t *testing.T) {
	a := NewAssembler(DefaultMaxBytes)
	data := randomBytes(3 * wire.MaxChunk)
	cs := Chunks(data)
	a.AddPart("x", 0, cs[0], t0)
	a.AddPart("x", 2, cs[2], t0)
	var me *MissingError
	if _, err := a.Complete("x", 3, int64(len(data)), SHA(data), t0); !errors.As(err, &me) || me.Have != 2 || me.Want != 3 {
		t.Fatalf("got %v", err)
	}
	a.AddPart("x", 1, cs[1], t0) // arrives after the file item
	if got, err := a.Complete("x", 3, int64(len(data)), SHA(data), t0); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("after the late part: %v", err)
	}
}

func TestAWrongSizeOrHashIsRefusedAndForgotten(t *testing.T) {
	data := randomBytes(wire.MaxChunk + 5)
	for name, c := range map[string]struct {
		size int64
		sha  string
		want error
	}{
		"size":    {int64(len(data)) + 1, SHA(data), ErrSize},
		"hash":    {int64(len(data)), SHA([]byte("other")), ErrHash},
		"too big": {DefaultMaxBytes + 1, SHA(data), ErrTooLarge},
	} {
		a := NewAssembler(DefaultMaxBytes)
		n := parts(t, a, "x", data)
		if _, err := a.Complete("x", n, c.size, c.sha, t0); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
		if tr, _ := a.Held(); tr != 0 {
			t.Errorf("%s: the transfer was kept", name)
		}
	}
}

func TestAStrangerCannotMakeTheReceiverHoldMoreThanTheBounds(t *testing.T) {
	a := NewAssembler(300 << 10) // 300 KiB files
	chunk := randomBytes(wire.MaxChunk)
	// one transfer cannot pass the file limit
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		err = a.AddPart("one", i, chunk, t0)
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("the file limit: %v", err)
	}
	// all transfers together stay under twice the limit
	a = NewAssembler(300 << 10)
	var refused bool
	for i := 0; i < maxTransfers; i++ {
		for j := 0; j < 3; j++ {
			if err := a.AddPart(string(rune('a'+i)), j, chunk, t0); err != nil {
				refused = true
			}
		}
	}
	if _, by := a.Held(); !refused || by > 2*(300<<10) {
		t.Errorf("holding %d bytes, refused %v", by, refused)
	}
	// and only so many at once
	a = NewAssembler(DefaultMaxBytes)
	for i := 0; i < maxTransfers; i++ {
		if err := a.AddPart(string(rune('a'+i)), 0, []byte("x"), t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.AddPart("another", 0, []byte("x"), t0); err == nil {
		t.Error("a fifth transfer was accepted")
	}
	// bad input
	for name, f := range map[string]func() error{
		"empty id":    func() error { return a.AddPart("", 0, []byte("x"), t0) },
		"long id":     func() error { return a.AddPart(strings.Repeat("i", 33), 0, []byte("x"), t0) },
		"negative":    func() error { return a.AddPart("a", -1, []byte("x"), t0) },
		"far index":   func() error { return a.AddPart("a", 1<<20, []byte("x"), t0) },
		"empty part":  func() error { return a.AddPart("a", 1, nil, t0) },
		"huge part":   func() error { return a.AddPart("a", 1, make([]byte, wire.MaxChunk+1), t0) },
		"changed one": func() error { a.AddPart("a", 5, []byte("one"), t0); return a.AddPart("a", 5, []byte("two"), t0) },
	} {
		if f() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAnUnfinishedTransferIsForgottenAfterTenMinutes(t *testing.T) {
	a := NewAssembler(DefaultMaxBytes)
	a.AddPart("old", 0, randomBytes(1000), t0)
	a.AddPart("new", 0, randomBytes(1000), t0.Add(11*time.Minute)) // any later call sweeps
	if tr, _ := a.Held(); tr != 1 {
		t.Fatalf("holding %d transfers", tr)
	}
	if _, err := a.Complete("old", 1, 1000, "", t0.Add(11*time.Minute)); !errors.Is(err, ErrUnknown) {
		t.Errorf("the old one: %v", err)
	}
}

func TestNewIDsAreShortAndDifferent(t *testing.T) {
	a, b := NewID(), NewID()
	if len(a) != 12 || a == b {
		t.Errorf("%q %q", a, b)
	}
}
