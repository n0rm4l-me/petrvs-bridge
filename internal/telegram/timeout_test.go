package telegram

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAStalledConnectionDoesNotHangTheHub(t *testing.T) {
	oldSlack, oldSend := slack, sendTimeout
	slack, sendTimeout = 150*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { slack, sendTimeout = oldSlack, oldSend })
	stall := make(chan struct{})
	c, _ := server(t, func(string, map[string]any, http.ResponseWriter) { <-stall })
	t.Cleanup(func() { close(stall) }) // registered after the server, so it runs first: the server waits for its handlers
	for name, call := range map[string]func() error{
		"poll": func() error { _, err := c.GetUpdates(t.Context(), 0, 50*time.Millisecond); return err },
		"send": func() error { return c.Send(t.Context(), 1, "hi") },
	} {
		start := time.Now()
		err := call()
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("%s: err=%v after %v", name, err, time.Since(start))
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("%s: the token is in the error: %v", name, err)
		}
	}
}

func TestMessageDateIsParsed(t *testing.T) {
	c, _ := server(t, func(_ string, _ map[string]any, w http.ResponseWriter) {
		reply(w, 200, map[string]any{"ok": true, "result": []map[string]any{
			{"update_id": 5, "message": map[string]any{"message_id": 1, "date": 1_800_000_000, "text": "hi", "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 8}}},
			{"update_id": 6, "message": map[string]any{"message_id": 2, "text": "no date", "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 8}}},
		}})
	})
	ups, err := c.GetUpdates(t.Context(), 0, time.Second)
	if err != nil || len(ups) != 2 {
		t.Fatalf("%v %v", ups, err)
	}
	if !ups[0].Date.Equal(time.Unix(1_800_000_000, 0)) || !ups[1].Date.IsZero() {
		t.Fatalf("dates: %v %v", ups[0].Date, ups[1].Date)
	}
}
