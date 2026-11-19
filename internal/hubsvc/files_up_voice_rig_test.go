package hubsvc_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// A voice message queued by the agent reaches the chat once, as a voice message, with its caption (the hub adds the agent's
// tag only when there are several agents).
func TestAVoiceMessageGoesToTheChatAsAVoiceMessage(t *testing.T) {
	r := testrig.New(t, testrig.Options{})
	r.StartAll()
	a := r.Agents["laptop"]
	data := upFileBytes(200_000, 7)
	tr := filetx.NewID()
	chunks := filetx.Chunks(data)
	for i, c := range chunks {
		if err := a.Spool.AddItem(upPartItem(tr, i, c)); err != nil {
			t.Fatal(err)
		}
	}
	closing := upClosingItem(tr, len(chunks), data, "hello.ogg", "say hi")
	closing.Meta[wire.MetaKind] = "voice"
	if err := a.Spool.AddItem(closing); err != nil {
		t.Fatal(err)
	}
	r.Eventually("the upload", func() bool { return len(r.TG.Uploads()) >= 1 })
	r.Eventually("the agent to be told that everything arrived", upEmptied(a))
	r.Never("a second upload", 600*time.Millisecond, func() bool { return len(r.TG.Uploads()) > 1 })
	up := r.TG.Uploads()[0]
	if !bytes.Equal(up.Data, data) || up.Name != "hello.ogg" || up.Caption != "say hi" || !up.Voice || up.Photo {
		t.Fatalf("%q %q voice %v photo %v, %d bytes", up.Name, up.Caption, up.Voice, up.Photo, len(up.Data))
	}
	if r.Sent("did not arrive") || r.Sent("⚠️") {
		t.Fatalf("the user was told of a problem: %q", r.SentTexts())
	}
}
