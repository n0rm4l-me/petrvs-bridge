package hubsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// These tests call the two steps of the file transfer directly, with the payloads an agent would send and with a
// clock of their own: what matters here is what the hub does with a part or a closing item, however odd.

type upFixture struct {
	h   *Hub
	tg  *telegram.Fake
	now time.Time
	mu  sync.Mutex
}

func (f *upFixture) clock() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *upFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func newUpFixture(t *testing.T, mutate func(*Config)) *upFixture {
	t.Helper()
	c := base(t)
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &upFixture{tg: c.Telegram.(*telegram.Fake), now: time.Unix(1_800_000_000, 0)}
	c.Now = f.clock
	if mutate != nil {
		mutate(&c)
	}
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	return f
}

func (f *upFixture) texts() []string {
	var out []string
	for _, s := range f.tg.Sent() {
		out = append(out, s.Text)
	}
	return out
}

func (f *upFixture) said(sub string) int {
	n := 0
	for _, s := range f.texts() {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (f *upFixture) held() (int, int64) { return f.h.up.asm.Held() }

// upRandom returns n bytes that no two tests share.
func upRandom(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func upPartPayload(id, t string, index int, data []byte) *wire.Payload {
	return &wire.Payload{ID: id, Type: wire.TypePart, Body: filetx.EncodePart(data), Meta: map[string]string{wire.MetaTransfer: t, wire.MetaIndex: strconv.Itoa(index)}}
}

func upFilePayload(id, t string, parts int, data []byte, name, caption string) *wire.Payload {
	return &wire.Payload{ID: id, Type: wire.TypeFile, Body: caption, Meta: map[string]string{
		wire.MetaTransfer: t, wire.MetaParts: strconv.Itoa(parts), wire.MetaName: name, wire.MetaSize: strconv.Itoa(len(data)),
		wire.MetaSHA: filetx.SHA(data), wire.MetaKind: filetx.Kind(name, int64(len(data))),
	}}
}

// send cuts data as the agent does and hands the parts and the closing item to the hub; it returns what the hub answered
// for the closing item.
func (f *upFixture) send(t *testing.T, agent, tr string, data []byte, name, caption string) bool {
	t.Helper()
	chunks := filetx.Chunks(data)
	for i, c := range chunks {
		if !f.h.upPart(agent, upPartPayload(fmt.Sprintf("%s-p%d", tr, i), tr, i, c)) {
			t.Fatalf("part %d of %s was not taken", i, tr)
		}
	}
	return f.h.upFile(context.Background(), agent, upFilePayload(tr+"-f", tr, len(chunks), data, name, caption))
}

func TestAFileInThreePartsIsPutTogetherAndUploadedOnce(t *testing.T) {
	f := newUpFixture(t, nil)
	data := upRandom(300_000)
	chunks := filetx.Chunks(data)
	if len(chunks) != 3 {
		t.Fatalf("%d chunks", len(chunks))
	}
	// out of order, and the second part twice
	for _, i := range []int{2, 0, 1, 1} {
		if !f.h.upPart("laptop", upPartPayload(fmt.Sprintf("p%d-%d", i, rand.Int()), "aaa111", i, chunks[i])) {
			t.Fatalf("part %d", i)
		}
	}
	if n, b := f.held(); n != 1 || b != int64(len(data)) {
		t.Fatalf("held %d transfers, %d bytes: the duplicate was counted", n, b)
	}
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f1", "aaa111", 3, data, "report.pdf", "the report")) {
		t.Fatal("the closing item was not dealt with")
	}
	ups := f.tg.Uploads()
	if len(ups) != 1 || !bytes.Equal(ups[0].Data, data) || ups[0].Name != "report.pdf" || ups[0].Caption != "the report" || ups[0].Photo {
		t.Fatalf("%d uploads, first %+v", len(ups), ups)
	}
	if n, b := f.held(); n != 0 || b != 0 {
		t.Fatalf("still holding %d transfers, %d bytes", n, b)
	}
	// the closing item again under another id (a resend after a lost receipt would carry the same id and never get here)
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f2", "aaa111", 3, data, "report.pdf", "the report")) || len(f.tg.Uploads()) != 1 {
		t.Fatalf("a second closing item for a file in the chat uploaded it again: %d", len(f.tg.Uploads()))
	}
	if f.said("did not arrive") != 0 {
		t.Fatalf("the user was told of a problem: %q", f.texts())
	}
}

func TestCaptionAndTagInTheChat(t *testing.T) {
	c := func(c *Config) {
		b, _ := identity.Generate("desk")
		c.Agents = append(c.Agents, b.Public())
	}
	f := newUpFixture(t, c)
	f.send(t, "desk", "t0000000001", []byte("one"), "a.txt", "")
	f.send(t, "desk", "t0000000002", []byte("two"), "b.txt", "with words")
	f.send(t, "laptop", "t0000000003", []byte("three"), "c.txt", "  \n")
	f.send(t, "laptop", "t0000000004", []byte("four"), "d.txt", strings.Repeat("é", 2000))
	got := f.tg.Uploads()
	if len(got) != 4 {
		t.Fatalf("%d uploads", len(got))
	}
	if got[0].Caption != "[desk] a.txt" || got[1].Caption != "[desk] with words" || got[2].Caption != "[laptop] c.txt" {
		t.Fatalf("%q, %q, %q", got[0].Caption, got[1].Caption, got[2].Caption)
	}
	if n := len(got[3].Caption); n > len("[laptop] ")+1000 || !strings.HasPrefix(got[3].Caption, "[laptop] é") {
		t.Fatalf("a caption of %d bytes", n)
	}
}

// The agent says whether a file is a photo, but a photo is decided by the name and the size, which the hub can see.
func TestThePhotoFlagIsNotTakenOnTrust(t *testing.T) {
	f := newUpFixture(t, nil)
	png := []byte("\x89PNG....")
	f.send(t, "laptop", "t0000000001", png, "shot.png", "")
	notes := []byte("plain text")
	p := upFilePayload("f", "t0000000002", 1, notes, "notes.txt", "")
	p.Meta[wire.MetaKind] = "photo" // an agent that claims a text file is a picture
	f.h.upPart("laptop", upPartPayload("p", "t0000000002", 0, notes))
	f.h.upFile(context.Background(), "laptop", p)
	asDoc := []byte("\x89PNG....more")
	q := upFilePayload("g", "t0000000003", 1, asDoc, "shot2.png", "")
	q.Meta[wire.MetaKind] = "document" // and one that asks for a document
	f.h.upPart("laptop", upPartPayload("q", "t0000000003", 0, asDoc))
	f.h.upFile(context.Background(), "laptop", q)
	ups := f.tg.Uploads()
	if len(ups) != 3 || !ups[0].Photo || ups[1].Photo || ups[2].Photo {
		t.Fatalf("photo flags: %v %v %v", ups[0].Photo, ups[1].Photo, ups[2].Photo)
	}
}

func TestAPartThatCannotBeUsedIsDealtWithAndKeepsNothing(t *testing.T) {
	f := newUpFixture(t, nil)
	good := upPartPayload("g", "t0000000001", 0, []byte("data"))
	for name, mutate := range map[string]func(p *wire.Payload){
		"no transfer":        func(p *wire.Payload) { delete(p.Meta, wire.MetaTransfer) },
		"a path as transfer": func(p *wire.Payload) { p.Meta[wire.MetaTransfer] = "../../etc" },
		"a long transfer":    func(p *wire.Payload) { p.Meta[wire.MetaTransfer] = strings.Repeat("a", 33) },
		"a control byte":     func(p *wire.Payload) { p.Meta[wire.MetaTransfer] = "ab\ncd" },
		"no index":           func(p *wire.Payload) { delete(p.Meta, wire.MetaIndex) },
		"a negative index":   func(p *wire.Payload) { p.Meta[wire.MetaIndex] = "-1" },
		"a signed index":     func(p *wire.Payload) { p.Meta[wire.MetaIndex] = "+1" },
		"a huge index":       func(p *wire.Payload) { p.Meta[wire.MetaIndex] = "99999999999999999999" },
		"a text index":       func(p *wire.Payload) { p.Meta[wire.MetaIndex] = "one" },
		"an index too high":  func(p *wire.Payload) { p.Meta[wire.MetaIndex] = "100000" },
		"not base64":         func(p *wire.Payload) { p.Body = "!!!! not base64 !!!!" },
		"an empty body":      func(p *wire.Payload) { p.Body = "" },
		"a body too long":    func(p *wire.Payload) { p.Body = filetx.EncodePart(make([]byte, wire.MaxChunk+1)) },
	} {
		p := *good
		p.Meta = map[string]string{}
		for k, v := range good.Meta {
			p.Meta[k] = v
		}
		mutate(&p)
		if !f.h.upPart("laptop", &p) {
			t.Errorf("%s: left to be sent again, for ever", name)
		}
		if n, b := f.held(); n != 0 || b != 0 {
			t.Errorf("%s: the hub keeps %d transfers, %d bytes", name, n, b)
		}
	}
	if len(f.h.up.records) != 0 {
		t.Fatalf("records were made for parts that cannot be used: %d", len(f.h.up.records))
	}
}

func TestAConflictingCopyOfAPartChangesNothing(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("hello world")
	f.h.upPart("laptop", upPartPayload("a", "t0000000001", 0, data))
	if !f.h.upPart("laptop", upPartPayload("b", "t0000000001", 0, []byte("HELLO WORLD"))) {
		t.Fatal("the conflicting part was left to be resent")
	}
	// the first copy stands, so the file is as announced and is uploaded
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f", "t0000000001", 1, data, "a.txt", "")) || len(f.tg.Uploads()) != 1 || !bytes.Equal(f.tg.Uploads()[0].Data, data) {
		t.Fatalf("%d uploads", len(f.tg.Uploads()))
	}
}

