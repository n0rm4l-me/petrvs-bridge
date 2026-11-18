package telegram

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestAVoiceGoesAsAVoiceMessageWithItsCaptionBytesAndType(t *testing.T) {
	for name, c := range map[string]struct{ file, ctype string }{
		"ogg": {"hello.ogg", "audio/ogg"}, "opus": {"hello.OPUS", "audio/ogg"}, "mp3": {"hello.mp3", "audio/mpeg"}, "m4a": {"hello.m4a", "audio/mp4"},
	} {
		srv, got := uploadServer(t, acceptUpload(31))
		data := []byte("OggS not really audio")
		ids, err := srv.SendFile(context.Background(), -42, OutFile{Name: c.file, Data: data, Caption: "say hi", Voice: true})
		if err != nil || len(ids) != 1 || ids[0] != 31 {
			t.Fatalf("%s: ids %v, err %v", name, ids, err)
		}
		us := got()
		if len(us) != 1 {
			t.Fatalf("%s: %d requests", name, len(us))
		}
		u := us[0]
		if u.method != "sendVoice" || u.field != "voice" || u.chatID != "-42" || u.caption != "say hi" || u.filename != c.file || !bytes.Equal(u.data, data) || u.partType != c.ctype {
			t.Fatalf("%s: %+v", name, u)
		}
	}
}

// A voice message takes the place of a photo when a caller asks for both; the hub never does, so this only pins the order.
func TestAVoiceBeatsAPhoto(t *testing.T) {
	srv, got := uploadServer(t, acceptUpload(1))
	if _, err := srv.SendFile(context.Background(), 1, OutFile{Name: "a.ogg", Data: []byte("x"), Voice: true, Photo: true}); err != nil {
		t.Fatal(err)
	}
	if us := got(); len(us) != 1 || us[0].method != "sendVoice" {
		t.Fatalf("%+v", us)
	}
}

// Telegram refuses a voice message that it cannot play, and one that the user does not take from bots
// (VOICE_MESSAGES_FORBIDDEN). The same bytes go through as a document, with nothing lost.
func TestAVoiceTelegramRefusesIsSentOnceAsADocument(t *testing.T) {
	srv, got := uploadServer(t, func(n int, u sentUpload, w http.ResponseWriter) {
		if u.method == "sendVoice" {
			reply(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: VOICE_MESSAGES_FORBIDDEN"})
			return
		}
		acceptUpload(9)(n, u, w)
	})
	data := []byte("voice that is refused")
	ids, err := srv.SendFile(context.Background(), 1, OutFile{Name: "hello.m4a", Data: data, Caption: "c", Voice: true})
	if err != nil || len(ids) != 1 || ids[0] != 9 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
	us := got()
	if len(us) != 2 || us[0].method != "sendVoice" || us[1].method != "sendDocument" || us[1].field != "document" ||
		!bytes.Equal(us[1].data, data) || us[1].caption != "c" || us[1].filename != "hello.m4a" || us[1].partType != "application/octet-stream" {
		t.Fatalf("%+v", us)
	}
}

func TestAVoiceThatFailsAsADocumentToo(t *testing.T) {
	srv, got := uploadServer(t, func(_ int, _ sentUpload, w http.ResponseWriter) {
		reply(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: chat not found"})
	})
	if _, err := srv.SendFile(context.Background(), 1, OutFile{Name: "hello.ogg", Data: []byte("x"), Voice: true}); err == nil {
		t.Fatal("no error")
	}
	if us := got(); len(us) != 2 {
		t.Fatalf("%d requests, want the voice and one document", len(us))
	}
}

// Only a voice message names what it is made of; a photo and a document stay plain bytes.
func TestOnlyAVoiceNamesItsType(t *testing.T) {
	for _, c := range []struct{ field, name, want string }{
		{"voice", "a.ogg", "audio/ogg"}, {"voice", "a.oga", "audio/ogg"}, {"voice", "a.mp3", "audio/mpeg"}, {"voice", "a.m4a", "audio/mp4"},
		{"voice", "a.bin", "application/octet-stream"}, {"document", "a.ogg", "application/octet-stream"}, {"photo", "a.png", "application/octet-stream"},
	} {
		if got := partType(c.field, c.name); got != c.want {
			t.Errorf("partType(%q, %q) = %q, want %q", c.field, c.name, got, c.want)
		}
	}
}
