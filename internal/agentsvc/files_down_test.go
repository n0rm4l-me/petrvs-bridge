package agentsvc

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// fileExec records what is typed into the session and can be made to fail.
type fileExec struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (e *fileExec) SendText(_ context.Context, s string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.texts = append(e.texts, s)
	return nil
}
func (e *fileExec) SendKey(context.Context, string) error { return nil }
func (e *fileExec) typed() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.texts...)
}

// dclock is a clock the tests move.
type dclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *dclock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *dclock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// downRig is an agent with a session whose other end the test holds, so that it can read what the agent sent.
type downRig struct {
	t     *testing.T
	a     *Agent
	ex    *fileExec
	clock *dclock
	inbox string
	mem   *transport.Mem
	hub   *ratchet.Ratchet
	sid   [8]byte
	names wire.Names
	read  int
}

func newDownRig(t *testing.T, mutate func(*Config)) *downRig {
	t.Helper()
	id, _ := identity.Generate("laptop")
	hubID, _ := identity.Generate("hub")
	mem := transport.NewMem()
	names := wire.DefaultNames()
	mem.Bind(names.ToHub(), names.HubSub())
	r := &downRig{t: t, ex: &fileExec{}, clock: &dclock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}, inbox: filepath.Join(t.TempDir(), "inbox"), mem: mem, names: names}
	cfg := Config{Identity: id, Hub: hubID.Public(), Transport: mem, Names: names, Exec: r.ex, Now: r.clock.Now, ReorderWindow: 40 * time.Millisecond,
		Ext: Extras{InboxDir: r.inbox}}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sk [32]byte
	rand.Read(sk[:])
	peer, _ := ecdh.X25519().GenerateKey(rand.Reader)
	init, err := ratchet.NewInitiator(sk, peer.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	r.sid = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	a.sess = &session{sid: r.sid, r: init}
	r.hub = ratchet.NewResponder(sk, peer)
	r.a = a
	a.down.wait, a.down.poll = 400*time.Millisecond, 10*time.Millisecond
	return r
}

// sent returns what the agent published to the hub since the last call.
func (r *downRig) sent() []*wire.Payload {
	r.t.Helper()
	all := r.mem.Published(r.names.ToHub())
	var out []*wire.Payload
	for _, m := range all[r.read:] {
		p, err := wire.Open(r.hub, "laptop", "hub", r.sid, m.Data, true)
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, p)
	}
	r.read = len(all)
	return out
}

func (r *downRig) part(transfer string, i int, chunk []byte) *wire.Payload {
	return &wire.Payload{ID: fmt.Sprintf("p-%s-%d", transfer, i), Type: wire.TypePart, Body: filetx.EncodePart(chunk),
		Meta: map[string]string{wire.MetaTransfer: transfer, wire.MetaIndex: strconv.Itoa(i)}}
}

// upload sends all of data as parts, last first, and returns the closing command.
func (r *downRig) upload(transfer, name, caption string, data []byte) *wire.Payload {
	chunks := filetx.Chunks(data)
	for i := len(chunks) - 1; i >= 0; i-- {
		r.a.onPart(context.Background(), r.part(transfer, i, chunks[i]))
	}
	return announce(transfer, name, caption, data, len(chunks))
}

func announce(transfer, name, caption string, data []byte, parts int) *wire.Payload {
	return &wire.Payload{ID: "file-" + transfer, Type: wire.TypeFile, Body: caption, Meta: map[string]string{
		wire.MetaTransfer: transfer, wire.MetaParts: strconv.Itoa(parts), wire.MetaName: name, wire.MetaSize: strconv.Itoa(len(data)),
		wire.MetaSHA: filetx.SHA(data), wire.MetaKind: filetx.Kind(name, int64(len(data)))}}
}