func TestTooManyTransfersAtOnceWaitForTheirResend(t *testing.T) {
	f := newUpFixture(t, nil)
	for i := 1; i <= upLiveMax; i++ {
		if !f.h.upPart("laptop", upPartPayload("p", fmt.Sprintf("t%010d", i), 0, []byte("x"))) {
			t.Fatalf("transfer %d was not taken", i)
		}
	}
	over := fmt.Sprintf("t%010d", upLiveMax+1)
	if f.h.upPart("laptop", upPartPayload("p5", over, 0, []byte("x"))) {
		t.Fatal("a transfer past the limit was taken: the agent would not resend its part")
	}
	if n, _ := f.held(); n != upLiveMax {
		t.Fatalf("held %d transfers", n)
	}
	// parts of a transfer that is held are still taken
	if !f.h.upPart("laptop", upPartPayload("p6", "t0000000001", 1, []byte("y"))) {
		t.Fatal("a part of a held transfer was refused")
	}
	// after the time a stale one is forgotten, room again
	f.advance(upRecordTTL + time.Minute)
	if !f.h.upPart("laptop", upPartPayload("p7", over, 0, []byte("x"))) {
		t.Fatal("stale transfers were not forgotten")
	}
	if n, _ := f.held(); n != 1 {
		t.Fatalf("held %d transfers after the sweep", n)
	}
}

