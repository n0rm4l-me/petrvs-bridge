package telegram

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChunksNeverReturnABlankPart(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		max  int
	}{
		"a tail of white space after a long line": {strings.Repeat("x", 3999) + "\n   ", MaxMessage},
		"only spaces":            {"   ", MaxMessage},
		"only line breaks":       {"\n\n\n", MaxMessage},
		"a whole part of spaces": {strings.Repeat(" ", MaxMessage) + "x", MaxMessage},
	} {
		for _, p := range Chunks(tc.text, tc.max) {
			if strings.TrimSpace(p) == "" {
				t.Errorf("%s: blank part %q", name, p)
			}
		}
	}
	// Random text full of white space: nothing blank, nothing too long, and no visible character lost.
	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []string{"x", "y", " ", " ", "\n", "\n", "\t", "я", "🙂"}
	visible := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := rng.IntN(120); n > 0; n-- {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		limit := 4 + rng.IntN(30)
		var got strings.Builder
		for _, p := range Chunks(b.String(), limit) {
			if strings.TrimSpace(p) == "" || len(p) > limit {
				t.Fatalf("text %q, limit %d: bad part %q", b.String(), limit, p)
			}
			got.WriteString(p)
		}
		if visible(got.String()) != visible(b.String()) {
			t.Fatalf("text %q, limit %d: content changed", b.String(), limit)
		}
	}
}

func TestATailOfWhiteSpaceIsNotSentToTelegram(t *testing.T) {
	var texts []string
	c, _ := server(t, func(_ string, body map[string]any, w http.ResponseWriter) {
		text := body["text"].(string)
		texts = append(texts, text)
		if strings.TrimSpace(text) == "" {
			reply(w, 400, map[string]any{"ok": false, "description": "Bad Request: message text is empty"})
			return
		}
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": 7}})
	})
	ids, err := c.SendMessage(t.Context(), 1, Message{Text: strings.Repeat("x", 3999) + "\n   "})
	if err != nil || len(ids) != 1 || len(texts) != 1 {
		t.Fatalf("ids=%v err=%v texts=%d", ids, err, len(texts))
	}
	texts = nil
	ids, err = c.SendMessage(t.Context(), 1, Message{Text: "   \n "})
	if err != nil || len(ids) != 0 || len(texts) != 0 {
		t.Fatalf("a message of white space must send nothing: ids=%v err=%v texts=%d", ids, err, len(texts))
	}
}

func TestAnAnswerThatSaysNoCarriesItsCodeAndNoToken(t *testing.T) {
	for _, code := range []int{400, 403, 500} {
		c, _ := server(t, func(_ string, _ map[string]any, w http.ResponseWriter) {
			reply(w, code, map[string]any{"ok": false, "description": "no because"})
		})
		err := c.Send(t.Context(), 1, "x")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != code || ae.Desc != "no because" {
			t.Fatalf("%d: %#v", code, err)
		}
		if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "no because") {
			t.Fatalf("%d: %v", code, err)
		}
	}
	// a proxy's page is not an answer of the API, so it must not look like one
	c, _ := server(t, func(_ string, _ map[string]any, w http.ResponseWriter) { w.WriteHeader(400); w.Write([]byte("<html>")) })
	var ae *APIError
	if err := c.Send(t.Context(), 1, "x"); err == nil || errors.As(err, &ae) {
		t.Fatalf("%#v", err)
	}
}

// closeCounter is a transport that counts CloseIdleConnections, which http.Client.CloseIdleConnections forwards to.
type closeCounter struct {
	fail   bool
	closed atomic.Int32
}

