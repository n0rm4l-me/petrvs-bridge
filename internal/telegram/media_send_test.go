package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
)

// sentUpload is what the fake Bot API saw of one request.
type sentUpload struct {
	method   string
	chatID   string
	caption  string
	markup   string
	field    string // "photo" or "document"
	filename string
	data     []byte
	length   int64
	ctype    string
}

// uploadServer answers the file methods with what answer returns for the n-th request (from 1) and records the requests.
func uploadServer(t *testing.T, answer func(n int, u sentUpload, w http.ResponseWriter)) (*HTTP, func() []sentUpload) {
	t.Helper()
	var mu sync.Mutex
	var got []sentUpload
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		u := sentUpload{method: strings.TrimPrefix(r.URL.Path, "/bot"+token+"/"), length: r.ContentLength, ctype: r.Header.Get("Content-Type")}
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			t.Errorf("not a multipart form: %v", err)
			return
		}
		u.chatID, u.caption, u.markup = r.FormValue("chat_id"), r.FormValue("caption"), r.FormValue("reply_markup")
		for _, field := range []string{"photo", "document"} {
			if fh := r.MultipartForm.File[field]; len(fh) == 1 {
				f, _ := fh[0].Open()
				u.field, u.filename = field, fh[0].Filename
				u.data, _ = io.ReadAll(f)
				f.Close()
			}
		}
		mu.Lock()
		got = append(got, u)
		n := len(got)
		mu.Unlock()
		answer(n, u, w)
	}))
	t.Cleanup(s.Close)
	return NewHTTP(token).WithBaseURL(s.URL), func() []sentUpload { mu.Lock(); defer mu.Unlock(); return append([]sentUpload(nil), got...) }
}

func acceptUpload(id int64) func(int, sentUpload, http.ResponseWriter) {
	return func(_ int, _ sentUpload, w http.ResponseWriter) {
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
	}
}

func TestAPhotoGoesAsAPhotoWithItsCaptionAndBytes(t *testing.T) {
	c, got := uploadServer(t, acceptUpload(77))
	data := []byte("\x89PNG not really a picture")
	ids, err := c.SendFile(context.Background(), -42, OutFile{Name: "shot.png", Data: data, Caption: "the screen", Photo: true})
	if err != nil || len(ids) != 1 || ids[0] != 77 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
	us := got()
	if len(us) != 1 {
		t.Fatalf("%d requests", len(us))
	}
	u := us[0]
	if u.method != "sendPhoto" || u.field != "photo" || u.chatID != "-42" || u.caption != "the screen" || u.filename != "shot.png" || !bytes.Equal(u.data, data) {
		t.Fatalf("%+v", u)
	}
	if u.markup != "" {
		t.Fatalf("buttons were not asked for: %q", u.markup)
	}
	if u.length <= int64(len(data)) {
		t.Fatalf("the request did not say how long it is: %d", u.length)
	}
	if !strings.HasPrefix(u.ctype, "multipart/form-data; boundary=") {
		t.Fatalf("content type %q", u.ctype)
	}
}

