package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sendBodies runs a fake Bot API, answers each sendMessage with answer(n) (the n-th request, from 1) and returns what was
// asked, as decoded JSON.
func sendServer(t *testing.T, answer func(n int, req map[string]any) (int, any)) (*HTTP, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var got []map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		mu.Lock()
		got = append(got, req)
		n := len(got)
		mu.Unlock()
		code, body := answer(n, req)
		reply(w, code, body)
	}))
	t.Cleanup(s.Close)
	return NewHTTP(token).WithBaseURL(s.URL), func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), got...)
	}
}

func ok(n int, _ map[string]any) (int, any) {
	return 200, map[string]any{"ok": true, "result": map[string]any{"message_id": 500 + n}}
}

func TestMarkdownGoesOutAsHTML(t *testing.T) {
	c, reqs := sendServer(t, ok)
	_, err := c.SendMessage(context.Background(), 5, Message{Text: "**done** in `1s`\n```\nx < y\n```", Markdown: true})
	if err != nil {
		t.Fatal(err)
	}
	got := reqs()
	if len(got) != 1 || got[0]["parse_mode"] != "HTML" || got[0]["text"] != "<b>done</b> in <code>1s</code>\n<pre>x &lt; y</pre>" {
		t.Fatalf("%+v", got)
	}
}

func TestPlainTextIsSentAsItWasWithNoParseMode(t *testing.T) {
	c, reqs := sendServer(t, ok)
	c.SendMessage(context.Background(), 5, Message{Text: "**not** formatted <b>"})
	got := reqs()
	if len(got) != 1 || got[0]["text"] != "**not** formatted <b>" || got[0]["parse_mode"] != nil {
		t.Fatalf("%+v", got)
	}
}

// Whatever Telegram thinks of the formatting, the words arrive.
func TestWhenTelegramRefusesTheFormattingThePartGoesAsPlainText(t *testing.T) {
	c, reqs := sendServer(t, func(n int, req map[string]any) (int, any) {
		if req["parse_mode"] == "HTML" {
			return 400, map[string]any{"ok": false, "description": "Bad Request: can't parse entities: Unsupported start tag"}
		}
		return ok(n, req)
	})
	ids, err := c.SendMessage(context.Background(), 5, Message{Text: "**done**", Markdown: true, Buttons: [][]Button{{{Text: "ok", Data: "x"}}}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
	got := reqs()
	if len(got) != 2 || got[0]["parse_mode"] != "HTML" || got[1]["parse_mode"] != nil || got[1]["text"] != "**done**" {
		t.Fatalf("%+v", got)
	}
	if got[1]["reply_markup"] == nil {
		t.Fatal("the buttons were lost with the formatting")
	}
}

func TestAnotherRefusalIsNotMistakenForAFormattingProblem(t *testing.T) {
	c, reqs := sendServer(t, func(n int, req map[string]any) (int, any) {
		return 403, map[string]any{"ok": false, "description": "Forbidden: bot was blocked by the user"}
	})
	if _, err := c.SendMessage(context.Background(), 5, Message{Text: "**x**", Markdown: true}); err == nil {
		t.Fatal("no error")
	}
	if n := len(reqs()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

func TestALongMarkdownTextIsCutSoThatEachPartIsFormattedOnItsOwn(t *testing.T) {
	c, reqs := sendServer(t, ok)
	var b strings.Builder
	b.WriteString("```sh\n")
	for i := 0; i < 600; i++ {
		b.WriteString("echo line number that is rather long to fill the message\n")
	}
	b.WriteString("```\n**the end**")
	ids, err := c.SendMessage(context.Background(), 5, Message{Text: b.String(), Markdown: true})
	if err != nil {
		t.Fatal(err)
	}
	got := reqs()
	if len(got) < 3 || len(ids) != len(got) {
		t.Fatalf("%d requests, %d ids", len(got), len(ids))
	}
	for i, r := range got {
		text := r["text"].(string)
		if strings.Count(text, "<pre>")+strings.Count(text, "<pre><code")-strings.Count(text, "<pre><code") != strings.Count(text, "</pre>") {
			t.Errorf("part %d has an open <pre>", i)
		}
		if r["parse_mode"] != "HTML" {
			t.Errorf("part %d is not formatted", i)
		}
	}
	if !strings.Contains(got[len(got)-1]["text"].(string), "<b>the end</b>") {
		t.Error("the end of the text is missing or unformatted")
	}
}
