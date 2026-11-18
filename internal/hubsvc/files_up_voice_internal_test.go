package hubsvc

import (
	"context"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// The agent says that a file is a voice message, but only an audio file that Telegram can play is sent as one: the hub
// looks at the name itself, as it does for a photo.
func TestTheVoiceFlagIsNotTakenOnTrust(t *testing.T) {
	f := newUpFixture(t, nil)
	ogg := []byte("OggS....")
	p := upFilePayload("f", "t0000000001", 1, ogg, "hello.ogg", "")
	p.Meta[wire.MetaKind] = "voice"
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, ogg))
	f.h.upFile(context.Background(), "laptop", p)

	notes := []byte("plain text")
	q := upFilePayload("g", "t0000000002", 1, notes, "notes.txt", "")
	q.Meta[wire.MetaKind] = "voice" // an agent that claims a text file is a voice message
	f.h.upPart("laptop", upPartPayload("q", "t0000000002", 0, notes))
	f.h.upFile(context.Background(), "laptop", q)

	asDoc := []byte("OggS....more")
	r := upFilePayload("h", "t0000000003", 1, asDoc, "clip.ogg", "")
	r.Meta[wire.MetaKind] = "document" // and one that asks for a document
	f.h.upPart("laptop", upPartPayload("r", "t0000000003", 0, asDoc))
	f.h.upFile(context.Background(), "laptop", r)

	ups := f.tg.Uploads()
	if len(ups) != 3 || !ups[0].Voice || ups[1].Voice || ups[2].Voice || ups[0].Photo || ups[1].Photo || ups[2].Photo {
		t.Fatalf("uploads: %+v", ups)
	}
}

// An agent from before voice messages sends "photo" or "document"; a hub from before them reads "voice" as a document.
// An unknown kind is a document here as well.
func TestAnUnknownKindIsADocument(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("OggS....")
	p := upFilePayload("f", "t0000000001", 1, data, "hello.ogg", "")
	p.Meta[wire.MetaKind] = "hologram"
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	f.h.upFile(context.Background(), "laptop", p)
	if ups := f.tg.Uploads(); len(ups) != 1 || ups[0].Voice || ups[0].Photo {
		t.Fatalf("uploads: %+v", ups)
	}
}
