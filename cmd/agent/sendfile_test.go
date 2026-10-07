package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func sfSpool(t *testing.T) (*spool.Spool, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sp, dir
}

func sfFill(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%020d-1-%08d.json", i+1, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"type":"reply","body":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// When the queue fills up while a file is being queued, the parts already in it are taken out again: half a file would
// only make the hub wait for the rest.
func TestAFileThatDoesNotFitLeavesNothingBehind(t *testing.T) {
	sp, dir := sfSpool(t)
	sfFill(t, dir, spool.MaxFiles-spool.FileReserve-2) // room for two more: the parts fit, the closing item does not
	data := make([]byte, 200_000)
	err := sfQueue(sp, dir, data, filetx.Chunks(data), "a.bin", "")
	if err != spool.ErrFull {
		t.Fatalf("err = %v", err)
	}
	if n := sp.Len(); n != spool.MaxFiles-spool.FileReserve-2 {
		t.Fatalf("%d items left; the parts of the refused file are still there", n)
	}
}

func TestTakingTheFileBackLeavesWhatOthersQueuedAtTheSameTime(t *testing.T) {
	sp, dir := sfSpool(t)
	sfFill(t, dir, spool.MaxFiles-spool.FileReserve-2)
	before := sfSpoolNames(dir)
	// another writer (a hook that sends a reply) adds a message while this transfer is being queued
	if err := sp.Add(wire.TypeReply, "a reply from somewhere else"); err != nil {
		t.Fatal(err)
	}
	other := sp.Len()
	data := make([]byte, 200_000)
	if err := sfQueue(sp, dir, data, filetx.Chunks(data), "a.bin", ""); err != spool.ErrFull {
		t.Fatalf("err = %v", err)
	}
	if sp.Len() != other {
		t.Fatalf("%d items, want %d", sp.Len(), other)
	}
	entries, _ := sp.List()
	found := false
	for _, e := range entries {
		if e.Item.Body == "a reply from somewhere else" {
			found = true
		}
	}
	if !found || len(before) >= other+1 {
		t.Fatal("the reply queued by someone else was removed")
	}
}

func TestTheRateLimitLooksAtTheLastMinuteOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) func() time.Time { return func() time.Time { return now.Add(d) } }
	for i := 0; i < sfRateFiles; i++ {
		if err := sfTakeSlot(dir, at(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	// a file at second 10: the oldest (second 0) is still inside the minute; the message says when to come back
	err := sfTakeSlot(dir, at(10*time.Second))
	if err == nil || !strings.Contains(err.Error(), "at most 10 a minute") || !strings.Contains(err.Error(), "51 s") {
		t.Fatalf("%v", err)
	}
	// a file counts for a minute: at second 59 the one from second 0 is still in, at 60 it is out and a place is free
	if err := sfTakeSlot(dir, at(59*time.Second)); err == nil {
		t.Fatal("the limit did not hold")
	}
	if err := sfTakeSlot(dir, at(60*time.Second)); err != nil {
		t.Fatalf("%v", err)
	}
	if err := sfTakeSlot(dir, at(60*time.Second)); err == nil {
		t.Fatal("the limit did not hold after the window moved")
	}
	if err := sfTakeSlot(dir, at(61*time.Second)); err != nil {
		t.Fatalf("%v", err)
	}
	if err := sfTakeSlot(dir, at(61*time.Second)); err == nil {
		t.Fatal("the limit did not hold after the window moved again")
	}
	// the file holds no more than the last minute
	raw, _ := os.ReadFile(filepath.Join(dir, "send-file-times"))
	if n := len(strings.Fields(string(raw))); n != sfRateFiles {
		t.Fatalf("%d times kept", n)
	}
}

func TestARecordOfTimesThatCannotBeUsedStopsTheSend(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "send-file-times"), 0o700); err != nil { // a directory where the file should be
		t.Fatal(err)
	}
	if err := sfTakeSlot(dir, time.Now); err == nil {
		t.Fatal("the limit was skipped because its record cannot be used")
	}
}

func TestSecretsAreRecognisedByTheirContent(t *testing.T) {
	for body, want := range map[string]string{
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----": "a private key",
		"-----BEGIN RSA PRIVATE KEY-----":                                             "a private key",
		"-----BEGIN PRIVATE KEY-----":                                                 "a private key",
		"-----BEGIN ENCRYPTED PRIVATE KEY-----":                                       "a private key",
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYBF0":                            "a private key",
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQENBF0":                             "",
		"notes\n\n-----BEGIN EC PRIVATE KEY-----\nabc":                                "a private key",
		`{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nabc"}`: "a private key",
		"aws_access_key_id = AKIAABCDEFGHIJKLMNOP":                                    "an AWS access key",
		"token: ghp_abcdefghijklmnopqrstuvwxyz0123456789":                             "a GitHub token",
		"SLACK=xoxb-123456789012-abcdefghij":                                          "a Slack token",
		"BOT=123456789:AAEhBP0av18Rl3AcWF1cBjD1rSBo3GtZ3dq":                           "a Telegram bot token",
		`{"name":"laptop","static":"AAAA","sign":"BBBB"}`:                             "a key file of the bridge",
		"-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----":                 "",
		"-----BEGIN PUBLIC KEY-----\nabc":                                             "",
		"ssh-ed25519 AAAA public":                                                     "",
		"the words PRIVATE KEY----- without a header":                                 "",
		"AKIA is a prefix, AKIAshort is not a key":                                    "",
		`{"name":"laptop"}`:                                                           "",
		"a static sign":                                                               "",
		"":                                                                            "",
	} {
		if got := sfSecretInside([]byte(body)); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
	far := strings.Repeat("x", sfSniffBytes+10) + "-----BEGIN PRIVATE KEY-----"
	if sfSecretInside([]byte(far)) != "" {
		t.Error("a marker far into a big file was looked for")
	}
}

func TestTheCaptionIsTrimmedCutOnACharacterAndMadeValid(t *testing.T) {
	if got := sfCaption("  hello \n"); got != "hello" {
		t.Fatalf("%q", got)
	}
	for _, in := range []string{strings.Repeat("é", 2000), strings.Repeat("日", 2000), strings.Repeat("😀", 2000), strings.Repeat("a\xff", 2000)} {
		got := sfCaption(in)
		if len(got) > sfCaptionBytes || strings.ToValidUTF8(got, "") != got || len(got) < sfCaptionBytes-4 {
			t.Errorf("%d bytes, valid %v", len(got), strings.ToValidUTF8(got, "") == got)
		}
	}
}