func TestATransferThatGrowsPastTheLimitIsDroppedAndReportedOnce(t *testing.T) {
	f := newUpFixture(t, func(c *Config) { c.MaxFileBytes = 300_000 })
	chunk := upRandom(wire.MaxChunk)
	for i := 0; i < 2; i++ {
		f.h.upPart("laptop", upPartPayload(fmt.Sprintf("p%d", i), "t0000000001", i, chunk))
	}
	if !f.h.upPart("laptop", upPartPayload("p2", "t0000000001", 2, chunk)) { // 393216 bytes: over the limit
		t.Fatal("the part that broke the limit was left to be resent")
	}
	if n, b := f.held(); n != 0 || b != 0 {
		t.Fatalf("the transfer is still held: %d, %d", n, b)
	}
	if !f.h.upPart("laptop", upPartPayload("p3", "t0000000001", 3, chunk)) {
		t.Fatal("a straggler was left to be resent")
	}
	if n, b := f.held(); n != 0 || b != 0 {
		t.Fatalf("a straggler of a dropped transfer is held: %d, %d", n, b)
	}
	data := bytes.Repeat(chunk, 3)
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f", "t0000000001", 3, data[:299_000], "big.bin", "")) {
		t.Fatal("the closing item was left to be resent")
	}
	if len(f.tg.Uploads()) != 0 || f.said("big.bin") != 1 || f.said("could not be taken") != 1 {
		t.Fatalf("uploads %d, chat %q", len(f.tg.Uploads()), f.texts())
	}
}

func TestAFileAboveTheLimitIsRefusedBeforeItIsPutTogether(t *testing.T) {
	f := newUpFixture(t, func(c *Config) { c.MaxFileBytes = 1 << 20 })
	data := upRandom(1<<20 + 1)
	p := upFilePayload("f", "t0000000001", 9, data, "huge.bin", "")
	if !f.h.upFile(context.Background(), "laptop", p) {
		t.Fatal("left to be resent")
	}
	if f.said("the file huge.bin is larger than this hub takes (1 MiB at most)") != 1 {
		t.Fatalf("%q", f.texts())
	}
	// its parts, which may still come, are confirmed and ignored
	if !f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, []byte("x"))) {
		t.Fatal("a part of a refused file was left to be resent")
	}
	if n, _ := f.held(); n != 0 {
		t.Fatalf("held %d", n)
	}
}

