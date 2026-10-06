package hubsvc

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestAStrangersMessagesAreCountedButDoNotFloodTheLog(t *testing.T) {
	cfg := base(t)
	buf := &lockedBuf{}
	cfg.Log = slog.New(slog.NewTextHandler(buf, nil))
	now := time.Unix(1_800_000_000, 0)
	cfg.Now = func() time.Time { return now }
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		h.noteUnauthorized(99, 99)
	}
	if h.Stats().Unauthorized != 500 {
		t.Fatalf("counted %d", h.Stats().Unauthorized)
	}
	if n := strings.Count(buf.String(), "unauthorised sender"); n != 1 {
		t.Fatalf("%d log lines for one burst", n)
	}
	now = now.Add(2 * time.Minute)
	h.noteUnauthorized(99, 99)
	out := buf.String()
	if strings.Count(out, "unauthorised sender") != 2 || !strings.Contains(out, "also_since_last_log=499") {
		t.Fatalf("the second log line should say how many were held back:\n%s", out)
	}
}
