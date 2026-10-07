package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClearButtonsEditsTheMessageToAnEmptyKeyboard(t *testing.T) {
	var raw string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+token+"/editMessageReplyMarkup" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer s.Close()
	c := NewHTTP(token).WithBaseURL(s.URL)
	var ed MarkupEditor = c
	if err := ed.ClearButtons(context.Background(), 99, 1234); err != nil {
		t.Fatal(err)
	}
	// An empty list, not null: Telegram answers 400 to a keyboard that is null.
	for _, want := range []string{`"chat_id":99`, `"message_id":1234`, `"reply_markup":{"inline_keyboard":[]}`} {
		if !strings.Contains(raw, want) {
			t.Errorf("the request lacks %s: %s", want, raw)
		}
	}
}

func TestClearButtonsReportsRefusalsWithoutTheToken(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		body any
		want func(error) bool
	}{
		"not modified": {400, map[string]any{"ok": false, "description": "Bad Request: message is not modified"}, func(err error) bool {
			var ae *APIError
			return errors.As(err, &ae) && ae.Code == 400
		}},
		"rate limited": {429, map[string]any{"ok": false, "parameters": map[string]any{"retry_after": 3}}, func(err error) bool {
			var rl ErrRateLimited
			return errors.As(err, &rl) && rl.After.Seconds() == 3
		}},
	} {
		c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) { reply(w, tc.code, tc.body) })
		err := c.ClearButtons(context.Background(), 1, 2)
		if err == nil || !tc.want(err) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a server that is not there: the error must not carry the URL, which holds the token
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	err := NewHTTP(token).WithBaseURL(url).ClearButtons(context.Background(), 1, 2)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("%v", err)
	}
}