func userMsg(t *testing.T, err error) string {
	t.Helper()
	var ue *userError
	if !errors.As(err, &ue) {
		t.Fatalf("expected a refusal for the user, got %v", err)
	}
	return ue.msg
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

var savedLine = regexp.MustCompile(`^\[(photo|file) from the chat, saved at (/\S+) \(([^,]+), ([^)]+)\)\]( .*)?$`)

func TestAFileIsAssembledSavedAndTheSessionIsToldWhereItIs(t *testing.T) {
	r := newDownRig(t, nil)
	data := payload(3*wire.MaxChunk + 100) // four parts, arriving last first
	cmd := r.upload("aabbccddeeff", "report.pdf", "please read this", data)
	if held, _ := r.a.down.asm.Held(); held != 1 {
		t.Fatalf("%d transfers held", held)
	}
	acks := r.sent()
	if len(acks) != 4 {
		t.Fatalf("one answer per stored part, got %d", len(acks))
	}
	for i, p := range acks {
		if p.Type != wire.TypeDone || p.Body != fmt.Sprintf("p-aabbccddeeff-%d", 3-i) {
			t.Fatalf("the answer must name the part: %+v", p)
		}
	}
	if err := r.a.executeFile(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	typed := r.ex.typed()
	if len(typed) != 1 {
		t.Fatalf("typed %q", typed)
	}
	m := savedLine.FindStringSubmatch(typed[0])
	if m == nil || m[1] != "file" || m[3] != "pdf" || m[4] != "384 KB" || m[5] != " please read this" {
		t.Fatalf("%q", typed[0])
	}
	path := m[2]
	if !filepath.IsAbs(path) || filepath.Dir(path) != evalDir(t, r.inbox) || !strings.HasSuffix(path, "-aabbcc-report.pdf") || !strings.HasPrefix(filepath.Base(path), "20261007-120000-") {
		t.Fatalf("%s", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the saved file differs (%v)", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(r.inbox); fi.Mode().Perm() != 0o700 {
		t.Fatalf("inbox mode %v", fi.Mode())
	}
	if held, bytes := r.a.down.asm.Held(); held != 0 || bytes != 0 {
		t.Fatalf("the transfer was not forgotten: %d, %d", held, bytes)
	}
	if tmuxctl.ValidateText(typed[0]) != nil {
		t.Fatal("the line cannot be typed")
	}
}

func evalDir(t *testing.T, d string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPhotoIsAnnouncedAsAPhoto(t *testing.T) {
	r := newDownRig(t, nil)
	cmd := r.upload("112233445566", "photo-20261007-115958.jpg", "", payload(48*1024))
	if err := r.a.executeFile(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	m := savedLine.FindStringSubmatch(r.ex.typed()[0])
	if m == nil || m[1] != "photo" || m[3] != "jpg" || m[4] != "48 KB" || m[5] != "" {
		t.Fatalf("%q", r.ex.typed())
	}
}

func TestHostileFileNamesCannotLeaveTheInboxOrBreakTheLine(t *testing.T) {
	for name, want := range map[string]string{
		"../../x":                    "x",
		"..\\..\\evil.sh":            "evil.sh",
		"/etc/passwd":                "passwd",
		"a\x00b\nc\x1b[31m.txt":      "a_b_c__31m.txt",
		".bashrc":                    "bashrc",
		"my photo (1).jpg":           "my_photo__1_.jpg",
		"semi;colon&pipe|`$(x)`.txt": "semi_colon_pipe____x__.txt",
		"":                           "file",
		"....":                       "file",
		strings.Repeat("n", 500):     strings.Repeat("n", 100),
	} {
		r := newDownRig(t, nil)
		cmd := r.upload("abcdef123456", name, "", payload(10))
		if err := r.a.executeFile(context.Background(), cmd); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		typed := r.ex.typed()[0]
		m := savedLine.FindStringSubmatch(typed)
		if m == nil {
			t.Fatalf("%q: %q", name, typed)
		}
		if filepath.Dir(m[2]) != evalDir(t, r.inbox) {
			t.Fatalf("%q escaped the inbox: %s", name, m[2])
		}
		if base := filepath.Base(m[2]); !strings.HasSuffix(base, "-abcdef-"+want) {
			t.Fatalf("%q became %q, want suffix %q", name, base, want)
		}
		if tmuxctl.ValidateText(typed) != nil || strings.ContainsAny(typed, "\n\r\x00\x1b") {
			t.Fatalf("%q", typed)
		}
		if ents := savedFiles(r.inbox); len(ents) != 1 {
			t.Fatalf("%q: %d files in the inbox", name, len(ents))
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(r.inbox), "x")); err == nil {
			t.Fatalf("%q: a file appeared beside the inbox", name)
		}
	}
}

func TestAHostileCaptionIsOneCleanShortLine(t *testing.T) {
	long := strings.Repeat("abcdefghij", 200) // 2000 characters
	for name, c := range map[string]struct{ in, want string }{
		"newlines":       {"line one\nline two\r\nline three", "line one line two line three"},
		"tabs and nul":   {"a\tb\x00c", "a b c"},
		"escape":         {"\x1b[2J\x1b]0;title\x07hi", "[2J ]0;title hi"},
		"separators":     {"x\u2028y\u2029z\u0085w", "x y z w"},
		"bidi":           {"abc\u202edef\u2066g\u200bh", "abcdefgh"},
		"leading spaces": {"  \n\t hello  world \n ", "hello world"},
		"invalid utf8":   {"ok\xff\xfeok", "ok ok"},
		"emoji family":   {"\U0001F468\u200d\U0001F469\u200d\U0001F467 ok", "\U0001F468\u200d\U0001F469\u200d\U0001F467 ok"},
		"only junk":      {"\n\x00\t", ""},
	} {
		if got := cleanCaption(c.in); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if got := cleanCaption(long); len([]rune(got)) != 500 || got != long[:500] {
		t.Errorf("a long caption kept %d characters", len([]rune(got)))
	}
	if got := cleanCaption(strings.Repeat("é ", 1000)); len([]rune(got)) > 500 || strings.HasSuffix(got, " ") || !strings.HasSuffix(got, "é") {
		t.Errorf("%d characters, ends %q", len([]rune(got)), got[len(got)-4:])
	}
	// through the whole command
	r := newDownRig(t, nil)
	cmd := r.upload("abcdef123456", "a.txt", "/clear\n!rm -rf ~\n\x1b[31m"+long+strings.Repeat("ÿ", 5000), payload(10))
	if err := r.a.executeFile(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	line := r.ex.typed()[0]
	if !strings.HasPrefix(line, "[file from the chat, saved at ") || strings.ContainsAny(line, "\n\r\x1b") || tmuxctl.ValidateText(line) != nil {
		t.Fatalf("%q", line)
	}
	caption := line[strings.Index(line, ")] ")+3:]
	if len([]rune(caption)) > 500 || !strings.HasPrefix(caption, "/clear !rm -rf ~ [31m") {
		t.Fatalf("%q", caption)
	}
}

func TestAnEmptyCaptionLeavesNothingAfterTheBracket(t *testing.T) {
	line := fileLine(false, "/in/x.txt", "x.txt", 5, " \n ")
	if line != "[file from the chat, saved at /in/x.txt (txt, 5 B)]" {
		t.Fatalf("%q", line)
	}
	if got := fileLine(true, "/in/My Dir/x", "x", 1500, ""); got != `[photo from the chat, saved at "/in/My Dir/x" (no extension, 1.5 KB)]` {
		t.Fatalf("%q", got)
	}
	if got := fileLine(false, "/in/x", "x.ta;r", 3<<20, ""); got != `[file from the chat, saved at /in/x (unknown type, 3.0 MB)]` {
		t.Fatalf("%q", got)
	}
}

func TestPartsThatNeverFinishGiveAnErrorForTheUserAndFreeTheMemory(t *testing.T) {
	r := newDownRig(t, nil)
	data := payload(3 * wire.MaxChunk)
	chunks := filetx.Chunks(data)
	r.a.onPart(context.Background(), r.part("aabbccddeeff", 0, chunks[0]))
	r.a.onPart(context.Background(), r.part("aabbccddeeff", 2, chunks[2]))
	start := time.Now()
	err := r.a.executeFile(context.Background(), announce("aabbccddeeff", "f.bin", "", data, 3))
	if msg := userMsg(t, err); !strings.Contains(msg, "did not arrive whole (2 of 3") {
		t.Fatal(msg)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 5*time.Second {
		t.Fatalf("waited %v, want about the configured 400ms", took)
	}
	if held, _ := r.a.down.asm.Held(); held != 0 {
		t.Fatal("the unfinished transfer is still held")
	}
	if len(r.ex.typed()) != 0 {
		t.Fatal("the session was told about a file that is not there")
	}
	if ents, _ := os.ReadDir(r.inbox); len(ents) != 0 {
		t.Fatal("a partial file was saved")
	}
}

func TestACommandWithNoPartsAtAllIsAnErrorToo(t *testing.T) {
	r := newDownRig(t, nil)
	msg := userMsg(t, r.a.executeFile(context.Background(), announce("aabbccddeeff", "f.bin", "", payload(10), 1)))
	if !strings.Contains(msg, "did not arrive") {
		t.Fatal(msg)
	}
}

func TestAPartThatComesWhileTheCommandWaitsIsUsed(t *testing.T) {
	r := newDownRig(t, nil)
	data := payload(2 * wire.MaxChunk)
	chunks := filetx.Chunks(data)
	r.a.onPart(context.Background(), r.part("aabbccddeeff", 0, chunks[0]))
	go func() {
		time.Sleep(80 * time.Millisecond)
		r.a.onPart(context.Background(), r.part("aabbccddeeff", 1, chunks[1]))
	}()
	if err := r.a.executeFile(context.Background(), announce("aabbccddeeff", "f.bin", "", data, 2)); err != nil {
		t.Fatal(err)
	}
	if len(r.ex.typed()) != 1 {
		t.Fatal("not typed")
	}
}

func TestADamagedOrMisdescribedFileIsRefused(t *testing.T) {
	data := payload(300)
	for name, c := range map[string]struct {
		change func(*wire.Payload)
		want   string
	}{
		"wrong hash":     {func(p *wire.Payload) { p.Meta[wire.MetaSHA] = filetx.SHA([]byte("other")) }, "damaged"},
		"false size":     {func(p *wire.Payload) { p.Meta[wire.MetaSize] = "299" }, "damaged"},
		"huge size":      {func(p *wire.Payload) { p.Meta[wire.MetaSize] = "99999999999" }, "larger"},
		"false parts":    {func(p *wire.Payload) { p.Meta[wire.MetaParts] = "1000000" }, "larger"},
		"zero size":      {func(p *wire.Payload) { p.Meta[wire.MetaSize] = "0" }, "not valid"},
		"negative parts": {func(p *wire.Payload) { p.Meta[wire.MetaParts] = "-1" }, "not valid"},
		"no hash":        {func(p *wire.Payload) { delete(p.Meta, wire.MetaSHA) }, "not valid"},
		"bad transfer":   {func(p *wire.Payload) { p.Meta[wire.MetaTransfer] = "../../etc" }, "not valid"},
		"not a number":   {func(p *wire.Payload) { p.Meta[wire.MetaSize] = "lots" }, "not valid"},
	} {
		r := newDownRig(t, nil)
		cmd := r.upload("aabbccddeeff", "f.bin", "", data)
		c.change(cmd)
		msg := userMsg(t, r.a.executeFile(context.Background(), cmd))
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q", name, msg)
		}
		if len(r.ex.typed()) != 0 {
			t.Errorf("%s: the session was told", name)
		}
		if ents, _ := os.ReadDir(r.inbox); len(ents) != 0 {
			t.Errorf("%s: a file was saved", name)
		}
	}
}

func TestWithoutAnInboxTheAgentTakesNoFiles(t *testing.T) {
	r := newDownRig(t, func(c *Config) { c.Ext.InboxDir = "" })
	r.a.onPart(context.Background(), r.part("aabbccddeeff", 0, payload(10)))
	if held, _ := r.a.down.asm.Held(); held != 0 {
		t.Fatal("a part was kept although files are off")
	}
	for _, p := range r.sent() {
		if p.Type == wire.TypeDone {
			t.Fatal("a part was confirmed although files are off")
		}
	}
	msg := userMsg(t, r.a.executeFile(context.Background(), announce("aabbccddeeff", "f.bin", "", payload(10), 1)))
	if !strings.Contains(msg, "no files") {
		t.Fatal(msg)
	}
}

func TestALockedMachineKeepsNoPartsAndSaysSoOnce(t *testing.T) {
	r := newDownRig(t, func(c *Config) { c.LockFile = filepath.Join(t.TempDir(), "lock") })
	r.a.lock(context.Background())
	r.sent()
	chunks := filetx.Chunks(payload(4 * wire.MaxChunk))
	for i, c := range chunks {
		r.a.onPart(context.Background(), r.part("aabbccddeeff", i, c))
	}
	if held, _ := r.a.down.asm.Held(); held != 0 {
		t.Fatal("a locked machine kept parts")
	}
	var dropped int
	for _, p := range r.sent() {
		switch p.Type {
		case wire.TypeDone:
			t.Fatal("a locked machine confirmed a part")
		case wire.TypeDropped:
			dropped++
			if !strings.Contains(p.Body, "locked") {
				t.Fatalf("%q", p.Body)
			}
		}
	}
	if dropped != 1 {
		t.Fatalf("the refusal was said %d times, want once per transfer", dropped)
	}
	if r.a.Stats().Refused != 1 {
		t.Fatalf("refused %d", r.a.Stats().Refused)
	}
}

func TestALockdownWhileTheCommandWaitsStopsTheFile(t *testing.T) {
	r := newDownRig(t, func(c *Config) { c.LockFile = filepath.Join(t.TempDir(), "lock") })
	data := payload(2 * wire.MaxChunk)
	chunks := filetx.Chunks(data)
	r.a.onPart(context.Background(), r.part("aabbccddeeff", 0, chunks[0]))
	go func() {
		time.Sleep(60 * time.Millisecond)
		r.a.onPart(context.Background(), r.part("aabbccddeeff", 1, chunks[1]))
		r.a.lock(context.Background())
	}()
	msg := userMsg(t, r.a.executeFile(context.Background(), announce("aabbccddeeff", "f.bin", "", data, 2)))
	if !strings.Contains(msg, "locked") || len(r.ex.typed()) != 0 {
		t.Fatalf("%q, typed %q", msg, r.ex.typed())
	}
	if ents, _ := os.ReadDir(r.inbox); len(ents) != 0 {
		t.Fatal("a file was saved on a locked machine")
	}
}

func TestATooLargeFileIsRefusedOnceAndItsOtherPartsAreDroppedQuietly(t *testing.T) {
	r := newDownRig(t, func(c *Config) { c.Ext.MaxFileBytes = 2*wire.MaxChunk + 10 })
	chunks := filetx.Chunks(payload(6 * wire.MaxChunk))
	for i, c := range chunks {
		r.a.onPart(context.Background(), r.part("aabbccddeeff", i, c))
	}
	var done, dropped int
	for _, p := range r.sent() {
		switch p.Type {
		case wire.TypeDone:
			done++
		case wire.TypeDropped:
			dropped++
			if !strings.Contains(p.Body, "larger") {
				t.Fatalf("%q", p.Body)
			}
		}
	}
	if done != 2 || dropped != 1 {
		t.Fatalf("%d parts confirmed and %d refusals, want 2 and 1", done, dropped)
	}
	if held, _ := r.a.down.asm.Held(); held != 0 {
		t.Fatal("the refused transfer is still held")
	}
}

func TestBadPartsAreNotConfirmed(t *testing.T) {
	r := newDownRig(t, nil)
	ctx := context.Background()
	good := r.part("aabbccddeeff", 0, payload(10))
	for name, p := range map[string]*wire.Payload{
		"no transfer":    {ID: "1", Type: wire.TypePart, Body: good.Body, Meta: map[string]string{wire.MetaIndex: "0"}},
		"bad transfer":   {ID: "2", Type: wire.TypePart, Body: good.Body, Meta: map[string]string{wire.MetaTransfer: "../x", wire.MetaIndex: "0"}},
		"short transfer": {ID: "3", Type: wire.TypePart, Body: good.Body, Meta: map[string]string{wire.MetaTransfer: "abc", wire.MetaIndex: "0"}},
		"no index":       {ID: "4", Type: wire.TypePart, Body: good.Body, Meta: map[string]string{wire.MetaTransfer: "aabbccddeeff"}},
		"negative index": {ID: "5", Type: wire.TypePart, Body: good.Body, Meta: map[string]string{wire.MetaTransfer: "aabbccddeeff", wire.MetaIndex: "-1"}},
		"huge index":     {ID: "6", Type: wire.TypePart, Body: good.Body, Meta: map[string]string{wire.MetaTransfer: "aabbccddeeff", wire.MetaIndex: "99999999"}},
		"not base64":     {ID: "7", Type: wire.TypePart, Body: "!!!", Meta: good.Meta},
		"empty":          {ID: "8", Type: wire.TypePart, Body: "", Meta: good.Meta},
		"too long":       {ID: "9", Type: wire.TypePart, Body: filetx.EncodePart(make([]byte, wire.MaxChunk+1)), Meta: good.Meta},
	} {
		r.a.onPart(ctx, p)
		for _, s := range r.sent() {
			if s.Type == wire.TypeDone {
				t.Errorf("%s: confirmed", name)
			}
		}
	}
	// a part whose index is already taken by different bytes is refused, not merged
	r2 := newDownRig(t, nil)
	r2.a.onPart(ctx, r2.part("aabbccddeeff", 0, payload(10)))
	r2.sent()
	r2.a.onPart(ctx, r2.part("aabbccddeeff", 0, payload(11)))
	for _, s := range r2.sent() {
		if s.Type == wire.TypeDone {
			t.Error("a different part under the same index was confirmed")
		}
	}
}

func TestTwoTransfersAtOnceDoNotMixTheirParts(t *testing.T) {
	r := newDownRig(t, nil)
	a, b := payload(2*wire.MaxChunk+5), bytes.Repeat([]byte{9}, wire.MaxChunk+7)
	ca, cb := filetx.Chunks(a), filetx.Chunks(b)
	ctx := context.Background()
	r.a.onPart(ctx, r.part("aaaaaaaaaaaa", 2, ca[2]))
	r.a.onPart(ctx, r.part("bbbbbbbbbbbb", 1, cb[1]))
	r.a.onPart(ctx, r.part("aaaaaaaaaaaa", 0, ca[0]))
	r.a.onPart(ctx, r.part("bbbbbbbbbbbb", 0, cb[0]))
	r.a.onPart(ctx, r.part("aaaaaaaaaaaa", 1, ca[1]))
	if err := r.a.executeFile(ctx, announce("bbbbbbbbbbbb", "b.bin", "", b, 2)); err != nil {
		t.Fatal(err)
	}
	if err := r.a.executeFile(ctx, announce("aaaaaaaaaaaa", "a.bin", "", a, 3)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		suffix string
		want   []byte
	}{{"-bbbbbb-b.bin", b}, {"-aaaaaa-a.bin", a}} {
		ents, _ := filepath.Glob(filepath.Join(r.inbox, "*"+c.suffix))
		if len(ents) != 1 {
			t.Fatalf("%s: %v", c.suffix, ents)
		}
		if got, _ := os.ReadFile(ents[0]); !bytes.Equal(got, c.want) {
			t.Fatalf("%s holds the wrong bytes", c.suffix)
		}
	}
}

func TestAbandonedTransfersMakeRoomForNewOnes(t *testing.T) {
	r := newDownRig(t, nil)
	ctx := context.Background()
	for i := 0; i < 16; i++ { // as many as the assembler takes
		r.a.onPart(ctx, r.part(fmt.Sprintf("abandoned%03d", i), 0, payload(10))) // the first part of a file that never finishes
	}
	r.sent()
	r.a.onPart(ctx, r.part("eeeeeeeeeeee", 0, payload(10)))
	got := r.sent()
	if len(got) != 1 || got[0].Type != wire.TypeDropped {
		t.Fatalf("one more transfer was taken while all were fresh: %+v", got)
	}
	r.clock.Advance(idleTransfer + time.Second)
	r.a.onPart(ctx, r.part("ffffffffffff", 0, payload(10)))
	got = r.sent()
	if len(got) != 1 || got[0].Type != wire.TypeDone {
		t.Fatalf("the abandoned transfers were not dropped: %+v", got)
	}
	if held, _ := r.a.down.asm.Held(); held != 1 {
		t.Fatalf("%d held", held)
	}
}

// savedFiles lists what the inbox holds besides the .gitignore that a new inbox gets.
func savedFiles(dir string) []os.DirEntry {
	var out []os.DirEntry
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != ".gitignore" {
			out = append(out, e)
		}
	}
	return out
}

func TestAFileThatCannotBeTypedIsReportedAndStaysInTheInbox(t *testing.T) {
	r := newDownRig(t, nil)
	r.ex.err = errors.New("tmux is gone")
	msg := userMsg(t, r.a.executeFile(context.Background(), r.upload("aabbccddeeff", "f.bin", "cap", payload(10))))
	if !strings.Contains(msg, "saved in the inbox") {
		t.Fatal(msg)
	}
	if ents := savedFiles(r.inbox); len(ents) != 1 {
		t.Fatal("the file was not kept")
	}
}

func TestTheSameTransferTwiceNeverOverwrites(t *testing.T) {
	r := newDownRig(t, nil)
	ctx := context.Background()
	data := payload(10)
	if err := r.a.executeFile(ctx, r.upload("aabbccddeeff", "f.bin", "", data)); err != nil {
		t.Fatal(err)
	}
	ents := savedFiles(r.inbox)
	path := filepath.Join(r.inbox, ents[0].Name())
	os.WriteFile(path, []byte("edited by the user"), 0o600)
	if err := r.a.executeFile(ctx, r.upload("aabbccddeeff", "f.bin", "", data)); err == nil {
		t.Fatal("a second file with the same name was saved")
	}
	if got, _ := os.ReadFile(path); string(got) != "edited by the user" {
		t.Fatal("the first file was overwritten")
	}
}

func TestALinkInTheFilesPlaceIsNeverFollowed(t *testing.T) {
	r := newDownRig(t, nil)
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("precious"), 0o600)
	if err := os.MkdirAll(r.inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	// the name is a function of the clock, the transfer and the file name: plant the link where the file will go
	name := "20261007-120000-aabbcc-f.bin"
	if err := os.Symlink(victim, filepath.Join(r.inbox, name)); err != nil {
		t.Fatal(err)
	}
	if err := r.a.executeFile(context.Background(), r.upload("aabbccddeeff", "f.bin", "", payload(10))); err == nil {
		t.Fatal("saved through a link")
	}
	if got, _ := os.ReadFile(victim); string(got) != "precious" {
		t.Fatal("the link was followed and the target overwritten")
	}
	if len(r.ex.typed()) != 0 {
		t.Fatal("the session was told about a file that was not saved")
	}
}

func TestAnInboxThatIsALinkIsRefused(t *testing.T) {
	elsewhere := t.TempDir()
	r := newDownRig(t, nil)
	if err := os.Symlink(elsewhere, r.inbox); err != nil {
		t.Fatal(err)
	}
	msg := userMsg(t, r.a.executeFile(context.Background(), r.upload("aabbccddeeff", "f.bin", "", payload(10))))
	if !strings.Contains(msg, "link") {
		t.Fatal(msg)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatal("a file was written through the link")
	}
}

func TestAnInboxBelowALinkedDirectoryWorksAndTheReportedPathIsReal(t *testing.T) {
	real := t.TempDir()
	linked := filepath.Join(t.TempDir(), "via")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}
	r := newDownRig(t, func(c *Config) { c.Ext.InboxDir = filepath.Join(linked, "inbox") })
	if err := r.a.executeFile(context.Background(), r.upload("aabbccddeeff", "f.bin", "", payload(10))); err != nil {
		t.Fatal(err)
	}
	path := savedLine.FindStringSubmatch(r.ex.typed()[0])[2]
	if strings.Contains(path, "/via/") || !strings.HasPrefix(path, evalDir(t, real)) {
		t.Fatalf("%s", path)
	}
}

func TestAnInboxThatIsAFileIsRefused(t *testing.T) {
	r := newDownRig(t, nil)
	os.WriteFile(r.inbox, []byte("x"), 0o600)
	if err := r.a.executeFile(context.Background(), r.upload("aabbccddeeff", "f.bin", "", payload(10))); err == nil {
		t.Fatal("saved into a file")
	}
}

func TestAnInboxThatCannotBeWrittenLeavesNoPartialFileAndTheUserIsTold(t *testing.T) {
	// A directory that cannot take a new file (the way a full disk would fail): nothing is left behind, and the user is told.
	r := newDownRig(t, nil)
	if err := os.MkdirAll(r.inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(r.inbox, 0o500)
	defer os.Chmod(r.inbox, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	msg := userMsg(t, r.a.executeFile(context.Background(), r.upload("aabbccddeeff", "f.bin", "", payload(10))))
	if !strings.Contains(msg, "could not save") {
		t.Fatal(msg)
	}
	os.Chmod(r.inbox, 0o700)
	if ents, _ := os.ReadDir(r.inbox); len(ents) != 0 {
		t.Fatal("a file was left behind")
	}
}

// ---- the inbox is kept tidy ----

func plant(t *testing.T, dir, name string, age time.Duration, now time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := now.Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestTidyingRemovesOldFilesOfTheBridgeAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	day := 24 * time.Hour
	oldMine := plant(t, dir, "20260101-000000-aaaaaa-old.txt", 15*day, now)
	freshMine := plant(t, dir, "20261001-000000-bbbbbb-new.txt", 13*day, now)
	oldOther := plant(t, dir, "notes.txt", 400*day, now) // the user's own file in the same directory
	oldAlmost := plant(t, dir, "20260101-000000-cc-short.txt", 400*day, now)
	outside := plant(t, t.TempDir(), "20260101-000000-dddddd-target.txt", 400*day, now)
	link := filepath.Join(dir, "20260101-000000-eeeeee-link.txt")
	os.Symlink(outside, link)
	sub := filepath.Join(dir, "20260101-000000-ffffff-dir")
	os.Mkdir(sub, 0o700)
	os.Chtimes(sub, now.Add(-400*day), now.Add(-400*day))
	if err := tidyInbox(dir, now); err != nil {
		t.Fatal(err)
	}
	if exists(oldMine) {
		t.Error("an old file of the bridge was kept")
	}
	for name, p := range map[string]string{"fresh": freshMine, "other": oldOther, "short name": oldAlmost, "link target": outside, "link": link, "directory": sub} {
		if !exists(p) {
			t.Errorf("%s was removed", name)
		}
	}
}

func TestTidyingKeepsTheNewestTwoHundred(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var paths []string
	for i := 0; i < 230; i++ {
		paths = append(paths, plant(t, dir, fmt.Sprintf("20261007-120000-%06d-f.bin", i), time.Duration(230-i)*time.Minute, now))
	}
	if err := tidyInbox(dir, now); err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		if want := i >= 30; exists(p) != want {
			t.Fatalf("file %d: exists %v, want %v", i, exists(p), want)
		}
	}
}

func TestTidyingDoesNotFollowALinkedInbox(t *testing.T) {
	target := t.TempDir()
	victim := plant(t, target, "20260101-000000-aaaaaa-x.txt", 400*24*time.Hour, time.Now())
	link := filepath.Join(t.TempDir(), "inbox")
	os.Symlink(target, link)
	if err := tidyInbox(link, time.Now()); err == nil || !exists(victim) {
		t.Fatalf("tidied through a link (%v)", err)
	}
}

func TestTheInboxIsTidiedAtMostOncePerHourOrEveryHundredFiles(t *testing.T) {
	r := newDownRig(t, nil)
	ctx := context.Background()
	save := func(id string) {
		if err := r.a.executeFile(ctx, r.upload(id, "f.bin", "", payload(5))); err != nil {
			t.Fatal(err)
		}
	}
	save("000001000000")
	old := plant(t, r.inbox, "20200101-000000-aaaaaa-old.txt", 400*24*time.Hour, r.clock.Now())
	save("000002000000")
	if !exists(old) {
		t.Fatal("the inbox was tidied twice within an hour")
	}
	r.clock.Advance(time.Hour + time.Second)
	save("000003000000")
	if exists(old) {
		t.Fatal("the inbox was not tidied after an hour")
	}
	// a burst of files does not wait for the hour
	old2 := plant(t, r.inbox, "20200101-000000-bbbbbb-old.txt", 400*24*time.Hour, r.clock.Now())
	for i := 0; i < tidyAfterSaves-1; i++ {
		save(fmt.Sprintf("%06d000000", 10+i))
	}
	if !exists(old2) {
		t.Fatal("the inbox was tidied before the hundredth file")
	}
	save("999999000000")
	if exists(old2) {
		t.Fatalf("the inbox was not tidied after %d files", tidyAfterSaves)
	}
}

func TestNamesAndSizesAreShownPlainly(t *testing.T) {
	for in, want := range map[int]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 15360: "15 KB", 1 << 20: "1.0 MB", 20 << 20: "20 MB"} {
		if got := humanSize(in); got != want {
			t.Errorf("%d: %q, want %q", in, got, want)
		}
	}
	if inboxName("a b(c)+d.é_-") != "a_b_c__d.é_-" {
		t.Errorf("%q", inboxName("a b(c)+d.é_-"))
	}
}
