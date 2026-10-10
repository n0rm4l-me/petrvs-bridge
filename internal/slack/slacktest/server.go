// Package slacktest is a Slack that runs in a test: the parts of the Web API and of Socket Mode that the client uses, on a local
// port, with a way to say what the user does (type a message, send a file, tap a button, type a slash command) and to read what
// the bot did (messages, reactions, uploads). It checks the tokens the way Slack does, so a test that passes the wrong token fails.
package slacktest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The identities of the test workspace.
const (
	BotToken = "xoxb-test-0000000000-bottoken"
	AppToken = "xapp-1-test-0000000000-apptoken"
	UserID   = "U0USER001"
	BotUser  = "U0BOT0001"
	Channel  = "D0TESTDM01"
	Team     = "T0TEAM001"
)

// Posted is a message the bot wrote.
type Posted struct {
	Channel, Text, TS string
	Mrkdwn            bool
	Blocks            string
	Unfurl            bool
	Updated           bool
}

// Upload is a file the bot uploaded.
type Upload struct {
	ID, Name, Channel, Comment string
	Data                       []byte
}

// Server is the fake Slack.
type Server struct {
	t    *testing.T
	HTTP *httptest.Server

	mu        sync.Mutex
	posted    []*Posted
	history   []map[string]any    // the messages of the user, as conversations.history gives them
	reactions map[string][]string // ts -> names currently on the message
	reactLog  []string            // "add name ts" / "remove name ts"
	uploads   map[string]*Upload  // by file id
	pending   map[string]*Upload
	files     map[string][]byte // downloadable by id
	fileMeta  map[string]map[string]any
	calls     map[string]int
	fails     map[string][]failure
	hooks     []string
	acks      []ack
	nextTS    int
	nextEnv   int
	baseSec   int64
	conn      *websocket.Conn
	connected chan struct{}
	opens     int
	closeConn func()

	// UploadURL and SocketURL, if set, are given out instead of the fake's own addresses (to check that the client refuses
	// an address that is not Slack's).
	UploadURL, SocketURL string
}

type failure struct {
	code   string // ok:false with this error
	status int    // an HTTP status instead
	retry  string // Retry-After
}

type ack struct {
	ID      string
	Payload json.RawMessage
}

// New starts the fake. It is closed when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{t: t, reactions: map[string][]string{}, uploads: map[string]*Upload{}, pending: map[string]*Upload{},
		files: map[string][]byte{}, fileMeta: map[string]map[string]any{}, calls: map[string]int{}, fails: map[string][]failure{},
		baseSec: time.Now().Unix() - 100, connected: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/socket", s.socket)
	mux.HandleFunc("/upload/", s.upload)
	mux.HandleFunc("/files/", s.download)
	mux.HandleFunc("/hooks/", s.hook)
	s.HTTP = httptest.NewServer(mux)
	t.Cleanup(s.HTTP.Close)
	return s
}

// APIURL is what to give the client as its API address.
func (s *Server) APIURL() string { return s.HTTP.URL + "/api" }

func (s *Server) host() string { return strings.TrimPrefix(s.HTTP.URL, "http://") }

func (s *Server) ts() string {
	s.nextTS++
	return fmt.Sprintf("%d.%06d", s.baseSec+int64(s.nextTS/900000), 100000+s.nextTS%900000)
}

// ---- the Web API ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// FailNext makes the next call of the method answer ok:false with the error code.
func (s *Server) FailNext(method, code string) {
	s.mu.Lock()
	s.fails[method] = append(s.fails[method], failure{code: code})
	s.mu.Unlock()
}

// RateLimitNext makes the next call of the method answer 429 with Retry-After (seconds).
func (s *Server) RateLimitNext(method string, retryAfter int) {
	s.mu.Lock()
	s.fails[method] = append(s.fails[method], failure{status: 429, retry: strconv.Itoa(retryAfter)})
	s.mu.Unlock()
}