func (c *closeCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.fail {
		return nil, errors.New("connection reset")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[]}`)), Request: r}, nil
}

func (c *closeCounter) CloseIdleConnections() { c.closed.Add(1) }

func TestAFailedCallDropsTheIdleConnections(t *testing.T) {
	rt := &closeCounter{}
	c := NewHTTP(token).WithHTTPClient(&http.Client{Transport: rt})
	if _, err := c.GetUpdates(t.Context(), 0, 0); err != nil || rt.closed.Load() != 0 {
		t.Fatalf("a good call must leave the pool alone: %v %d", err, rt.closed.Load())
	}
	rt.fail = true
	if _, err := c.GetUpdates(t.Context(), 0, 0); err == nil || rt.closed.Load() != 1 {
		t.Fatalf("err=%v closed=%d", err, rt.closed.Load())
	}
}

// blackhole is a TCP proxy that can be told to swallow everything on the connections it has now, as a NAT does that
// forgot a connection: both ends stay open and nothing arrives.
type blackhole struct {
	ln    net.Listener
	mu    sync.Mutex
	muted []*atomic.Bool
}

func newBlackhole(t *testing.T, target string) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackhole{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			mute := &atomic.Bool{}
			b.mu.Lock()
			b.muted = append(b.muted, mute)
			b.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !mute.Load() {
						dst.Write(buf[:n])
					}
					if err != nil {
						dst.Close()
						return
					}
				}
			}
			go pipe(out, in)
			go pipe(in, out)
		}
	}()
	return b
}

// MuteAll makes the connections open now swallow their traffic; connections made later work.
func (b *blackhole) MuteAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.muted {
		m.Store(true)
	}
}

func TestADeadHTTP2ConnectionIsNotReusedForEveryLaterCall(t *testing.T) {
	oldSlack := slack
	slack = 300 * time.Millisecond
	t.Cleanup(func() { slack = oldSlack })
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true, "result": []any{}})
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	proxy := newBlackhole(t, srv.Listener.Addr().String())
	c := NewHTTP(token).WithBaseURL("https://" + proxy.ln.Addr().String()).WithHTTPClient(srv.Client())
	poll := func() error { _, err := c.GetUpdates(t.Context(), 0, 0); return err }

	if err := poll(); err != nil {
		t.Fatal(err)
	}
	if srv.Client().Transport.(*http.Transport).ForceAttemptHTTP2 != true {
		t.Fatal("the test needs HTTP/2")
	}
	proxy.MuteAll() // the connection the client holds is now dead
	if err := poll(); err == nil {
		t.Fatal("a call on a dead connection worked")
	}
	time.Sleep(100 * time.Millisecond) // the hub's next call comes after its back-off, never within microseconds
	if err := poll(); err != nil {
		t.Fatalf("the next call still used the dead connection: %v", err)
	}
}

func TestASilentHTTP2ConnectionIsPingedAndClosed(t *testing.T) {
	oldSlack, oldAfter, oldWait := slack, pingAfter, pingWait
	slack, pingAfter, pingWait = 2*time.Second, 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { slack, pingAfter, pingWait = oldSlack, oldAfter, oldWait })
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true, "result": []any{}})
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	proxy := newBlackhole(t, srv.Listener.Addr().String())
	c := NewHTTP(token).WithBaseURL("https://" + proxy.ln.Addr().String())
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok || tr == http.DefaultTransport {
		t.Fatalf("the shared default transport must not be the one that is changed: %#v", c.hc.Transport)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	poll := func() error { _, err := c.GetUpdates(t.Context(), 0, 0); return err }

	if err := poll(); err != nil {
		t.Fatal(err)
	}
	proxy.MuteAll()
	time.Sleep(600 * time.Millisecond) // the client pings after 100ms of silence and gives up on the connection 100ms later
	start := time.Now()
	if err := poll(); err != nil || time.Since(start) > time.Second {
		t.Fatalf("the silent connection was kept: %v after %v", err, time.Since(start))
	}
}

func TestMessagesToTheChatGoOutOneAtATime(t *testing.T) {
	var mu sync.Mutex
	var order []byte
	c, _ := server(t, func(_ string, body map[string]any, w http.ResponseWriter) {
		text := body["text"].(string)
		time.Sleep(10 * time.Millisecond) // long enough for the other sender to get in between, if it may
		mu.Lock()
		order = append(order, text[0])
		mu.Unlock()
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
	})
	three := func(ch string) string {
		return strings.Repeat(ch, 3990) + "\n" + strings.Repeat(ch, 3990) + "\n" + strings.Repeat(ch, 3990)
	}
	var wg sync.WaitGroup
	for _, ch := range []string{"A", "B"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ids, err := c.SendMessage(t.Context(), 1, Message{Text: three(ch)}); err != nil || len(ids) != 3 {
				t.Errorf("%s: %v %v", ch, ids, err)
			}
		}()
	}
	wg.Wait()
	if got := string(order); got != "AAABBB" && got != "BBBAAA" {
		t.Fatalf("the parts of two messages were mixed: %s", got)
	}
}

func TestWaitingForTheTurnEndsWithTheContext(t *testing.T) {
	stall := make(chan struct{})
	c, _ := server(t, func(string, map[string]any, http.ResponseWriter) { <-stall })
	t.Cleanup(func() { close(stall) })
	go c.Send(context.Background(), 1, "first") // holds the turn until the server answers
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Send(ctx, 1, "second"); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestARateLimitLongerThanTheCapIsHandedBackAtOnce(t *testing.T) {
	var calls atomic.Int32
	c, _ := server(t, func(_ string, _ map[string]any, w http.ResponseWriter) {
		calls.Add(1)
		reply(w, 429, map[string]any{"ok": false, "description": "Too Many Requests: retry after 45", "parameters": map[string]any{"retry_after": 45}})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := c.Send(ctx, 1, "x")
	var rl ErrRateLimited
	if !errors.As(err, &rl) || rl.After != 45*time.Second {
		t.Fatalf("%v", err)
	}
	if calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("the client went on asking: %d calls over %v", calls.Load(), time.Since(start))
	}
}

func TestARateLimitFromAProxyIsHonouredToo(t *testing.T) {
	var calls atomic.Int32
	c, _ := server(t, func(_ string, _ map[string]any, w http.ResponseWriter) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.Write([]byte("<html>slow down</html>"))
			return
		}
		reply(w, 200, map[string]any{"ok": true, "result": map[string]any{"message_id": 3}})
	})
	start := time.Now()
	ids, err := c.SendMessage(t.Context(), 1, Message{Text: "x"})
	if err != nil || len(ids) != 1 || calls.Load() != 2 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("ids=%v err=%v calls=%d after %v", ids, err, calls.Load(), time.Since(start))
	}
	// Polling gets the wait as an error, with the time the proxy named, or none when it named none.
	for header, want := range map[string]time.Duration{"7": 7 * time.Second, "": 0, "soon": 0} {
		c, _ := server(t, func(_ string, _ map[string]any, w http.ResponseWriter) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(429)
			w.Write([]byte("<html>"))
		})
		_, err := c.GetUpdates(t.Context(), 0, time.Second)
		var rl ErrRateLimited
		if !errors.As(err, &rl) || rl.After != want {
			t.Fatalf("Retry-After %q: %v", header, err)
		}
	}
}

func TestAPollShorterThanASecondIsNotAShortPoll(t *testing.T) {
	var got []float64
	c, _ := server(t, func(_ string, body map[string]any, w http.ResponseWriter) {
		got = append(got, body["timeout"].(float64))
		reply(w, 200, map[string]any{"ok": true, "result": []any{}})
	})
	for _, d := range []time.Duration{500 * time.Millisecond, time.Millisecond, 0, 1500 * time.Millisecond, 25 * time.Second} {
		if _, err := c.GetUpdates(t.Context(), 0, d); err != nil {
			t.Fatal(err)
		}
	}
	// 0 stays 0: that is the explicit short poll the hub uses to confirm what it has read
	want := []float64{1, 1, 0, 1, 25}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("timeouts sent: %v, want %v", got, want)
		}
	}
}