func TestADamagedDescriptionIsSaidOnceAndDropped(t *testing.T) {
	data := []byte("hello")
	good := func() *wire.Payload { return upFilePayload("f", "t0000000001", 1, data, "a.txt", "") }
	for name, mutate := range map[string]func(p *wire.Payload){
		"no transfer":        func(p *wire.Payload) { delete(p.Meta, wire.MetaTransfer) },
		"a path as transfer": func(p *wire.Payload) { p.Meta[wire.MetaTransfer] = "a/b" },
		"no parts":           func(p *wire.Payload) { delete(p.Meta, wire.MetaParts) },
		"zero parts":         func(p *wire.Payload) { p.Meta[wire.MetaParts] = "0" },
		"text parts":         func(p *wire.Payload) { p.Meta[wire.MetaParts] = "many" },
		"negative parts":     func(p *wire.Payload) { p.Meta[wire.MetaParts] = "-3" },
		"more parts than bytes": func(p *wire.Payload) {
			p.Meta[wire.MetaParts] = "6"
		},
		"too few parts for the size": func(p *wire.Payload) { p.Meta[wire.MetaSize] = "500000" },
		"a huge count":               func(p *wire.Payload) { p.Meta[wire.MetaParts] = "99999999999" },
		"no size":                    func(p *wire.Payload) { delete(p.Meta, wire.MetaSize) },
		"zero size":                  func(p *wire.Payload) { p.Meta[wire.MetaSize] = "0" },
		"a negative size":            func(p *wire.Payload) { p.Meta[wire.MetaSize] = "-5" },
		"a text size":                func(p *wire.Payload) { p.Meta[wire.MetaSize] = "0x10" },
		"no hash":                    func(p *wire.Payload) { delete(p.Meta, wire.MetaSHA) },
		"a short hash":               func(p *wire.Payload) { p.Meta[wire.MetaSHA] = "abcd" },
		"a hash that is not hex":     func(p *wire.Payload) { p.Meta[wire.MetaSHA] = strings.Repeat("z", 64) },
	} {
		f := newUpFixture(t, nil)
		p := good()
		mutate(p)
		if !f.h.upFile(context.Background(), "laptop", p) {
			t.Errorf("%s: left to be resent", name)
		}
		if len(f.tg.Uploads()) != 0 || f.said("damaged description") != 1 {
			t.Errorf("%s: uploads %d, chat %q", name, len(f.tg.Uploads()), f.texts())
		}
		if len(f.h.up.records) != 0 {
			t.Errorf("%s: a record was made", name)
		}
	}
}

func TestTheNameInTheClosingItemIsMadeSafe(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("hello")
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	p := upFilePayload("f", "t0000000001", 1, data, "ignored", "")
	p.Meta[wire.MetaName] = "../../.ssh/authorized_keys\n\x00"
	f.h.upFile(context.Background(), "laptop", p)
	ups := f.tg.Uploads()
	if len(ups) != 1 || ups[0].Name != filetx.SafeName(p.Meta[wire.MetaName]) || strings.ContainsAny(ups[0].Name, "/\\\n\x00") {
		t.Fatalf("%+v", ups)
	}
}

func TestPartsMissingAreGivenUpAfterThreeTriesWithAWordInTheChat(t *testing.T) {
	f := newUpFixture(t, nil)
	data := upRandom(300_000)
	chunks := filetx.Chunks(data)
	f.h.upPart("laptop", upPartPayload("p0", "t0000000001", 0, chunks[0]))
	f.h.upPart("laptop", upPartPayload("p2", "t0000000001", 2, chunks[2]))
	fp := func(id string) *wire.Payload { return upFilePayload(id, "t0000000001", 3, data, "scan.pdf", "") }
	for i := 1; i <= upMissGiveUp-1; i++ {
		f.advance(30 * time.Second)
		if f.h.upFile(context.Background(), "laptop", fp("f")) {
			t.Fatalf("try %d: given up too early", i)
		}
	}
	if len(f.texts()) != 0 {
		t.Fatalf("the chat was told before the hub gave up: %q", f.texts())
	}
	f.advance(30 * time.Second)
	if !f.h.upFile(context.Background(), "laptop", fp("f")) {
		t.Fatal("not given up at the third try")
	}
	if f.said("the file scan.pdf did not arrive whole (2 of 3 parts); send it again") != 1 || len(f.tg.Uploads()) != 0 {
		t.Fatalf("%q", f.texts())
	}
	if n, b := f.held(); n != 0 || b != 0 {
		t.Fatalf("the parts of a file given up are still held: %d, %d", n, b)
	}
	// a late part of it, or another closing item, is confirmed without a word
	if !f.h.upPart("laptop", upPartPayload("p1", "t0000000001", 1, chunks[1])) || !f.h.upFile(context.Background(), "laptop", fp("f3")) {
		t.Fatal("stragglers were left to be resent")
	}
	if len(f.texts()) != 1 || len(f.tg.Uploads()) != 0 || len(f.h.up.records) != 0 {
		t.Fatalf("%q, %d records", f.texts(), len(f.h.up.records))
	}
}

