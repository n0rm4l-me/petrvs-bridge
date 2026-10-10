package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// maxCaption bounds the words that go with a file (the hub's captions are short; this stops a long one from being refused).
const maxCaption = 1000

// fileName makes a name that is safe to hand to Slack and to show: no directory, no control characters, not empty, at most 200
// bytes.
func fileName(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	n = path.Base(n)
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '‮' || r == '⁦' || r == '⁧' || r == '⁨' || r == '⁩' {
			return -1
		}
		return r
	}, n)
	n = strings.TrimSpace(n)
	if n == "" || n == "." || n == ".." {
		n = "file"
	}
	return cut(n, 200)
}

// SendFile uploads a file to the allowed chat with Slack's three steps (ask for an address, send the bytes, complete the upload
// into the channel). Slack does not say which message the file became, so no id is returned and a reply to the file cannot be
// told apart from any other message. Buttons, if there are any, go in a message of their own right after the file.
func (c *Client) SendFile(ctx context.Context, chatID int64, f telegram.OutFile) ([]int64, error) {
	ch, err := c.chat(chatID)
	if err != nil {
		return nil, err
	}
	if len(f.Data) == 0 {
		return nil, &telegram.APIError{Method: "files.getUploadURLExternal", Code: 400, Desc: "the file is empty"}
	}
	name := fileName(f.Name)
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	var up struct {
		reply
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := c.callRetry(ctx, "files.getUploadURLExternal", c.cfg.BotToken, url.Values{"filename": {name}, "length": {strconv.Itoa(len(f.Data))}}, &up); err != nil {
		return nil, err
	}
	u, ok := c.trusted(up.UploadURL, "https", "http")
	if !ok || !fileIDRe.MatchString(up.FileID) {
		return nil, &telegram.APIError{Method: "files.getUploadURLExternal", Code: 502, Desc: "the upload address is not a Slack address"}
	}
	if err := c.api.post(ctx, "upload", u.String(), f.Data, uploadTimeout(len(f.Data))); err != nil {
		return nil, err
	}
	params := url.Values{
		"files":      {jsonString([]map[string]string{{"id": up.FileID, "title": name}})},
		"channel_id": {ch},
	}
	if cap := strings.TrimSpace(cutRunes(f.Caption, maxCaption)); cap != "" {
		params.Set("initial_comment", Escape(cap))
	}
	if err := c.callRetry(ctx, "files.completeUploadExternal", c.cfg.BotToken, params, nil); err != nil {
		return nil, err
	}
	if !hasButtons(f.Buttons) {
		return nil, nil
	}
	var out struct {
		reply
		TS string `json:"ts"`
	}
	bp := part{text: "·", plain: true, raw: "·"}
	err = c.callRetry(ctx, "chat.postMessage", c.cfg.BotToken, url.Values{
		"channel": {ch}, "text": {bp.text}, "mrkdwn": {"false"}, "blocks": {blocks(bp, f.Buttons)},
	}, &out)
	if err != nil {
		return nil, err
	}
	if id, ok := tsToID(out.TS); ok {
		return []int64{id}, nil
	}
	return nil, nil
}

// uploadTimeout is a minute, plus a second for each MiB, so that a slow line can finish a large file.
func uploadTimeout(n int) time.Duration {
	return time.Minute + time.Duration(n>>20)*time.Second
}

// Fetch downloads a file the user sent. It asks Slack for the file first, refuses one that is larger than maxBytes before any
// download, downloads only from a Slack address with the bot's token, and stops reading at maxBytes whatever the headers say.
func (c *Client) Fetch(ctx context.Context, f telegram.FileRef, maxBytes int64) ([]byte, error) {
	if !fileIDRe.MatchString(f.ID) {
		return nil, &telegram.APIError{Method: "files.info", Code: 400, Desc: "not a file id"}
	}
	var info struct {
		reply
		File struct {
			Size        int64  `json:"size"`
			Mode        string `json:"mode"`
			DownloadURL string `json:"url_private_download"`
			PrivateURL  string `json:"url_private"`
		} `json:"file"`
	}
	if err := c.callRetry(ctx, "files.info", c.cfg.BotToken, url.Values{"file": {f.ID}}, &info); err != nil {
		return nil, err
	}
	if info.File.Size > maxBytes || f.Size > maxBytes {
		return nil, telegram.ErrFileTooLarge
	}
	raw := info.File.DownloadURL
	if raw == "" {
		raw = info.File.PrivateURL
	}
	u, ok := c.trusted(raw, "https", "http")
	if !ok {
		return nil, &telegram.APIError{Method: "files.info", Code: 502, Desc: "the file is not on a Slack address"}
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout(int(min(maxBytes, 1<<30))))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, clean("download", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.BotToken)
	resp, err := c.api.hc.Do(req)
	if err != nil {
		return nil, clean("download", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &telegram.APIError{Method: "download", Code: resp.StatusCode, Desc: "the download was refused"}
	}
	// Slack answers a download it will not serve (a missing files:read right, a file of another workspace) with a web page.
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.HasPrefix(ct, "text/html") && !strings.HasPrefix(strings.ToLower(f.Mime), "text/html") {
		return nil, &telegram.APIError{Method: "download", Code: 403, Desc: "Slack did not serve the file (check the files:read right)"}
	}
	if resp.ContentLength > maxBytes {
		return nil, telegram.ErrFileTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, clean("download", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, telegram.ErrFileTooLarge
	}
	return data, nil
}

var (
	_ telegram.FileSender  = (*Client)(nil)
	_ telegram.FileFetcher = (*Client)(nil)
	_                      = errors.New
	_                      = utf8.RuneError
)
