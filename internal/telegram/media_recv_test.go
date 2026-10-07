package telegram

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func msg(extra map[string]any) map[string]any {
	m := map[string]any{"message_id": 5, "date": 1700000000, "from": map[string]any{"id": 42}, "chat": map[string]any{"id": 99}}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func updates(t *testing.T, msgs ...map[string]any) []Update {
	t.Helper()
	c, _ := server(t, func(m string, body map[string]any, w http.ResponseWriter) {
		var res []any
		for i, x := range msgs {
			res = append(res, map[string]any{"update_id": 10 + i, "message": x})
		}
		reply(w, 200, map[string]any{"ok": true, "result": res})
	})
	ups, err := c.GetUpdates(context.Background(), 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return ups
}

func TestAPhotoIsTheLargestSizeWithTheCaptionAsText(t *testing.T) {
	ups := updates(t, msg(map[string]any{
		"caption": "what is this?",
		"photo": []any{
			map[string]any{"file_id": "small", "file_size": 1200, "width": 90, "height": 60},
			map[string]any{"file_id": "big", "file_size": 90000, "width": 1280, "height": 960},
			map[string]any{"file_id": "mid", "file_size": 20000, "width": 320, "height": 240},
		}}))
	m := ups[0].Media
	if m == nil || m.ID != "big" || !m.Photo || m.Size != 90000 || m.Name != "" || ups[0].Text != "what is this?" {
		t.Fatalf("%+v %+v", ups[0], m)
	}
	if ups[0].ChatID != 99 || ups[0].UserID != 42 || ups[0].MessageID != 5 || ups[0].Date.IsZero() {
		t.Fatalf("the rest of the message was lost: %+v", ups[0])
	}
}

func TestAPhotoWithoutSizesInBytesIsTheOneWithMostPixels(t *testing.T) {
	ups := updates(t, msg(map[string]any{"photo": []any{
		map[string]any{"file_id": "a", "width": 90, "height": 60},
		map[string]any{"file_id": "c", "width": 1280, "height": 960},
		map[string]any{"file_id": "b", "width": 320, "height": 240},
	}}))
	if m := ups[0].Media; m == nil || m.ID != "c" || m.Size != 0 {
		t.Fatalf("%+v", ups[0].Media)
	}
}

func TestADocumentKeepsItsNameTypeAndSize(t *testing.T) {
	ups := updates(t, msg(map[string]any{
		"caption":  "the report",
		"document": map[string]any{"file_id": "doc1", "file_name": "report v2.pdf", "mime_type": "application/pdf", "file_size": 4096},
	}))
	m := ups[0].Media
	if m == nil || m.ID != "doc1" || m.Name != "report v2.pdf" || m.Mime != "application/pdf" || m.Size != 4096 || m.Photo || ups[0].Text != "the report" {
		t.Fatalf("%+v %+v", ups[0], m)
	}
}

func TestOtherKindsOfMessageCarryNoMediaAndNoText(t *testing.T) {
	ups := updates(t,
		msg(map[string]any{"sticker": map[string]any{"file_id": "s"}}),
		msg(map[string]any{"voice": map[string]any{"file_id": "v", "duration": 3}}),
		// A video with a caption must not have its caption taken for something the user typed.
		msg(map[string]any{"video": map[string]any{"file_id": "x"}, "caption": "/lockdown"}),
		msg(map[string]any{"text": "plain"}),
	)
	if len(ups) != 4 {
		t.Fatalf("every update must come back so that the offset advances: %d", len(ups))
	}
	for _, u := range ups[:3] {
		if u.Media != nil || u.Text != "" {
			t.Fatalf("%+v", u)
		}
	}
	if ups[3].Text != "plain" || ups[3].Media != nil {
		t.Fatalf("%+v", ups[3])
	}
}

func TestAMediaFileWithoutAnIdOrWithAHugeOneIsIgnored(t *testing.T) {
	ups := updates(t,
		msg(map[string]any{"document": map[string]any{"file_name": "a.txt"}, "caption": "x"}),
		msg(map[string]any{"document": map[string]any{"file_id": strings.Repeat("A", 300)}}),
		msg(map[string]any{"photo": []any{map[string]any{"width": 10, "height": 10}}}),
	)
	for _, u := range ups {
		if u.Media != nil || u.Text != "" {
			t.Fatalf("%+v", u)
		}
	}
}

func TestANegativeOrHugeNameAndSizeAreBounded(t *testing.T) {
	ups := updates(t, msg(map[string]any{"document": map[string]any{"file_id": "d", "file_name": strings.Repeat("é", 2000), "mime_type": strings.Repeat("m", 1000), "file_size": -5}}))
	m := ups[0].Media
	if m == nil || m.Size != 0 || len(m.Name) > maxNameLen || len(m.Mime) > maxMimeLen {
		t.Fatalf("%+v", m)
	}
	if !strings.HasSuffix(m.Name, "é") {
		t.Fatal("the name was cut inside a character")
	}
}

// ---- Fetch ----

const filePath = "documents/file_7.bin"

// fileServer answers getFile with info and serves the download with serve.
func fileServer(t *testing.T, info map[string]any, serve func(w http.ResponseWriter, r *http.Request)) (*HTTP, *atomic.Int32) {
	t.Helper()
	var downloads atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/bot"+token+"/getFile":
			reply(w, 200, map[string]any{"ok": true, "result": info})
		case strings.HasPrefix(r.URL.Path, "/file/bot"+token+"/"):
			downloads.Add(1)
			serve(w, r)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return NewHTTP(token).WithBaseURL(s.URL), &downloads
}

func body(data []byte) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.Write(data) }
}