func TestPartsMissingAreGivenUpAfterTwoMinutesToo(t *testing.T) {
	f := newUpFixture(t, nil)
	data := upRandom(200_000)
	chunks := filetx.Chunks(data)
	f.h.upPart("laptop", upPartPayload("p0", "t0000000001", 0, chunks[0]))
	fp := upFilePayload("f", "t0000000001", 2, data, "a.bin", "")
	if f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("given up at once")
	}
	f.advance(upMissWindow)
	if !f.h.upFile(context.Background(), "laptop", fp) || f.said("1 of 2 parts") != 1 {
		t.Fatalf("%q", f.texts())
	}
}

// A closing item can come before any part of its file (a small file is one part, sent a moment before it); the hub waits
// for the resend like for a missing part, and nothing is said when the parts come in time.
func TestAClosingItemThatComesFirstIsAnsweredByTheResend(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("small")
	fp := upFilePayload("f", "t0000000001", 1, data, "small.txt", "")
	if f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("confirmed a file that has no part")
	}
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	if !f.h.upFile(context.Background(), "laptop", fp) || len(f.tg.Uploads()) != 1 {
		t.Fatalf("uploads %d", len(f.tg.Uploads()))
	}
	if len(f.texts()) != 0 {
		t.Fatalf("the user was told something: %q", f.texts())
	}
}

func TestAWrongHashOrSizeIsSaidAndDropped(t *testing.T) {
	data := upRandom(1000)
	for name, mutate := range map[string]func(p *wire.Payload){
		"a wrong hash": func(p *wire.Payload) { p.Meta[wire.MetaSHA] = filetx.SHA([]byte("other")) },
		"a wrong size": func(p *wire.Payload) { p.Meta[wire.MetaSize] = "999" },
	} {
		f := newUpFixture(t, nil)
		f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
		p := upFilePayload("f", "t0000000001", 1, data, "a.bin", "")
		mutate(p)
		if !f.h.upFile(context.Background(), "laptop", p) {
			t.Errorf("%s: left to be resent", name)
		}
		if len(f.tg.Uploads()) != 0 || f.said("the file a.bin arrived damaged") != 1 {
			t.Errorf("%s: uploads %d, chat %q", name, len(f.tg.Uploads()), f.texts())
		}
		if n, b := f.held(); n != 0 || b != 0 {
			t.Errorf("%s: held %d, %d", name, n, b)
		}
	}
}

func TestMorePartsThanAnnouncedIsDamageToo(t *testing.T) {
	f := newUpFixture(t, nil)
	data := upRandom(200_000)
	chunks := filetx.Chunks(data)
	f.h.upPart("laptop", upPartPayload("p0", "t0000000001", 0, chunks[0]))
	f.h.upPart("laptop", upPartPayload("p1", "t0000000001", 1, chunks[1]))
	f.h.upPart("laptop", upPartPayload("p2", "t0000000001", 2, []byte("one more than announced")))
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f", "t0000000001", 2, data, "a.bin", "")) {
		t.Fatal("left to be resent")
	}
	if len(f.tg.Uploads()) != 0 || f.said("arrived damaged") != 1 {
		t.Fatalf("uploads %d, chat %q", len(f.tg.Uploads()), f.texts())
	}
}

func TestAHubThatCannotUploadSaysSoOnce(t *testing.T) {
	f := newUpFixture(t, func(c *Config) { c.Telegram = struct{ telegram.Client }{c.Telegram} })
	data := []byte("hello")
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	if !f.h.upFile(context.Background(), "laptop", upFilePayload("f", "t0000000001", 1, data, "a.txt", "")) {
		t.Fatal("left to be resent for ever")
	}
	if f.said("this hub cannot upload files") != 1 {
		t.Fatalf("%q", f.texts())
	}
	if n, _ := f.held(); n != 0 {
		t.Fatalf("the parts of a file that cannot be sent are held: %d", n)
	}
}

