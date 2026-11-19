package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
)

// MaxFileCaption is Telegram's limit for the caption of a file.
const MaxFileCaption = 1024

// sendFileTimeout is how long an upload may take before the size adds to it (one more second per MiB).
var sendFileTimeout = 60 * time.Second

// SendFile uploads a file to the chat: as a voice message when f.Voice is set, as a photo when f.Photo is set and Telegram
// takes it as one, otherwise as a document. It returns the id Telegram gave to the message. The upload holds the slot that
// keeps messages in order, so a text written at the same moment waits for it. A rate limit that Telegram asks to wait out
// for a short while is waited out, as for text; a longer one goes to the caller.
func (c *HTTP) SendFile(ctx context.Context, chatID int64, f OutFile) ([]int64, error) {
	select {
	case c.sending <- struct{}{}:
		defer func() { <-c.sending }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	method, field := "sendDocument", "document"
	switch {
	case f.Voice:
		method, field = "sendVoice", "voice"
	case f.Photo && int64(len(f.Data)) <= filetx.PhotoMaxBytes:
		method, field = "sendPhoto", "photo"
	}
	ids, err := c.uploadFile(ctx, method, field, chatID, f)
	var ae *APIError
	if err != nil && method != "sendDocument" && errors.As(err, &ae) && ae.Code == 400 {
		// Telegram refuses some pictures as photos (sides too long or too thin) and some audio as a voice message (a format it
		// cannot play, or the user has forbidden voice messages from bots); as a document the same bytes go through.
		ids, err = c.uploadFile(ctx, "sendDocument", "document", chatID, f)
	}
	return ids, err
}

// uploadFile makes the multipart request and sends it, waiting out a short rate limit twice at most.
func (c *HTTP) uploadFile(ctx context.Context, method, field string, chatID int64, f OutFile) ([]int64, error) {
	head, tail, contentType, err := fileFrame(field, chatID, f)
	if err != nil {
		return nil, err
	}
	within := sendFileTimeout + time.Duration(len(f.Data)>>20)*time.Second
	for attempt := 0; ; attempt++ {
		out, err := c.postFile(ctx, method, contentType, head, f.Data, tail, within)
		var rl ErrRateLimited
		if errors.As(err, &rl) && attempt < 2 {
			wait := rl.After
			if wait <= 0 {
				wait = 5 * time.Second // the answer did not say
			}
			if wait <= maxRetryWait {
				select {
				case <-time.After(wait):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		if err != nil {
			return nil, err
		}
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		json.Unmarshal(out.Result, &sent) // an answer without an id just means the message cannot be told apart later
		return []int64{sent.MessageID}, nil
	}
}

// fileFrame writes the form up to the bytes of the file and after them, so that the file is never copied: head and
// tail go around f.Data in the request body.
func fileFrame(field string, chatID int64, f OutFile) (head, tail []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := [][2]string{{"chat_id", strconv.FormatInt(chatID, 10)}}
	if caption := cutFileCaption(f.Caption); caption != "" {
		fields = append(fields, [2]string{"caption", caption})
	}
	if len(f.Buttons) > 0 {
		markup, err := json.Marshal(map[string]any{"inline_keyboard": keyboard(f.Buttons)})
		if err != nil {
			return nil, nil, "", err
		}
		fields = append(fields, [2]string{"reply_markup", string(markup)})
	}
	for _, kv := range fields {
		if err := w.WriteField(kv[0], kv[1]); err != nil {
			return nil, nil, "", err
		}
	}
	// SafeName leaves nothing that needs quoting in a header: letters, digits and . _ - + ( ) space.
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filetx.SafeName(f.Name)))
	h.Set("Content-Type", partType(field, f.Name))
	if _, err := w.CreatePart(h); err != nil {
		return nil, nil, "", err
	}
	return buf.Bytes(), []byte("\r\n--" + w.Boundary() + "--\r\n"), w.FormDataContentType(), nil
}

// partType is the Content-Type of the file part. A voice message says what it is made of, as Telegram's own clients do;
// everything else is plain bytes.
func partType(field, name string) string {
	if field == "voice" {
		switch strings.ToLower(path.Ext(name)) {
		case ".ogg", ".oga", ".opus":
			return "audio/ogg"
		case ".mp3":
			return "audio/mpeg"
		case ".m4a":
			return "audio/mp4"
		}
	}
	return "application/octet-stream"
}

// post is call for a request with a file: the same rules for the answer (429, errors of the API, unreadable answers) and
// the same care that the token never reaches an error.
func (c *HTTP) postFile(ctx context.Context, method, contentType string, head, data, tail []byte, within time.Duration) (*apiResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	body := io.MultiReader(bytes.NewReader(head), bytes.NewReader(data), bytes.NewReader(tail))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return nil, clean(method, err)
	}
	req.ContentLength = int64(len(head) + len(data) + len(tail))
	req.Header.Set("Content-Type", contentType)
	if c.recycle.CompareAndSwap(true, false) {
		c.hc.CloseIdleConnections()
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.hc.CloseIdleConnections()
		c.recycle.Store(true)
		return nil, clean(method, err)
	}
	defer resp.Body.Close()
	var out apiResponse
	decodeErr := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&out)
	if resp.StatusCode == http.StatusTooManyRequests {
		after := retryAfterHeader(resp.Header)
		if decodeErr == nil && out.Parameters.RetryAfter > 0 {
			after = seconds(out.Parameters.RetryAfter)
		}
		return nil, ErrRateLimited{After: after}
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("telegram %s: HTTP %d, unreadable answer", method, resp.StatusCode)
	}
	if !out.OK {
		return nil, &APIError{Method: method, Code: resp.StatusCode, Desc: out.Description}
	}
	return &out, nil
}

// fileUTF16Len is how many UTF-16 units a character takes, which is what Telegram's caption limit counts.
func fileUTF16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// cutFileCaption cuts a caption to what Telegram takes and marks the cut. Bytes that are not UTF-8 are replaced first, since
// Telegram refuses them.
func cutFileCaption(s string) string {
	s = strings.ToValidUTF8(s, "�")
	total := 0
	for _, r := range s {
		total += fileUTF16Len(r)
	}
	if total <= MaxFileCaption {
		return s
	}
	units := 0
	for i, r := range s {
		n := fileUTF16Len(r)
		if units+n > MaxFileCaption-1 {
			return s[:i] + "…"
		}
		units += n
	}
	return s
}

var _ FileSender = (*HTTP)(nil)