func TestFetchDownloadsTheFile(t *testing.T) {
	want := bytes.Repeat([]byte("0123456789"), 1000)
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_size": len(want), "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/bot"+token+"/"+filePath {
			t.Errorf("downloaded from %s", r.URL.Path)
		}
		w.Write(want)
	})
	got, err := c.Fetch(context.Background(), FileRef{ID: "f", Size: int64(len(want))}, 1<<20)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
}

func TestFetchRefusesWhatTelegramSaysIsTooLargeBeforeDownloading(t *testing.T) {
	c, downloads := fileServer(t, map[string]any{"file_id": "f", "file_size": 5000, "file_path": filePath}, body(make([]byte, 5000)))
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1000); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("the size in the getFile answer was not enough: %v", err)
	}
	if downloads.Load() != 0 {
		t.Fatal("the file was downloaded although it was too large")
	}
}

func TestFetchRefusesTheSizeOfTheUpdateWithoutAskingTelegram(t *testing.T) {
	var calls atomic.Int32
	c, _ := server(t, func(string, map[string]any, http.ResponseWriter) { calls.Add(1) })
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f", Size: 2000}, 1000); !errors.Is(err, ErrFileTooLarge) || calls.Load() != 0 {
		t.Fatalf("%v, %d calls", err, calls.Load())
	}
}

func TestFetchStopsADownloadThatIsLargerThanTheSizesSaid(t *testing.T) {
	// getFile claims 10 bytes, the update claims 0 (unknown), the body is far more and has no Content-Length.
	var sent atomic.Int64
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_size": 10, "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		for i := 0; i < 1000; i++ {
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	_, err := c.Fetch(context.Background(), FileRef{ID: "f", Size: 10}, 100<<10)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("%v", err)
	}
	if sent.Load() > 8<<20 {
		t.Fatalf("the client kept reading: the server wrote %d bytes", sent.Load())
	}
}

func TestFetchRefusesADeclaredLengthAboveTheLimit(t *testing.T) {
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.Write(make([]byte, 100000))
	})
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1000); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("%v", err)
	}
}

func TestFetchOfExactlyTheLimitWorks(t *testing.T) {
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, body(make([]byte, 1000)))
	if got, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1000); err != nil || len(got) != 1000 {
		t.Fatalf("%d, %v", len(got), err)
	}
}