func TestWhatTelegramAnswersDecidesWhetherTheAgentSendsAgain(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		resend bool
	}{
		"bad request":       {&telegram.APIError{Method: "sendDocument", Code: 400, Desc: "Bad Request: file must be non-empty"}, false},
		"too large":         {&telegram.APIError{Method: "sendDocument", Code: 413, Desc: "Request Entity Too Large"}, false},
		"wrapped bad":       {fmt.Errorf("sending: %w", &telegram.APIError{Method: "sendPhoto", Code: 400, Desc: "x"}), false},
		"rate limited":      {telegram.ErrRateLimited{After: time.Minute}, true},
		"server error":      {&telegram.APIError{Method: "sendDocument", Code: 502, Desc: "Bad Gateway"}, true},
		"forbidden":         {&telegram.APIError{Method: "sendDocument", Code: 403, Desc: "Forbidden: bot was blocked by the user"}, true},
		"no network":        {errors.New("telegram sendDocument: connection reset"), true},
		"deadline exceeded": {context.DeadlineExceeded, true},
	} {
		f := newUpFixture(t, nil)
		data := []byte("hello")
		f.tg.SetFileErr(c.err)
		f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
		done := f.h.upFile(context.Background(), "laptop", upFilePayload("f", "t0000000001", 1, data, "a.txt", ""))
		if done == c.resend {
			t.Errorf("%s: dealt with = %v", name, done)
		}
		if c.resend && len(f.texts()) != 0 {
			t.Errorf("%s: the chat heard of a failure that may pass: %q", name, f.texts())
		}
		if !c.resend && f.said("a.txt") != 1 {
			t.Errorf("%s: %q", name, f.texts())
		}
		if !c.resend && name == "bad request" && f.said("file must be non-empty") != 1 {
			t.Errorf("%s: Telegram's reason is not in the chat: %q", name, f.texts())
		}
	}
}

func TestAnUploadThatFailsIsRetriedWithoutTheParts(t *testing.T) {
	f := newUpFixture(t, nil)
	data := upRandom(300_000)
	f.tg.SetFileErr(&telegram.APIError{Method: "sendDocument", Code: 500, Desc: "oops"})
	f.send2(t, data)
	fp := func() *wire.Payload { return upFilePayload("f", "t0000000001", 3, data, "a.bin", "cap") }
	if f.h.upFile(context.Background(), "laptop", fp()) {
		t.Fatal("dealt with although Telegram took nothing")
	}
	if n, b := f.held(); n != 0 || b != 0 {
		t.Fatalf("the parts are kept twice (once as parts, once as the finished file): %d, %d", n, b)
	}
	if len(f.h.up.ready) != 1 {
		t.Fatal("the finished file is not kept for the retry")
	}
	f.tg.SetFileErr(nil)
	f.advance(30 * time.Second)
	if !f.h.upFile(context.Background(), "laptop", fp()) || len(f.tg.Uploads()) != 1 || !bytes.Equal(f.tg.Uploads()[0].Data, data) {
		t.Fatalf("uploads %d", len(f.tg.Uploads()))
	}
	if len(f.h.up.ready) != 0 || len(f.texts()) != 0 {
		t.Fatalf("ready %d, chat %q", len(f.h.up.ready), f.texts())
	}
}

func (f *upFixture) send2(t *testing.T, data []byte) {
	t.Helper()
	for i, c := range filetx.Chunks(data) {
		if !f.h.upPart("laptop", upPartPayload(fmt.Sprintf("p%d", i), "t0000000001", i, c)) {
			t.Fatal("part not taken")
		}
	}
}

func TestAnUploadThatKeepsFailingIsGivenUpWithAWordInTheChat(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("hello")
	f.tg.SetFileErr(telegram.ErrRateLimited{After: time.Second})
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	fp := upFilePayload("f", "t0000000001", 1, data, "a.txt", "")
	for i := 1; i < upFailGiveUp; i++ {
		if f.h.upFile(context.Background(), "laptop", fp) {
			t.Fatalf("given up at try %d", i)
		}
		f.advance(30 * time.Second)
	}
	if !f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("not given up")
	}
	if f.said("Telegram did not take the file a.txt after several tries") != 1 || len(f.h.up.ready) != 0 {
		t.Fatalf("%q, ready %d", f.texts(), len(f.h.up.ready))
	}
}

func TestAFileWaitingForTelegramForTooLongIsGivenUpByTheClock(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("hello")
	f.tg.SetFileErr(errors.New("no network"))
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	fp := upFilePayload("f", "t0000000001", 1, data, "a.txt", "")
	if f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("given up at once")
	}
	f.advance(upFailWindow + time.Second)
	if !f.h.upFile(context.Background(), "laptop", fp) || f.said("did not take the file") != 1 {
		t.Fatalf("%q", f.texts())
	}
}

