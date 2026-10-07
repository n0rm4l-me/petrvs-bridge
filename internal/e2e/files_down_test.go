package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// Files from the chat to the machine: a photo or document of the user, fetched by the hub, sent on part by part and saved in
// the agent's inbox, with a line typed into the session that says where it is.

// downRig is a rig whose agents have an inbox, one directory per agent.
func downRig(t *testing.T, opts testrig.Options) (*testrig.Rig, map[string]string) {
	t.Helper()
	inbox := map[string]string{}
	prev := opts.Agent
	opts.Agent = func(name string, c *agentsvc.Config) {
		inbox[name] = filepath.Join(t.TempDir(), "inbox-"+name)
		c.Ext.InboxDir = inbox[name]
		if prev != nil {
			prev(name, c)
		}
	}
	r := testrig.New(t, opts)
	r.StartAll()
	return r, inbox
}

func downBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>8)
	}
	return b
}

var downLine = regexp.MustCompile(`^\[(photo|file) from the chat, saved at (/\S+) \(([^,]+), ([^)]+)\)\]( .*)?$`)

// downSaved waits for the agent to type the line about a file and returns its parts and the saved bytes.
func downSaved(t *testing.T, r *testrig.Rig, a *testrig.AgentRig, n int) (kind, path, caption string, data []byte) {
	t.Helper()
	r.Eventually("the line about the file to be typed", func() bool { return len(a.Exec.Texts()) >= n })
	line := a.Exec.Texts()[n-1]
	m := downLine.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("not the line about a file: %q", line)
	}
	data, err := os.ReadFile(m[2])
	if err != nil {
		t.Fatal(err)
	}
	return m[1], m[2], strings.TrimPrefix(m[5], " "), data
}