// StatusNext makes the next call of the method answer with the HTTP status.
func (s *Server) StatusNext(method string, status int) {
	s.mu.Lock()
	s.fails[method] = append(s.fails[method], failure{status: status})
	s.mu.Unlock()
}

// Calls says how many times a method was called.
func (s *Server) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.Method != http.MethodPost {
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		writeJSON(w, map[string]any{"ok": false, "error": "invalid_form_data"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "invalid_form_data"})
		return
	}
	if len(r.URL.Query()) > 0 {
		s.t.Errorf("slacktest: %s was called with a query string, the token or the parameters must not be in the address", method)
	}
	s.mu.Lock()
	s.calls[method]++
	var f *failure
	if q := s.fails[method]; len(q) > 0 {
		f = &q[0]
		s.fails[method] = q[1:]
	}
	s.mu.Unlock()
	want := BotToken
	if method == "apps.connections.open" {
		want = AppToken
	}
	if bearer(r) != want {
		writeJSON(w, map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	if f != nil {
		if f.status != 0 {
			if f.retry != "" {
				w.Header().Set("Retry-After", f.retry)
			}
			w.WriteHeader(f.status)
			return
		}
		writeJSON(w, map[string]any{"ok": false, "error": f.code})
		return
	}
	form := r.PostForm
	switch method {
	case "auth.test":
		writeJSON(w, map[string]any{"ok": true, "user_id": BotUser, "team_id": Team, "bot_id": "B0BOT0001"})
	case "apps.connections.open":
		s.mu.Lock()
		s.opens++
		s.mu.Unlock()
		u := "ws://" + s.host() + "/socket?ticket=one-time-secret-ticket"
		if s.SocketURL != "" {
			u = s.SocketURL
		}
		writeJSON(w, map[string]any{"ok": true, "url": u})
	case "conversations.open":
		if form.Get("users") != UserID {
			writeJSON(w, map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "channel": map[string]any{"id": Channel}})
	case "chat.postMessage":
		if form.Get("channel") != Channel {
			writeJSON(w, map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}
		text := form.Get("text")
		if len(text) > 40000 {
			writeJSON(w, map[string]any{"ok": false, "error": "msg_too_long"})
			return
		}
		if b := form.Get("blocks"); b != "" {
			var arr []map[string]any
			if err := json.Unmarshal([]byte(b), &arr); err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": "invalid_blocks"})
				return
			}
			for _, bl := range arr {
				if bl["type"] == "section" {
					if tx, _ := bl["text"].(map[string]any); tx != nil {
						if n := len([]rune(fmt.Sprint(tx["text"]))); n > 3000 {
							writeJSON(w, map[string]any{"ok": false, "error": "invalid_blocks"})
							return
						}
					}
				}
			}
		}
		s.mu.Lock()
		ts := s.ts()
		s.posted = append(s.posted, &Posted{Channel: form.Get("channel"), Text: text, TS: ts, Mrkdwn: form.Get("mrkdwn") != "false",
			Blocks: form.Get("blocks"), Unfurl: form.Get("unfurl_links") != "false"})
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "ts": ts, "channel": Channel})
	case "chat.update":
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, p := range s.posted {
			if p.TS == form.Get("ts") {
				p.Text, p.Blocks, p.Updated = form.Get("text"), form.Get("blocks"), true
				writeJSON(w, map[string]any{"ok": true, "ts": p.TS})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "error": "message_not_found"})
	case "conversations.history":
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []map[string]any
		if oldest := form.Get("oldest"); oldest != "" {
			// what the user wrote after `oldest`, the newest first
			for i := len(s.history) - 1; i >= 0; i-- {
				if h := s.history[i]; fmt.Sprint(h["ts"]) > oldest {
					out = append(out, h)
				}
			}
			if n, _ := strconv.Atoi(form.Get("limit")); n > 0 && len(out) > n {
				out = out[:n]
			}
			writeJSON(w, map[string]any{"ok": true, "messages": out})
			return
		}
		for _, p := range s.posted {
			if p.TS == form.Get("latest") {
				m := map[string]any{"ts": p.TS, "text": p.Text}
				if p.Blocks != "" {
					var bl []any
					json.Unmarshal([]byte(p.Blocks), &bl)
					m["blocks"] = bl
				}
				out = append(out, m)
			}
		}
		writeJSON(w, map[string]any{"ok": true, "messages": out})
	case "reactions.add", "reactions.remove":
		name, ts := form.Get("name"), form.Get("timestamp")
		known := map[string]bool{"eyes": true, "thumbsdown": true, "thumbsup": true, "white_check_mark": true, "x": true}
		if !known[name] {
			writeJSON(w, map[string]any{"ok": false, "error": "invalid_name"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.reactions[ts]
		if method == "reactions.add" {
			for _, n := range cur {
				if n == name {
					writeJSON(w, map[string]any{"ok": false, "error": "already_reacted"})
					return
				}
			}
			s.reactions[ts] = append(cur, name)
			s.reactLog = append(s.reactLog, "add "+name+" "+ts)
		} else {
			var keep []string
			found := false
			for _, n := range cur {
				if n == name {
					found = true
					continue
				}
				keep = append(keep, n)
			}
			if !found {
				writeJSON(w, map[string]any{"ok": false, "error": "no_reaction"})
				return
			}
			s.reactions[ts] = keep
			s.reactLog = append(s.reactLog, "remove "+name+" "+ts)
		}
		writeJSON(w, map[string]any{"ok": true})
	case "files.getUploadURLExternal":
		n, _ := strconv.Atoi(form.Get("length"))
		s.mu.Lock()
		s.nextEnv++
		id := fmt.Sprintf("F0UP%05d", s.nextEnv)
		s.pending[id] = &Upload{ID: id, Name: form.Get("filename")}
		s.mu.Unlock()
		_ = n
		up := s.HTTP.URL + "/upload/" + id
		if s.UploadURL != "" {
			up = s.UploadURL
		}
		writeJSON(w, map[string]any{"ok": true, "file_id": id, "upload_url": up})
	case "files.completeUploadExternal":
		var files []struct{ ID, Title string }
		if err := json.Unmarshal([]byte(form.Get("files")), &files); err != nil || len(files) == 0 {
			writeJSON(w, map[string]any{"ok": false, "error": "invalid_arguments"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, f := range files {
			u := s.pending[f.ID]
			if u == nil || u.Data == nil {
				writeJSON(w, map[string]any{"ok": false, "error": "invalid_arguments"})
				return
			}
			u.Channel, u.Comment = form.Get("channel_id"), form.Get("initial_comment")
			delete(s.pending, f.ID)
			s.uploads[f.ID] = u
		}
		writeJSON(w, map[string]any{"ok": true, "files": []map[string]any{{"id": files[0].ID}}})
	case "files.info":
		s.mu.Lock()
		defer s.mu.Unlock()
		id := form.Get("file")
		data, ok := s.files[id]
		if !ok {
			writeJSON(w, map[string]any{"ok": false, "error": "file_not_found"})
			return
		}
		meta := map[string]any{"id": id, "size": len(data), "mode": "hosted", "url_private_download": s.HTTP.URL + "/files/" + id + "/download"}
		for k, v := range s.fileMeta[id] {
			meta[k] = v
		}
		writeJSON(w, map[string]any{"ok": true, "file": meta})
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/upload/")
	data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.pending[id]
	if u == nil {
		http.Error(w, "no such upload", http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "" {
		s.t.Errorf("slacktest: the bot token was sent to the upload address")
	}
	u.Data = data
	fmt.Fprintf(w, "OK - %d", len(data))
}

// AddFile makes a file the user "sent": the client can download it by its id.
func (s *Server) AddFile(id string, data []byte, meta map[string]any) {
	s.mu.Lock()
	s.files[id] = data
	s.fileMeta[id] = meta
	s.mu.Unlock()
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/files/"), "/")
	if bearer(r) != BotToken {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>please sign in</html>")
		return
	}
	s.mu.Lock()
	data, ok := s.files[parts[0]]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	s.mu.Lock()
	s.hooks = append(s.hooks, string(b))
	s.mu.Unlock()
	io.WriteString(w, "ok")
}

// ---- what the bot did ----

// Posted gives the messages the bot wrote, oldest first.
func (s *Server) Posted() []Posted {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Posted, len(s.posted))
	for i, p := range s.posted {
		out[i] = *p
	}
	return out
}

// Reactions gives the names on a message now.
func (s *Server) Reactions(ts string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reactions[ts]...)
}

// ReactionLog gives every add and remove, in order.
func (s *Server) ReactionLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reactLog...)
}

// Uploads gives the files the bot uploaded.
func (s *Server) Uploads() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Upload
	for _, u := range s.uploads {
		out = append(out, *u)
	}
	return out
}

// Hooks gives the bodies posted to response addresses.
func (s *Server) Hooks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hooks...)
}

// Opens says how many times a socket address was asked for (that is, how many connections were made).
func (s *Server) Opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// Acks gives the envelope ids the client acknowledged, with the payload of each.
func (s *Server) Acks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, a := range s.acks {
		out = append(out, a.ID)
	}
	return out
}