// The finished file is kept for a few minutes only. When the retry comes later, the parts are gone: the user hears that
// the upload did not work, not that the file did not arrive.
func TestAFinishedFileThatExpiredWhileTelegramWasDownIsReportedAsNotUploaded(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("hello")
	f.tg.SetFileErr(errors.New("no network"))
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, data))
	fp := upFilePayload("f", "t0000000001", 1, data, "a.txt", "")
	if f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("dealt with although Telegram took nothing")
	}
	f.tg.SetFileErr(nil)
	f.advance(upReadyTTL + time.Minute)
	if !f.h.upFile(context.Background(), "laptop", fp) {
		t.Fatal("left to be resent for ever")
	}
	if len(f.tg.Uploads()) != 0 || f.said("the file a.txt could not be uploaded to the chat in time") != 1 || len(f.h.up.ready) != 0 {
		t.Fatalf("uploads %d, chat %q", len(f.tg.Uploads()), f.texts())
	}
}

func TestAtMostTwoFinishedFilesWaitForTelegram(t *testing.T) {
	f := newUpFixture(t, nil)
	f.tg.SetFileErr(errors.New("no network"))
	datas := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for i, d := range datas {
		tr := fmt.Sprintf("t%010d", i+1)
		f.h.upPart("laptop", upPartPayload("p"+tr, tr, 0, d))
	}
	for i, d := range datas {
		tr := fmt.Sprintf("t%010d", i+1)
		if f.h.upFile(context.Background(), "laptop", upFilePayload("f"+tr, tr, 1, d, fmt.Sprintf("f%d.txt", i), "")) {
			t.Fatalf("file %d dealt with", i)
		}
	}
	if len(f.h.up.ready) != upReadyMax {
		t.Fatalf("%d finished files are kept", len(f.h.up.ready))
	}
	if n, b := f.held(); n != 1 || b != 5 {
		t.Fatalf("the third file should still wait as parts: %d transfers, %d bytes", n, b)
	}
	// Telegram is back: the first two go, then the third
	f.tg.SetFileErr(nil)
	for i, d := range datas {
		tr := fmt.Sprintf("t%010d", i+1)
		if !f.h.upFile(context.Background(), "laptop", upFilePayload("f"+tr, tr, 1, d, fmt.Sprintf("f%d.txt", i), "")) {
			t.Fatalf("file %d not dealt with", i)
		}
	}
	if len(f.tg.Uploads()) != 3 || len(f.h.up.ready) != 0 {
		t.Fatalf("%d uploads, %d kept", len(f.tg.Uploads()), len(f.h.up.ready))
	}
}

func TestRecordsAreBoundedAndForgotten(t *testing.T) {
	f := newUpFixture(t, nil)
	data := []byte("x")
	for i := 0; i < upRecordsMax+10; i++ {
		tr := fmt.Sprintf("t%010d", i)
		f.h.upFile(context.Background(), "laptop", upFilePayload("f", tr, 1, data, "a.txt", "")) // closing items of files that never had parts
	}
	if n := len(f.h.up.records); n > upRecordsMax {
		t.Fatalf("%d records", n)
	}
	f.advance(upRecordTTL + time.Second)
	f.h.upFile(context.Background(), "laptop", upFilePayload("f", "t9999999999", 1, data, "a.txt", ""))
	if n := len(f.h.up.records); n != 1 {
		t.Fatalf("%d records after they expired", n)
	}
}

func TestFinishedTransfersAreRememberedInABoundedWay(t *testing.T) {
	f := newUpFixture(t, nil)
	for i := 0; i < upDoneMax+20; i++ {
		f.send(t, "laptop", fmt.Sprintf("t%010d", i), []byte("x"), "a.txt", "")
	}
	if len(f.h.up.done) > upDoneMax || len(f.h.up.doneLog) > upDoneMax || len(f.tg.Uploads()) != upDoneMax+20 {
		t.Fatalf("%d remembered, %d uploads", len(f.h.up.done), len(f.tg.Uploads()))
	}
}

// upGatedSender counts the uploads that are in progress at once and holds each until released.
type upGatedSender struct {
	*telegram.Fake
	gate    chan struct{}
	started chan struct{}
	now, hi atomic.Int32
}

