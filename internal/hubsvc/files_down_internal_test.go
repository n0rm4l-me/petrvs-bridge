package hubsvc

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

func TestAPhotoIsNamedByTheTimeItWasSent(t *testing.T) {
	at := time.Date(2026, 10, 7, 11, 59, 58, 0, time.FixedZone("x", 9*3600))
	if got := fileNameFor(telegram.FileRef{Photo: true, Name: "../../ignored"}, at); got != "photo-20261007-025958.jpg" {
		t.Fatal(got)
	}
}

func TestADocumentIsNamedSafelyWhateverItIsCalled(t *testing.T) {
	at := time.Now()
	for in, want := range map[string]string{
		"report.pdf":             "report.pdf",
		"../../x":                "x",
		"..\\..\\boot.ini":       "boot.ini",
		"/etc/passwd":            "passwd",
		"a\x00b\nc\x1b[0m.txt":   "a_b_c__0m.txt",
		".ssh":                   "ssh",
		"":                       "file",
		" ":                      "file",
		strings.Repeat("ö", 200): strings.Repeat("ö", 50),
		"photo.jpg\u202egnp.exe": "photo.jpg_gnp.exe",
	} {
		if got := fileNameFor(telegram.FileRef{Name: in}, at); got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
}

func TestADocumentWithoutANameGetsAnExtensionFromItsType(t *testing.T) {
	at := time.Now()
	for mime, want := range map[string]string{"image/png": "file.png", "application/pdf": "file.pdf", "IMAGE/JPEG": "file.jpg", "application/x-evil": "file", "": "file"} {
		if got := fileNameFor(telegram.FileRef{Mime: mime}, at); got != want {
			t.Errorf("%q: %q, want %q", mime, got, want)
		}
	}
	if got := fileNameFor(telegram.FileRef{Name: "data", Mime: "image/png"}, at); got != "data" {
		t.Errorf("a name that was given was changed: %q", got)
	}
}

func TestACaptionIsCutOnACharacterBoundary(t *testing.T) {
	s := strings.Repeat("é", 700) // 1400 bytes
	got := cutBytes(s, maxCaptionBytes)
	if len(got) > maxCaptionBytes || !utf8.ValidString(got) || len(got) < maxCaptionBytes-3 {
		t.Fatalf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if cutBytes("short", 10) != "short" || cutBytes("", 10) != "" {
		t.Fatal("a short text was changed")
	}
}
