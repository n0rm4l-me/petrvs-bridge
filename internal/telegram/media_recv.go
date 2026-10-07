package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// What the user sends to the bot: a photo or a document, fetched by the hub to be sent on to a machine.

// ErrFileTooLarge says that a file is bigger than the limit given to Fetch, whether Telegram said so or the download did.
var ErrFileTooLarge = errors.New("telegram: the file is larger than allowed")

// rawPhotoSize is one size of a photo; Telegram sends several, smallest first.
type rawPhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// rawDocument is a file sent as a document.
type rawDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// Bounds on what is kept of an update: ids, names and types come from the network and are copied around.
const (
	maxFileIDLen = 256
	maxNameLen   = 512
	maxMimeLen   = 128
)

// mediaOf turns the photo or document of a message into a FileRef, nil when the message has neither (or an unusable one).
// A photo is the largest of its sizes.
func mediaOf(photo []rawPhotoSize, doc *rawDocument) *FileRef {
	if doc != nil && usableID(doc.FileID) {
		return &FileRef{ID: doc.FileID, Name: cut(doc.FileName, maxNameLen), Mime: cut(doc.MimeType, maxMimeLen), Size: max(doc.FileSize, 0)}
	}
	var best *rawPhotoSize
	for i := range photo {
		p := &photo[i]
		if !usableID(p.FileID) {
			continue
		}
		if best == nil || p.FileSize > best.FileSize || (p.FileSize == best.FileSize && int64(p.Width)*int64(p.Height) >= int64(best.Width)*int64(best.Height)) {
			best = p
		}
	}
	if best == nil {
		return nil
	}
	return &FileRef{ID: best.FileID, Mime: "image/jpeg", Size: max(best.FileSize, 0), Photo: true}
}

func usableID(id string) bool { return id != "" && len(id) <= maxFileIDLen }

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Download limits: a minute, plus a second for each MiB of the largest file allowed, so that a slow line can still finish
// a big file while a stalled one does not hold a transfer for ever. A variable so that tests can shorten it.
var (
	fetchBase   = 60 * time.Second
	fetchPerMiB = time.Second
)

func fetchTimeout(maxBytes int64) time.Duration {
	return fetchBase + time.Duration((maxBytes+(1<<20)-1)>>20)*fetchPerMiB
}

// maxRedirects is how many redirects a download follows, all within the same host.
const maxRedirects = 3

// sameHost lets a download follow a redirect inside the same server and no other: the address holds the bot token, and
// another host would receive it.
func sameHost(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return errors.New("too many redirects")
	}
	first := via[0].URL
	if req.URL.Host != first.Host || req.URL.Scheme != first.Scheme {
		return errors.New("redirect to another host refused")
	}
	return nil
}

// filePathURL turns the file_path of getFile into the escaped tail of the download address, refusing what is not a plain
// relative path. Telegram's own paths look like "documents/file_12.pdf".
func filePathURL(p string) (string, bool) {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") {
		return "", false
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == '\\' }) {
			return "", false
		}
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/"), true
}

// Fetch downloads a file that the user sent. It refuses, before any download, a file that Telegram says is larger than
// maxBytes, and stops a download that turns out larger, whatever the sizes said. The bot token is part of the download
// address and never appears in an error.
func (c *HTTP) Fetch(ctx context.Context, f FileRef, maxBytes int64) ([]byte, error) {
	if f.ID == "" || maxBytes <= 0 {
		return nil, errors.New("telegram: nothing to fetch")
	}
	if f.Size > maxBytes {
		return nil, ErrFileTooLarge
	}
	out, err := c.call(ctx, "getFile", map[string]any{"file_id": f.ID}, sendTimeout)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == http.StatusBadRequest && strings.Contains(strings.ToLower(ae.Desc), "too big") {
			return nil, ErrFileTooLarge // the Bot API serves files up to 20 MB
		}
		return nil, err
	}
	var info struct {
		FileSize int64  `json:"file_size"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(out.Result, &info); err != nil {
		return nil, errors.New("telegram getFile: unreadable answer")
	}
	if info.FileSize > maxBytes {
		return nil, ErrFileTooLarge
	}
	tail, ok := filePathURL(info.FilePath)
	if !ok {
		return nil, errors.New("telegram getFile: no usable file path")
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout(maxBytes))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+tail, nil)
	if err != nil {
		return nil, clean("download", err)
	}
	hc := *c.hc
	hc.CheckRedirect = sameHost
	if c.recycle.CompareAndSwap(true, false) {
		c.hc.CloseIdleConnections()
	}
	resp, err := hc.Do(req)
	if err != nil {
		c.hc.CloseIdleConnections()
		c.recycle.Store(true)
		return nil, clean("download", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrRateLimited{After: retryAfterHeader(resp.Header)}
	case resp.StatusCode != http.StatusOK:
		return nil, &APIError{Method: "download", Code: resp.StatusCode, Desc: http.StatusText(resp.StatusCode)}
	case resp.ContentLength > maxBytes:
		return nil, ErrFileTooLarge
	}
	// One byte more than allowed is enough to tell that the file is too large, without reading the rest of it.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, clean("download", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrFileTooLarge
	}
	return data, nil
}

var _ FileFetcher = (*HTTP)(nil)