func (g *upGatedSender) SendFile(ctx context.Context, chatID int64, o telegram.OutFile) ([]int64, error) {
	n := g.now.Add(1)
	for {
		h := g.hi.Load()
		if n <= h || g.hi.CompareAndSwap(h, n) {
			break
		}
	}
	g.started <- struct{}{}
	defer g.now.Add(-1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Fake.SendFile(ctx, chatID, o)
}

func TestOnlyOneFileIsUploadedAtATime(t *testing.T) {
	var g *upGatedSender
	f := newUpFixture(t, func(c *Config) {
		g = &upGatedSender{Fake: c.Telegram.(*telegram.Fake), gate: make(chan struct{}), started: make(chan struct{}, 8)}
		c.Telegram = g
	})
	for i := 1; i <= 3; i++ {
		tr := fmt.Sprintf("t%010d", i)
		f.h.upPart("laptop", upPartPayload("p"+tr, tr, 0, []byte("data"+tr)))
	}
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		tr := fmt.Sprintf("t%010d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.h.upFile(context.Background(), "laptop", upFilePayload("f"+tr, tr, 1, []byte("data"+tr), tr+".txt", ""))
		}()
	}
	<-g.started
	time.Sleep(150 * time.Millisecond) // the other two would have started by now
	if n := g.now.Load(); n != 1 {
		t.Fatalf("%d uploads in progress at once", n)
	}
	close(g.gate)
	wg.Wait()
	if g.hi.Load() != 1 || len(g.Fake.Uploads()) != 3 {
		t.Fatalf("at most %d at once, %d uploaded", g.hi.Load(), len(g.Fake.Uploads()))
	}
}

func TestAHubThatStopsDuringAnUploadLeavesTheFileToBeResent(t *testing.T) {
	var g *upGatedSender
	f := newUpFixture(t, func(c *Config) {
		g = &upGatedSender{Fake: c.Telegram.(*telegram.Fake), gate: make(chan struct{}), started: make(chan struct{}, 8)}
		c.Telegram = g
	})
	f.h.upPart("laptop", upPartPayload("p", "t0000000001", 0, []byte("data")))
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan bool, 1)
	go func() {
		res <- f.h.upFile(ctx, "laptop", upFilePayload("f", "t0000000001", 1, []byte("data"), "a.txt", ""))
	}()
	<-g.started
	cancel()
	if done := <-res; done {
		t.Fatal("the item was confirmed although the hub was stopped before Telegram took the file")
	}
	if len(f.texts()) != 0 {
		t.Fatalf("the chat was told of a failure: %q", f.texts())
	}
	// the next hub run gets the resend and the file is there to be uploaded: the retry does not count as a failure
	f.h.up.mu.Lock()
	fails := f.h.up.records["laptop/t0000000001"].fails
	f.h.up.mu.Unlock()
	if fails != 0 {
		t.Fatalf("%d failures were counted for a stop", fails)
	}
}

func TestAnAgentNameLongerThanTheAssemblersKeyStillWorks(t *testing.T) {
	long := strings.Repeat("a", 31) + "b" // 32 bytes is the longest name an agent may have
	if len(long) != 32 || !identity.ValidName(long) {
		t.Fatalf("test name %q", long)
	}
	c := base(t)
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a, _ := identity.Generate(long)
	c.Agents = []identity.Public{a.Public()}
	h, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello")
	if !h.upPart(long, upPartPayload("p", "t000000000001", 0, data)) {
		t.Fatal("part")
	}
	if n, _ := h.up.asm.Held(); n != 1 {
		t.Fatalf("the part was refused: held %d", n)
	}
	if !h.upFile(context.Background(), long, upFilePayload("f", "t000000000001", 1, data, "a.txt", "")) || len(c.Telegram.(*telegram.Fake).Uploads()) != 1 {
		t.Fatal("not uploaded")
	}
}

func TestTwoAgentsMayUseTheSameTransferId(t *testing.T) {
	f := newUpFixture(t, func(c *Config) {
		b, _ := identity.Generate("desk")
		c.Agents = append(c.Agents, b.Public())
	})
	f.h.upPart("laptop", upPartPayload("p1", "t0000000001", 0, []byte("from the laptop")))
	f.h.upPart("desk", upPartPayload("p2", "t0000000001", 0, []byte("from the desk")))
	f.h.upFile(context.Background(), "laptop", upFilePayload("f1", "t0000000001", 1, []byte("from the laptop"), "a.txt", ""))
	f.h.upFile(context.Background(), "desk", upFilePayload("f2", "t0000000001", 1, []byte("from the desk"), "b.txt", ""))
	ups := f.tg.Uploads()
	if len(ups) != 2 || string(ups[0].Data) != "from the laptop" || string(ups[1].Data) != "from the desk" {
		t.Fatalf("%+v", ups)
	}
}