func TestAnythingElseGoesAsADocument(t *testing.T) {
	big := make([]byte, filetx.PhotoMaxBytes+1)
	for name, f := range map[string]OutFile{
		"not asked as a photo":    {Name: "notes.txt", Data: []byte("hello")},
		"a photo above the limit": {Name: "huge.png", Data: big, Photo: true},
	} {
		c, got := uploadServer(t, acceptUpload(5))
		if _, err := c.SendFile(context.Background(), 1, f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		us := got()
		if len(us) != 1 || us[0].method != "sendDocument" || us[0].field != "document" || !bytes.Equal(us[0].data, f.Data) {
			t.Fatalf("%s: %+v", name, us)
		}
	}
}

// Telegram refuses some pictures as photos (sides too long, too thin) that it takes as documents.
func TestAPhotoTelegramRefusesIsSentOnceAsADocument(t *testing.T) {
	c, got := uploadServer(t, func(n int, u sentUpload, w http.ResponseWriter) {
		if u.method == "sendPhoto" {
			reply(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: PHOTO_INVALID_DIMENSIONS"})
			return
		}
		acceptUpload(9)(n, u, w)
	})
	data := []byte("thin picture")
	ids, err := c.SendFile(context.Background(), 1, OutFile{Name: "thin.png", Data: data, Caption: "c", Photo: true})
	if err != nil || len(ids) != 1 || ids[0] != 9 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
	us := got()
	if len(us) != 2 || us[0].method != "sendPhoto" || us[1].method != "sendDocument" || !bytes.Equal(us[1].data, data) || us[1].caption != "c" || us[1].filename != "thin.png" {
		t.Fatalf("%+v", us)
	}
}

func TestADocumentTelegramRefusesIsNotTriedAgain(t *testing.T) {
	c, got := uploadServer(t, func(_ int, _ sentUpload, w http.ResponseWriter) {
		reply(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: file must be non-empty"})
	})
	_, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 400 || !strings.Contains(ae.Desc, "non-empty") {
		t.Fatalf("%v", err)
	}
	if len(got()) != 1 {
		t.Fatalf("%d requests", len(got()))
	}
	// a photo refused twice (as a photo, then as a document) ends with the second refusal
	c2, got2 := uploadServer(t, func(_ int, _ sentUpload, w http.ResponseWriter) {
		reply(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: nope"})
	})
	if _, err := c2.SendFile(context.Background(), 1, OutFile{Name: "a.png", Data: []byte("x"), Photo: true}); !errors.As(err, &ae) || ae.Method != "sendDocument" {
		t.Fatalf("%v", err)
	}
	if n := len(got2()); n != 2 {
		t.Fatalf("%d requests", n)
	}
}

func TestTheCaptionIsCutToWhatTelegramTakes(t *testing.T) {
	units := func(s string) int {
		n := 0
		for _, r := range s {
			n += fileUTF16Len(r)
		}
		return n
	}
	cases := map[string]string{
		"ascii":  strings.Repeat("a", 3000),
		"accent": strings.Repeat("é", 3000),
		"emoji":  strings.Repeat("😀", 3000),
		"bytes":  strings.Repeat("ab\xff", 1000),
	}
	for name, caption := range cases {
		c, got := uploadServer(t, acceptUpload(1))
		if _, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x"), Caption: caption}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cap := got()[0].caption
		if !utf8.ValidString(cap) || units(cap) > MaxFileCaption || units(cap) < MaxFileCaption-2 || !strings.HasSuffix(cap, "…") {
			t.Errorf("%s: a caption of %d units, valid %v, ends %q", name, units(cap), utf8.ValidString(cap), cap[max(0, len(cap)-6):])
		}
	}
	exact := strings.Repeat("x", MaxFileCaption)
	c, got := uploadServer(t, acceptUpload(1))
	c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x"), Caption: exact})
	if got()[0].caption != exact {
		t.Error("a caption that fits was changed")
	}
	c, got = uploadServer(t, acceptUpload(1))
	c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")})
	if got()[0].caption != "" {
		t.Error("an empty caption was sent as something")
	}
}

// The name is the one piece of the form that a file brings: it must not carry a path or anything that breaks the header.
func TestTheFileNameIsMadeSafe(t *testing.T) {
	for _, name := range []string{"../../etc/pass wd\"x.png", "a\r\nX-Evil: 1.txt", "dir\\sub\\..\\k.txt", "", "...", "ünï.png"} {
		c, got := uploadServer(t, acceptUpload(1))
		if _, err := c.SendFile(context.Background(), 1, OutFile{Name: name, Data: []byte("x")}); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if n := got()[0].filename; n != filetx.SafeName(name) || strings.ContainsAny(n, "/\\\"\r\n") {
			t.Errorf("%q went as %q", name, n)
		}
	}
}

func TestButtonsBecomeAnInlineKeyboard(t *testing.T) {
	c, got := uploadServer(t, acceptUpload(1))
	_, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x"), Buttons: [][]Button{{{Text: "One", Data: "k:a:1:"}, {Text: "Two", Data: "s:a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Keyboard [][]map[string]string `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(got()[0].markup), &m); err != nil || len(m.Keyboard) != 1 || len(m.Keyboard[0]) != 2 ||
		m.Keyboard[0][0]["text"] != "One" || m.Keyboard[0][1]["callback_data"] != "s:a" {
		t.Fatalf("%q (%v)", got()[0].markup, err)
	}
}

func TestATooShortRateLimitIsWaitedOutAndALongOneIsReported(t *testing.T) {
	c, got := uploadServer(t, func(n int, u sentUpload, w http.ResponseWriter) {
		if n == 1 {
			reply(w, 429, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 1", "parameters": map[string]any{"retry_after": 1}})
			return
		}
		acceptUpload(3)(n, u, w)
	})
	start := time.Now()
	ids, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("payload")})
	if err != nil || len(ids) != 1 || ids[0] != 3 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("ids %v, err %v after %v", ids, err, time.Since(start))
	}
	if us := got(); len(us) != 2 || !bytes.Equal(us[1].data, []byte("payload")) {
		t.Fatalf("the retry did not carry the file: %+v", us)
	}

	old := maxRetryWait
	maxRetryWait = 500 * time.Millisecond
	t.Cleanup(func() { maxRetryWait = old })
	c2, got2 := uploadServer(t, func(_ int, _ sentUpload, w http.ResponseWriter) {
		reply(w, 429, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": 60}})
	})
	start = time.Now()
	_, err = c2.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")})
	var rl ErrRateLimited
	if !errors.As(err, &rl) || rl.After != 60*time.Second || time.Since(start) > 400*time.Millisecond || len(got2()) != 1 {
		t.Fatalf("err %v after %v, %d requests", err, time.Since(start), len(got2()))
	}
}

func TestServerErrorsAreAPIErrorsAndNoAnswerIsAnErrorWithoutTheToken(t *testing.T) {
	c, _ := uploadServer(t, func(_ int, _ sentUpload, w http.ResponseWriter) {
		reply(w, 500, map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"})
	})
	var ae *APIError
	if _, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")}); !errors.As(err, &ae) || ae.Code != 500 {
		t.Fatalf("%v", err)
	}

	// a page from a proxy instead of JSON
	c, _ = uploadServer(t, func(_ int, _ sentUpload, w http.ResponseWriter) {
		w.WriteHeader(502)
		io.WriteString(w, "<html>bad gateway</html>")
	})
	if _, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), token) {
		t.Fatalf("%v", err)
	}

	// nothing listening: the error is the transport's, with the URL (and the token in it) taken out
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, err := NewHTTP(token).WithBaseURL(url).SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")})
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), url) {
		t.Fatalf("%v", err)
	}
}

func TestAStalledUploadIsGivenUp(t *testing.T) {
	old := sendFileTimeout
	sendFileTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sendFileTimeout = old })
	stall := make(chan struct{})
	c, _ := uploadServer(t, func(int, sentUpload, http.ResponseWriter) { <-stall })
	t.Cleanup(func() { close(stall) })
	start := time.Now()
	_, err := c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")})
	if err == nil || time.Since(start) > 3*time.Second || strings.Contains(err.Error(), token) {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestTheTimeoutGrowsWithTheSizeOfTheFile(t *testing.T) {
	old := sendFileTimeout
	sendFileTimeout = 10 * time.Millisecond // without the extra second per MiB a 3 MiB file could not be sent at all
	t.Cleanup(func() { sendFileTimeout = old })
	c, _ := uploadServer(t, func(n int, u sentUpload, w http.ResponseWriter) {
		time.Sleep(100 * time.Millisecond)
		acceptUpload(1)(n, u, w)
	})
	if _, err := c.SendFile(context.Background(), 1, OutFile{Name: "big.bin", Data: make([]byte, 3<<20)}); err != nil {
		t.Fatal(err)
	}
}

// An sentUpload holds the slot that keeps messages in order, as a long text does.
func TestAnUploadAndATextDoNotInterleave(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var order []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
		mu.Lock()
		order = append(order, method+" start")
		mu.Unlock()
		if method == "sendDocument" {
			once.Do(func() { close(started) })
			<-release
		}
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		order = append(order, method+" end")
		mu.Unlock()
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
	}))
	t.Cleanup(s.Close)
	c := NewHTTP(token).WithBaseURL(s.URL)
	done := make(chan struct{}, 2)
	go func() {
		c.SendFile(context.Background(), 1, OutFile{Name: "a.txt", Data: []byte("x")})
		done <- struct{}{}
	}()
	<-started
	go func() { c.SendMessage(context.Background(), 1, Message{Text: "hello"}); done <- struct{}{} }()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if len(order) != 1 {
		t.Errorf("the text went out during the upload: %v", order)
	}
	mu.Unlock()
	close(release)
	<-done
	<-done
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "sendDocument start,sendDocument end,sendMessage start,sendMessage end" {
		t.Fatalf("%v", order)
	}
}

func TestAnUploadWaitingForTheSlotGivesUpWithItsContext(t *testing.T) {
	c := NewHTTP(token)
	c.sending <- struct{}{} // somebody else is sending
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.SendFile(ctx, 1, OutFile{Name: "a.txt", Data: []byte("x")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}
