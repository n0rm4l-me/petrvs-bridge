package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slackid"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
)

// DefaultAPIBase is where the Web API lives.
const DefaultAPIBase = slackid.DefaultAPIBase

// The hub already speaks one vocabulary of errors (telegram.APIError, telegram.ErrRateLimited): a 400 means that this very request
// is wrong and will be wrong again, so the agent drops the message; anything else may pass, so the agent sends it again. The
// Slack client answers in the same words.

const (
	maxAPIBody  = 4 << 20
	callTimeout = 30 * time.Second
)

// apiClient makes the calls of the Web API. Every call is a form post with the token in the Authorization header, never in the
// address or the body.
type apiClient struct {
	base string
	hc   *http.Client
}

// reply is what every method answers: ok, or an error code and, for a missing right, which one.
type reply struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Needed   string `json:"needed"`
	Warning  string `json:"warning"`
	Provided string `json:"provided"`
}

// statusOf says how the hub should treat an error code of Slack, in the codes of HTTP the hub understands.
func statusOf(code string) int {
	n, _ := statusKnown(code)
	return n
}

// statusKnown is statusOf, and says whether the code is one that Slack documents or this client knows.
func statusKnown(code string) (int, bool) {
	switch code {
	case "ratelimited", "rate_limited", "message_limit_exceeded":
		return 429, true
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope", "no_permission",
		"not_allowed_token_type", "access_denied", "channel_not_found", "not_in_channel", "is_archived", "user_not_found",
		"user_disabled", "team_access_not_granted", "restricted_action", "action_prohibited", "external_channel_migrating",
		"cannot_dm_bot", "ekm_access_denied", "org_login_required", "accesslimited":
		return 403, true
	case "internal_error", "fatal_error", "service_unavailable", "request_timeout", "team_added_to_org":
		return 503, true
	case "msg_too_long", "invalid_blocks", "invalid_blocks_format", "invalid_arguments", "invalid_arg_name", "invalid_array_arg",
		"invalid_charset", "invalid_form_data", "invalid_post_type", "missing_post_type", "no_text", "invalid_name", "message_not_found",
		"thread_not_found", "file_not_found", "invalid_ts_latest", "invalid_ts_oldest", "too_many_attachments", "no_reaction",
		"already_reacted", "too_many_reactions", "bad_timestamp", "cant_update_message", "edit_window_closed", "user_not_found_in_channel":
		return 400, true
	}
	// A code that is not known: this request is wrong, as far as Slack's own answer goes (see call for the rest).
	return 400, false
}

// clean keeps the address, which can carry a one-time ticket, out of a transport error.
func clean(method string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("slack %s: %w", method, err)
}

// call posts the form to the method and decodes the answer into out (which may be nil). A refusal comes back as a
// *telegram.APIError, a rate limit as telegram.ErrRateLimited.
func (a *apiClient) call(ctx context.Context, method, token string, params url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, strings.NewReader(params.Encode()))
	if err != nil {
		return clean(method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.hc.Do(req)
	if err != nil {
		return clean(method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return clean(method, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return telegram.ErrRateLimited{After: retryAfter(resp.Header)}
	}
	var r reply
	jsonErr := json.Unmarshal(body, &r)
	if resp.StatusCode >= 500 {
		return &telegram.APIError{Method: method, Code: resp.StatusCode, Desc: "the server answered with an error"}
	}
	if jsonErr != nil {
		return &telegram.APIError{Method: method, Code: max(resp.StatusCode, 502), Desc: "the answer is not JSON"}
	}
	if !r.OK {
		if statusOf(r.Error) == 429 {
			return telegram.ErrRateLimited{After: retryAfter(resp.Header)}
		}
		code, known := statusKnown(r.Error)
		if !known && resp.StatusCode >= 400 {
			// Not Slack's own answer (a proxy, a gateway, an error page that happens to be JSON): the HTTP status says more than a
			// code nobody knows, and a 400 here would make the hub drop the message for good.
			code = resp.StatusCode
		}
		return &telegram.APIError{Method: method, Code: code, Desc: describe(r)}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return &telegram.APIError{Method: method, Code: 502, Desc: "the answer has an unexpected shape"}
		}
	}
	return nil
}

// describe is the text of an error code; only what Slack says about the call, and which right is missing.
func describe(r reply) string {
	s := safeWord(r.Error)
	if r.Error == "missing_scope" && r.Needed != "" {
		s += " (needed: " + safeWord(r.Needed) + ")"
	}
	return s
}

// safeWord keeps a code short and made of harmless characters, since it is copied into logs and into the chat.
func safeWord(s string) string {
	if l := strings.ToLower(s); strings.Contains(l, "xox") || strings.Contains(l, "xapp") {
		return "redacted" // whatever it was, it looks like a token, and a code of Slack never does
	}
	if len(s) > 80 {
		s = s[:80]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == ':', r == ',', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
	}
	return b.String()
}

// retryAfter reads Retry-After (seconds); zero when it is absent or not a number.
func retryAfter(h http.Header) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil {
		return 0
	}
	return time.Duration(min(max(n, 0), 24*3600)) * time.Second
}

// post sends bytes to an address that Slack gave (the upload address of a file) and checks that the answer is a success.
func (a *apiClient) post(ctx context.Context, what, rawURL string, data []byte, within time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		return clean(what, err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.hc.Do(req)
	if err != nil {
		return clean(what, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusTooManyRequests {
		return telegram.ErrRateLimited{After: retryAfter(resp.Header)}
	}
	if resp.StatusCode != http.StatusOK {
		return &telegram.APIError{Method: what, Code: resp.StatusCode, Desc: "the upload was refused"}
	}
	return nil
}
