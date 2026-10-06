package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const token = "123456:SECRET-TOKEN-VALUE"

func server(t *testing.T, h func(method string, body map[string]any, w http.ResponseWriter)) (*HTTP, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		h(strings.TrimPrefix(r.URL.Path, "/bot"+token+"/"), body, w)
	}))
	t.Cleanup(s.Close)
	return NewHTTP(token).WithBaseURL(s.URL), s
}

func reply(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func TestGetUpdatesParsesTextAndKeepsEmptyOnes(t *testing.T) {
	c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) {
		if m != "getUpdates" || body["offset"].(float64) != 7 || body["timeout"].(float64) != 25 {
			t.Errorf("bad request %s %v", m, body)
		}
		reply(w, 200, map[string]any{"ok": true, "result": []any{
			map[string]any{"update_id": 7, "message": map[string]any{"message_id": 1, "text": "hello", "from": map[string]any{"id": 42}, "chat": map[string]any{"id": 99}}},
			map[string]any{"update_id": 8, "message": map[string]any{"message_id": 2, "from": map[string]any{"id": 42}, "chat": map[string]any{"id": 99}}}, // a photo: no text
			map[string]any{"update_id": 9}, // some other update type
		}})
	})
	ups, err := c.GetUpdates(context.Background(), 7, 25*time.Second)
	if err != nil || len(ups) != 3 {
		t.Fatalf("%v %v", ups, err)
	}
	if ups[0].Text != "hello" || ups[0].ChatID != 99 || ups[0].UserID != 42 || ups[0].ID != 7 || ups[0].MessageID != 1 {
		t.Fatalf("%+v", ups[0])
	}
	if ups[1].Text != "" || ups[2].ID != 9 {
		t.Fatalf("empty updates must still be returned so the offset advances: %+v", ups)
	}
}

func TestSendSplitsLongTextAndSendsPlain(t *testing.T) {
	var got []string
	c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) {
		if _, has := body["parse_mode"]; has {
			t.Error("parse_mode must not be set: plain text needs no escaping")
		}
		got = append(got, body["text"].(string))
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{}})
	})
	long := strings.Repeat("line of text\n", 1000)
	if err := c.Send(context.Background(), 5, long); err != nil {
		t.Fatal(err)
	}
	if len(got) < 4 {
		t.Fatalf("only %d parts", len(got))
	}
	for _, p := range got {
		if len(p) > MaxMessage || p == "" {
			t.Fatalf("bad part of %d bytes", len(p))
		}
	}
	if strings.ReplaceAll(strings.Join(got, "\n"), "\n", "") != strings.ReplaceAll(long, "\n", "") {
		t.Fatal("text was lost or changed by splitting")
	}
}

func TestChunksNeverCutARune(t *testing.T) {
	text := strings.Repeat("привет 你好 🙂 ", 2000)
	for _, p := range Chunks(text, 100) {
		if len(p) > 100 || strings.ContainsRune(p, '�') {
			t.Fatalf("bad chunk %q", p)
		}
	}
	if got := Chunks("short", 100); len(got) != 1 || got[0] != "short" {
		t.Fatalf("%v", got)
	}
	if got := Chunks("", 100); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestRateLimitIsHonoured(t *testing.T) {
	var calls atomic.Int32
	c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			reply(w, 429, map[string]any{"ok": false, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": 1}})
			return
		}
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{}})
	})
	start := time.Now()
	if err := c.Send(context.Background(), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("calls=%d after %v", calls.Load(), time.Since(start))
	}
}

func TestAPIErrorsAreReported(t *testing.T) {
	c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) {
		reply(w, 403, map[string]any{"ok": false, "description": "Forbidden: bot was blocked by the user"})
	})
	err := c.Send(context.Background(), 1, "x")
	if err == nil || !strings.Contains(err.Error(), "blocked by the user") {
		t.Fatalf("%v", err)
	}
}

func TestTheTokenNeverAppearsInErrors(t *testing.T) {
	c := NewHTTP(token).WithBaseURL("http://127.0.0.1:1") // nothing listens: a transport error whose text normally includes the URL
	errs := []error{}
	_, e1 := c.GetUpdates(context.Background(), 0, time.Second)
	errs = append(errs, e1, c.Send(context.Background(), 1, "x"))
	s, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) { w.Write([]byte("<html>blocked</html>")) })
	_, e3 := s.GetUpdates(context.Background(), 0, time.Second)
	errs = append(errs, e3)
	for i, e := range errs {
		if e == nil {
			t.Fatalf("error %d is nil", i)
		}
		if strings.Contains(e.Error(), "SECRET-TOKEN-VALUE") || strings.Contains(e.Error(), "123456:") {
			t.Fatalf("error %d leaks the token: %v", i, e)
		}
	}
}

func TestContextCancelStopsSend(t *testing.T) {
	c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) {
		reply(w, 429, map[string]any{"ok": false, "parameters": map[string]any{"retry_after": 20}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Send(ctx, 1, "x"); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestFake(t *testing.T) {
	f := NewFake()
	go func() { time.Sleep(50 * time.Millisecond); f.Push(Update{ID: 3, Text: "hi"}) }()
	ups, err := f.GetUpdates(context.Background(), 0, 2*time.Second)
	if err != nil || len(ups) != 1 || ups[0].Text != "hi" {
		t.Fatalf("%v %v", ups, err)
	}
	if ups, _ := f.GetUpdates(context.Background(), 4, 50*time.Millisecond); len(ups) != 0 {
		t.Fatalf("an acknowledged update was returned again: %v", ups)
	}
	f.Send(context.Background(), 1, "out")
	if s := f.Sent(); len(s) != 1 || s[0].Text != "out" {
		t.Fatalf("%v", s)
	}
}