func TestFetchOfATooBigFileAsTelegramSaysIt(t *testing.T) {
	c, _ := server(t, func(m string, _ map[string]any, w http.ResponseWriter) {
		reply(w, 400, map[string]any{"ok": false, "description": "Bad Request: file is too big"})
	})
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("%v", err)
	}
}

func TestFetchErrorsNeverCarryTheToken(t *testing.T) {
	for name, serve := range map[string]func(w http.ResponseWriter, r *http.Request){
		"status": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope "+r.URL.Path, 500) },
		"cut off": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte("short"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		},
		"hijack": func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		},
	} {
		c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, serve)
		_, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "123456") {
			t.Fatalf("%s: the token is in the error: %v", name, err)
		}
	}
	// The server is gone: the transport error names the address, which holds the token.
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, body(nil))
	c.WithBaseURL("http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Fetch(ctx, FileRef{ID: "f"}, 1<<20); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}

func TestFetchDownloadErrorsKeepTheirMeaning(t *testing.T) {
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	})
	_, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20)
	var rl ErrRateLimited
	if !errors.As(err, &rl) || rl.After != 7*time.Second {
		t.Fatalf("%v", err)
	}
	c, _ = fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	var ae *APIError
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20); !errors.As(err, &ae) || ae.Code != 404 {
		t.Fatalf("%v", err)
	}
}

func TestFetchDoesNotFollowARedirectToAnotherHost(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Write([]byte("from elsewhere, with " + r.URL.Path))
	}))
	defer other.Close()
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound) // the path holds the token
	})
	_, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20)
	if err == nil || reached.Load() != 0 {
		t.Fatalf("a redirect to another host was followed (%d requests there): %v", reached.Load(), err)
	}
	if strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("%v", err)
	}
}

func TestFetchFollowsARedirectInsideTheSameServer(t *testing.T) {
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/moved") {
			http.Redirect(w, r, r.URL.Path+"/moved", http.StatusFound)
			return
		}
		w.Write([]byte("here"))
	})
	if got, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20); err != nil || string(got) != "here" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestFetchGivesUpOnARedirectLoop(t *testing.T) {
	c, downloads := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	})
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20); err == nil || downloads.Load() > maxRedirects+2 {
		t.Fatalf("%v after %d requests", err, downloads.Load())
	}
}

func TestFetchRefusesAFilePathThatIsNotPlain(t *testing.T) {
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "a/./b", "a\\b", "a/b\nc", strings.Repeat("a/", 600)} {
		c, downloads := fileServer(t, map[string]any{"file_id": "f", "file_path": p}, body([]byte("x")))
		if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20); err == nil || downloads.Load() != 0 {
			t.Errorf("file_path %q was used (%v)", p, err)
		}
	}
	// characters that would change the address are escaped, not interpreted
	var seen string
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": "documents/a b?c#d.txt"}, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
		w.Write([]byte("x"))
	})
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20); err != nil || !strings.HasSuffix(seen, "/documents/a%20b%3Fc%23d.txt") {
		t.Fatalf("%q, %v", seen, err)
	}
}

func TestFetchStopsAStalledDownload(t *testing.T) {
	old := fetchBase
	fetchBase = 300 * time.Millisecond
	defer func() { fetchBase = old }()
	release := make(chan struct{})
	defer close(release)
	c, _ := fileServer(t, map[string]any{"file_id": "f", "file_path": filePath}, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		<-release
	})
	start := time.Now()
	_, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 1<<20)
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestTheDownloadTimeoutIsAMinutePlusASecondPerMiB(t *testing.T) {
	if got := fetchTimeout(20 << 20); got != 80*time.Second {
		t.Fatalf("%v", got)
	}
	if got := fetchTimeout(1); got != 61*time.Second {
		t.Fatalf("%v", got)
	}
}

func TestFetchRefusesNothingToFetch(t *testing.T) {
	c := NewHTTP(token)
	if _, err := c.Fetch(context.Background(), FileRef{}, 10); err == nil {
		t.Fatal("an empty reference was fetched")
	}
	if _, err := c.Fetch(context.Background(), FileRef{ID: "f"}, 0); err == nil {
		t.Fatal("a limit of zero was accepted")
	}
}