// AckPayload gives what the client answered with the envelope (a slash command's reply).
func (s *Server) AckPayload(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.acks {
		if a.ID == id {
			return string(a.Payload)
		}
	}
	return ""
}

// ---- Socket Mode ----

func (s *Server) socket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("ticket") != "one-time-secret-ticket" {
		http.Error(w, "bad ticket", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	s.mu.Lock()
	s.conn = conn
	s.closeConn = func() { conn.CloseNow() }
	s.mu.Unlock()
	ctx := r.Context()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","num_connections":1}`)); err != nil {
		return
	}
	select {
	case s.connected <- struct{}{}:
	default:
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			s.mu.Lock()
			if s.conn == conn {
				s.conn = nil
			}
			s.mu.Unlock()
			return
		}
		var a struct {
			EnvelopeID string          `json:"envelope_id"`
			Payload    json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &a) == nil && a.EnvelopeID != "" {
			s.mu.Lock()
			s.acks = append(s.acks, ack{ID: a.EnvelopeID, Payload: a.Payload})
			s.mu.Unlock()
		}
	}
}

// WaitConnected waits until the client has a socket open.
func (s *Server) WaitConnected(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		s.mu.Lock()
		up := s.conn != nil
		s.mu.Unlock()
		if up {
			return
		}
		select {
		case <-s.connected:
		case <-deadline:
			t.Fatalf("slacktest: the client did not open the socket within %v", within)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Connected says whether a socket is open now.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// DropSocket closes the socket without a word, as a network that failed would.
func (s *Server) DropSocket() {
	s.mu.Lock()
	c := s.closeConn
	s.conn = nil
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

// Disconnect sends Slack's request to open the connection again.
func (s *Server) Disconnect(reason string) {
	s.send("disconnect", map[string]any{"reason": reason}, "")
}

// send writes one envelope on the socket and returns its id.
func (s *Server) send(typ string, payload any, forceID string) string {
	s.mu.Lock()
	conn := s.conn
	s.nextEnv++
	id := forceID
	if id == "" {
		id = fmt.Sprintf("env-%d", s.nextEnv)
	}
	s.mu.Unlock()
	if conn == nil {
		s.t.Fatalf("slacktest: no socket is open to send %s on", typ)
	}
	raw, _ := json.Marshal(payload)
	msg, _ := json.Marshal(map[string]any{"type": typ, "envelope_id": id, "payload": json.RawMessage(raw), "accepts_response_payload": typ == "slash_commands"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		s.t.Fatalf("slacktest: %v", err)
	}
	return id
}

// Msg describes a message of the user.
type Msg struct {
	Channel, User, Team, Text, ThreadTS, Subtype, BotID, ChannelType string
	TS                                                               string
	Files                                                            []map[string]any
}

func (m Msg) event(s *Server) (map[string]any, string) {
	if m.Channel == "" {
		m.Channel = Channel
	}
	if m.User == "" {
		m.User = UserID
	}
	if m.ChannelType == "" {
		m.ChannelType = "im"
	}
	if m.Team == "" {
		m.Team = Team
	}
	ts := m.TS
	if ts == "" {
		s.mu.Lock()
		ts = s.ts()
		s.mu.Unlock()
	}
	ev := map[string]any{"type": "message", "channel": m.Channel, "channel_type": m.ChannelType, "user": m.User, "team": m.Team, "text": m.Text, "ts": ts}
	if m.ThreadTS != "" {
		ev["thread_ts"] = m.ThreadTS
	}
	if m.Subtype != "" {
		ev["subtype"] = m.Subtype
	}
	if m.BotID != "" {
		ev["bot_id"] = m.BotID
	}
	if m.Files != nil {
		ev["files"] = m.Files
	}
	return ev, ts
}

// remember keeps a message of the user in the history that conversations.history serves (without the fields that a history does
// not have).
func (s *Server) remember(ev map[string]any) {
	h := map[string]any{}
	for k, v := range ev {
		if k != "channel" && k != "channel_type" {
			h[k] = v
		}
	}
	s.mu.Lock()
	s.history = append(s.history, h)
	s.mu.Unlock()
}

// SayMissed makes the user write a message that the client never gets as an event (the socket was closed): it is only in the
// history. It returns the ts.
func (s *Server) SayMissed(m Msg) string {
	ev, ts := m.event(s)
	s.remember(ev)
	return ts
}

// Say makes the user type a message and returns its ts and the envelope id.
func (s *Server) Say(m Msg) (ts, envelope string) {
	ev, ts := m.event(s)
	s.remember(ev)
	s.mu.Lock()
	s.nextEnv++
	eid := fmt.Sprintf("Ev%06d", s.nextEnv)
	s.mu.Unlock()
	env := s.send("events_api", map[string]any{"team_id": Team, "event_id": eid, "type": "event_callback", "event": ev}, "")
	return ts, env
}

// SayRaw sends an event payload as it is, with the envelope id given (to repeat an event).
func (s *Server) SayRaw(eventID, envelopeID string, ev map[string]any) {
	s.send("events_api", map[string]any{"team_id": Team, "event_id": eventID, "type": "event_callback", "event": ev}, envelopeID)
}

// Tap makes a user tap a button of a message.
func (s *Server) Tap(user, channel, messageTS, value, triggerID string) {
	s.send("interactive", map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user}, "team": map[string]any{"id": Team},
		"channel": map[string]any{"id": channel}, "message": map[string]any{"ts": messageTS},
		"container":    map[string]any{"channel_id": channel, "message_ts": messageTS},
		"actions":      []map[string]any{{"type": "button", "action_id": "pb0_0", "value": value}},
		"trigger_id":   triggerID,
		"response_url": s.HTTP.URL + "/hooks/response/" + triggerID,
	}, "")
}

// Slash makes a user type a slash command; it returns the envelope id (see AckPayload).
func (s *Server) Slash(user, channel, command, text, triggerID string) string {
	return s.send("slash_commands", map[string]any{
		"command": command, "text": text, "user_id": user, "channel_id": channel, "team_id": Team, "trigger_id": triggerID,
		"response_url": s.HTTP.URL + "/hooks/response/" + triggerID,
	}, "")
}

// Quote escapes a value for a query string, for tests that build an address.
func Quote(s string) string { return url.QueryEscape(s) }
