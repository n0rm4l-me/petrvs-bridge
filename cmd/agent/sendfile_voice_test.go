package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// kindsQueued reads the closing items of the files in the spool and returns the "kind" each one asks for, by file name.
func kindsQueued(t *testing.T, state string) map[string]string {
	t.Helper()
	sp, err := spool.Open(filepath.Join(state, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sp.List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.Item.Type == wire.TypeFile {
			out[e.Item.Meta[wire.MetaName]] = e.Item.Meta[wire.MetaKind]
		}
	}
	return out
}

func TestTheVoiceFlagQueuesAnAudioFileAsAVoiceMessage(t *testing.T) {
	root := realTemp(t)
	state := filepath.Join(root, "state")
	env := envOf(map[string]string{"PB_STATE_DIR": state})
	hello := mustWrite(t, filepath.Join(root, "work", "hello.ogg"), "OggS not really audio")
	plain := mustWrite(t, filepath.Join(root, "work", "plain.mp3"), "ID3 not really audio")
	if code := sfSendFile([]string{"-voice", hello}, env); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := sfSendFile([]string{plain, "-voice"}, env); code != 0 { // flags may follow the path
		t.Fatalf("exit %d", code)
	}
	if code := sfSendFile([]string{plain}, env); code != 0 { // the same file without the flag stays a document
		t.Fatalf("exit %d", code)
	}
	var voice, document int
	sp, _ := spool.Open(filepath.Join(state, "spool"))
	entries, _ := sp.List()
	for _, e := range entries {
		if e.Item.Type != wire.TypeFile {
			continue
		}
		switch e.Item.Meta[wire.MetaKind] {
		case "voice":
			voice++
		case "document":
			document++
		}
	}
	if voice != 2 || document != 1 {
		t.Fatalf("%d voice and %d document files queued, want 2 and 1 (%v)", voice, document, kindsQueued(t, state))
	}
}

// Something that Telegram would not play as a voice message is refused before anything is queued, with the formats in
// the message, rather than going out as a document the user did not ask for.
func TestTheVoiceFlagRefusesWhatTelegramCannotPlay(t *testing.T) {
	root := realTemp(t)
	state := filepath.Join(root, "state")
	env := envOf(map[string]string{"PB_STATE_DIR": state})
	for _, name := range []string{"notes.txt", "song.flac", "clip.wav", "shot.png", "noext"} {
		p := mustWrite(t, filepath.Join(root, "work", name), "bytes")
		if code := sfSendFile([]string{"-voice", p}, env); code != 1 {
			t.Errorf("%s: exit %d, want 1", name, code)
		}
	}
	if sp, err := spool.Open(filepath.Join(state, "spool")); err != nil || sp.Len() != 0 {
		t.Fatalf("something was queued (%v): %v", err, kindsQueued(t, state))
	}
	if !strings.Contains(sfUsage, "-voice") {
		t.Fatalf("the usage does not mention -voice: %q", sfUsage)
	}
}