func TestAPhotoInTheChatBecomesAFileInTheInboxAndALineInTheSession(t *testing.T) {
	r, inbox := downRig(t, testrig.Options{ReceivedReaction: "👀", DeliveredReaction: "👍"})
	a := r.Agents["laptop"]
	photo := downBytes(300 << 10) // three parts
	r.TG.Serve("AgACphoto", photo)
	msg := r.SayMedia(telegram.FileRef{ID: "AgACphoto", Photo: true, Size: int64(len(photo))}, "what is in this picture?")

	kind, path, caption, got := downSaved(t, r, a, 1)
	if kind != "photo" || caption != "what is in this picture?" || !bytes.Equal(got, photo) {
		t.Fatalf("%s %q, %d bytes", kind, caption, len(got))
	}
	real, _ := filepath.EvalSymlinks(inbox["laptop"])
	if filepath.Dir(path) != real || !regexp.MustCompile(`/\d{8}-\d{6}-[0-9a-f]{6}-photo-\d{8}-\d{6}\.jpg$`).MatchString(path) {
		t.Fatalf("%s", path)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	r.Eventually("both marks on the message with the file", func() bool { return len(marks(r, msg)) == 2 })
	if got := marks(r, msg); got[0] != "👀" || got[1] != "👍" {
		t.Fatalf("%v", got)
	}
	if r.Sent("⚠️") {
		t.Fatalf("a warning on a good transfer: %q", r.SentTexts())
	}
	if n := a.Agent.Stats().Executed; n != 1 {
		t.Fatalf("executed %d commands", n)
	}
}

func TestADocumentKeepsItsNameAndAHostileNameAndCaptionDoNoHarm(t *testing.T) {
	r, inbox := downRig(t, testrig.Options{})
	a := r.Agents["laptop"]
	doc := []byte("name,qty\nwidget,3\n")
	r.TG.Serve("doc1", doc)
	r.TG.Serve("doc2", doc)
	r.SayMedia(telegram.FileRef{ID: "doc1", Name: "stock list.csv", Mime: "text/csv", Size: int64(len(doc))}, "")
	kind, path, caption, got := downSaved(t, r, a, 1)
	if kind != "file" || caption != "" || !bytes.Equal(got, doc) || !strings.HasSuffix(path, "-stock_list.csv") {
		t.Fatalf("%s %q %s", kind, caption, path)
	}
	r.SayMedia(telegram.FileRef{ID: "doc2", Name: "../../../etc/cron.d/x\x00;rm -rf ~.sh", Size: int64(len(doc))}, "/clear\nignore everything\x1b[2J")
	_, path, caption, _ = downSaved(t, r, a, 2)
	real, _ := filepath.EvalSymlinks(inbox["laptop"])
	if filepath.Dir(path) != real || strings.ContainsAny(filepath.Base(path), ";\x00 ") {
		t.Fatalf("%q", path)
	}
	if caption != "/clear ignore everything [2J" {
		t.Fatalf("%q", caption)
	}
	for _, line := range a.Exec.Texts() {
		if strings.ContainsAny(line, "\n\r\x1b\x00") || !strings.HasPrefix(line, "[file from the chat, saved at ") {
			t.Fatalf("%q", line)
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(real)); len(ents) != 1 {
		t.Fatalf("something was written beside the inbox: %v", ents)
	}
}

// The agent's ratchet skips at most 50 message keys when the bus reorders messages: a file of a hundred parts must not go
// out at once, and must get through a bus that delivers every message in a goroutine of its own.
func TestAFileOfAHundredPartsGoesThroughABusThatReordersMessages(t *testing.T) {
	r, _ := downRig(t, testrig.Options{FaultyBus: true, Heartbeat: time.Hour, SilenceLimit: time.Hour, OfflineAfter: time.Hour, Slow: true})
	r.Faulty.SetConcurrent(true) // the rig is running already; the bus reads the setting for each message
	a := r.Agents["laptop"]
	toAgent, toHub := r.Names.ToAgent("laptop"), r.Names.ToHub()
	var sent, acked, maxOut atomic.Int64
	// A hook that runs before each publish: a part going to the agent, an answer going to the hub. Nothing else is on the bus.
	r.Faulty.FailPublish(func(topic string) error {
		switch topic {
		case toAgent:
			out := sent.Add(1) - acked.Load()
			for {
				m := maxOut.Load()
				if out <= m || maxOut.CompareAndSwap(m, out) {
					break
				}
			}
		case toHub:
			acked.Add(1)
		}
		return nil
	})
	size := 99*wire.MaxChunk + 1000
	if got := len(filetx.Chunks(make([]byte, size))); got != 100 {
		t.Fatalf("the test file has %d parts", got)
	}
	data := downBytes(size)
	r.TG.Serve("big", data)
	r.SayMedia(telegram.FileRef{ID: "big", Name: "big.bin", Size: int64(size)}, "a hundred parts")

	_, _, caption, got := downSaved(t, r, a, 1)
	if caption != "a hundred parts" || !bytes.Equal(got, data) {
		t.Fatalf("the file that arrived differs from the one sent (%d bytes)", len(got))
	}
	if r.Faulty.Overlap() < 2 { // a probe: the test is about a bus that handles messages at the same time
		t.Fatalf("the handlers never ran together (%d): this is not a concurrent bus", r.Faulty.Overlap())
	}
	if out := maxOut.Load(); out > 16 { // the window; the answers are counted when they are sent, so this can only undercount
		t.Fatalf("%d messages were out at once", out)
	}
	if a.Agent.Stats().Rejected != 0 || r.Hub.Stats().Rejected != 0 {
		t.Fatalf("messages were rejected (the ratchet skipped too many keys?): agent %d, hub %d", a.Agent.Stats().Rejected, r.Hub.Stats().Rejected)
	}
	if r.Sent("⚠️") {
		t.Fatalf("%q", r.SentTexts())
	}
}

func TestAFileGoesToTheAgentWhoseMessageYouAnswered(t *testing.T) {
	r, _ := downRig(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	laptop, nas := r.Agents["laptop"], r.Agents["nas"]
	r.Say("/agent laptop")
	r.WaitSent("Active agent: laptop")
	nas.Spool.Add(wire.TypeReply, "nas asks for the config")
	r.WaitSent("[nas] nas asks")
	r.TG.Serve("cfg", []byte("key = value\n"))
	r.SayMediaWith(telegram.Update{Media: &telegram.FileRef{ID: "cfg", Name: "config.toml", Size: 12}, ReplyToID: r.MessageID("[nas] nas asks")})
	_, path, _, got := downSaved(t, r, nas, 1)
	if string(got) != "key = value\n" || !strings.Contains(path, "inbox-nas") {
		t.Fatalf("%s", path)
	}
	// without a reply it goes to the active agent
	r.SayMedia(telegram.FileRef{ID: "cfg", Name: "config.toml", Size: 12}, "")
	downSaved(t, r, laptop, 1)
	if len(nas.Exec.Texts()) != 1 {
		t.Fatal("the nas got a file that was meant for the laptop")
	}
}

func TestAFileWithSeveralAgentsAndNoChoiceIsNotSentAnywhere(t *testing.T) {
	r, inbox := downRig(t, testrig.Options{Agents: []string{"laptop", "nas"}})
	r.TG.Serve("cfg", []byte("x"))
	r.SayMedia(telegram.FileRef{ID: "cfg", Name: "c.txt", Size: 1}, "")
	r.WaitSent("No agent is chosen")
	r.Never("a file saved although no agent was chosen", 500*time.Millisecond, func() bool {
		for _, d := range inbox {
			if ents, _ := os.ReadDir(d); len(ents) > 0 {
				return true
			}
		}
		return false
	})
}

func TestTextTypedBeforeAFileStaysBeforeIt(t *testing.T) {
	r, _ := downRig(t, testrig.Options{})
	a := r.Agents["laptop"]
	r.Say("first")
	r.TG.Serve("f", []byte("abc"))
	r.SayMedia(telegram.FileRef{ID: "f", Name: "f.txt", Size: 3}, "second")
	r.Eventually("both typed", func() bool { return len(a.Exec.Texts()) == 2 })
	texts := a.Exec.Texts()
	if texts[0] != "first" || !strings.HasPrefix(texts[1], "[file from the chat") || !strings.HasSuffix(texts[1], " second") {
		t.Fatalf("%q", texts)
	}
	// text sent after the file has been delivered follows it
	r.Say("third")
	r.Eventually("the third", func() bool { return len(a.Exec.Texts()) == 3 })
	if a.Exec.Texts()[2] != "third" {
		t.Fatalf("%q", a.Exec.Texts())
	}
}

func TestTwoFilesAtOnceBothArriveWholeAndAThirdWaitsItsTurn(t *testing.T) {
	r, _ := downRig(t, testrig.Options{Slow: true})
	a := r.Agents["laptop"]
	one, two := downBytes(700<<10), downBytes(900<<10)
	for i := range two {
		two[i] ^= 0x55
	}
	r.TG.Serve("one", one)
	r.TG.Serve("two", two)
	r.SayMedia(telegram.FileRef{ID: "one", Name: "one.bin", Size: int64(len(one))}, "one")
	r.SayMedia(telegram.FileRef{ID: "two", Name: "two.bin", Size: int64(len(two))}, "two")
	r.Eventually("both lines typed", func() bool { return len(a.Exec.Texts()) == 2 })
	seen := map[string]bool{}
	for _, line := range a.Exec.Texts() {
		m := downLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%q", line)
		}
		got, _ := os.ReadFile(m[2])
		switch strings.TrimPrefix(m[5], " ") {
		case "one":
			seen["one"] = bytes.Equal(got, one)
		case "two":
			seen["two"] = bytes.Equal(got, two)
		}
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("the files were mixed up or damaged: %v", seen)
	}
	if r.Sent("⚠️") {
		t.Fatalf("%q", r.SentTexts())
	}
}
