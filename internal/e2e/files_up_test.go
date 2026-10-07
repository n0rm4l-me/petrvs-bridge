package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/telegram"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// Files from the machine to the chat, the whole way: the real `agent send-file` binary puts a file into the spool of a
// real agent, which sends it over the bus to a real hub, which uploads it with the real Telegram client to a stand-in
// for the Bot API. Only the bus, the clock of the chat and the tmux session are fakes.

const upToken = "123456:FILES-UP-TOKEN"

// upUpload is a file the stand-in Bot API received.
type upUpload struct {
	method, chatID, caption, filename string
	data                              []byte
}

// upAPI is a stand-in for the Bot API that answers the calls of the hub and records the files.
type upAPI struct {
	srv *httptest.Server
	mu  sync.Mutex
	// files is what was uploaded; answer, when set, decides the n-th upload (from 1) and returns the status to send
	// instead of accepting it (0 accepts).
	files  []upUpload
	texts  []string // what the hub wrote to the chat
	tries  int
	answer func(n int, method string) (status int, description string)
	nextID int
}

func newUpAPI(t *testing.T) *upAPI {
	t.Helper()
	a := &upAPI{nextID: 5000}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bot"+upToken+"/")
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getUpdates":
			time.Sleep(100 * time.Millisecond)
			io.WriteString(w, `{"ok":true,"result":[]}`)
		case "sendPhoto", "sendDocument":
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				w.WriteHeader(400)
				io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: not a form"}`)
				return
			}
			up := upUpload{method: method, chatID: r.FormValue("chat_id"), caption: r.FormValue("caption")}
			for _, field := range []string{"photo", "document"} {
				if fh := r.MultipartForm.File[field]; len(fh) == 1 {
					f, _ := fh[0].Open()
					up.filename = fh[0].Filename
					up.data, _ = io.ReadAll(f)
					f.Close()
				}
			}
			a.mu.Lock()
			a.tries++
			n, answer := a.tries, a.answer
			a.mu.Unlock()
			if answer != nil {
				if status, desc := answer(n, method); status != 0 {
					w.WriteHeader(status)
					json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": desc})
					return
				}
			}
			a.mu.Lock()
			a.files = append(a.files, up)
			a.nextID++
			id := a.nextID
			a.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
		case "sendMessage":
			var body struct {
				Text string `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			a.mu.Lock()
			a.texts = append(a.texts, body.Text)
			a.nextID++
			id := a.nextID
			a.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
		default:
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *upAPI) uploads() []upUpload {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]upUpload(nil), a.files...)
}

// said reports whether the hub wrote something containing sub to the chat.
func (a *upAPI) said(sub string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.texts {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

func (a *upAPI) attempts() int { a.mu.Lock(); defer a.mu.Unlock(); return a.tries }

// upRigFor builds a rig whose agents keep their spool in <state>/spool, where the real binary puts files, and whose hub
// talks to the stand-in API through the real client.
func upRigFor(t *testing.T, api *upAPI, opts testrig.Options, names ...string) (*testrig.Rig, map[string]string) {
	t.Helper()
	states := map[string]string{}
	spools := map[string]*spool.Spool{}
	for _, n := range names {
		states[n] = t.TempDir()
		sp, err := spool.Open(filepath.Join(states[n], "spool"))
		if err != nil {
			t.Fatal(err)
		}
		spools[n] = sp
	}
	opts.Agents = names
	opts.WrapTelegram = func(*telegram.Fake) telegram.Client {
		return telegram.NewHTTP(upToken).WithBaseURL(api.srv.URL)
	}
	opts.Agent = func(name string, c *agentsvc.Config) { c.Spool = spools[name] }
	return testrig.New(t, opts), states
}

func buildUpAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/n0rm4l-me/petrvs-bridge/cmd/agent").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return bin
}

// sendFile runs `agent send-file` the way a hook does, with nothing but the state directory in its environment.
func runSendFile(t *testing.T, bin, state string, args ...string) string {
	t.Helper()
	cmd := exec.Command("env", append([]string{"-i", "PB_STATE_DIR=" + state, bin, "send-file"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("send-file %v: %v: %s", args, err, out.String())
	}
	return out.String()
}

func upWrite(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func upBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func upSpoolLen(state string) int {
	files, _ := filepath.Glob(filepath.Join(state, "spool", "*.json"))
	return len(files)
}

func TestAFileSentFromTwoMachinesReachesTheChatAsPhotoAndDocument(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	r, states := upRigFor(t, api, testrig.Options{}, "laptop", "desk")
	r.StartAll()
	files := t.TempDir()
	shot := upBytes(300_000, 1)
	report := upBytes(50_000, 2)
	runSendFile(t, bin, states["laptop"], "-caption", "look at this", upWrite(t, files, "screen.png", shot))
	runSendFile(t, bin, states["desk"], upWrite(t, files, "report.txt", report))

	r.Eventually("both files in the chat", func() bool { return len(api.uploads()) == 2 })
	r.Eventually("both spools empty", func() bool { return upSpoolLen(states["laptop"]) == 0 && upSpoolLen(states["desk"]) == 0 })
	r.Never("a third upload", 500*time.Millisecond, func() bool { return len(api.uploads()) > 2 })
	by := map[string]upUpload{}
	for _, u := range api.uploads() {
		by[u.filename] = u
	}
	if u := by["screen.png"]; u.method != "sendPhoto" || u.caption != "[laptop] look at this" || !bytes.Equal(u.data, shot) || u.chatID != strconv.Itoa(testrig.ChatID) {
		t.Fatalf("laptop's file: %s %q chat %s, %d bytes", u.method, u.caption, u.chatID, len(u.data))
	}
	if u := by["report.txt"]; u.method != "sendDocument" || u.caption != "[desk] report.txt" || !bytes.Equal(u.data, report) {
		t.Fatalf("desk's file: %s %q, %d bytes", u.method, u.caption, len(u.data))
	}
	if api.said("⚠️") {
		t.Fatalf("the user was told of a problem: %q", api.texts)
	}
}

// A long file goes through the window of unconfirmed parts and a bus that handles messages at the same time, as Pub/Sub
// does; the ratchet of the hub would refuse a burst of a hundred parts, and the file must still be whole.
func TestABigFileSurvivesTheWindowAndAConcurrentBus(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	r, states := upRigFor(t, api, testrig.Options{FaultyBus: true}, "laptop")
	r.Faulty.Concurrent = true
	r.StartAll()
	big := upBytes(6<<20, 3) // 48 parts
	runSendFile(t, bin, states["laptop"], upWrite(t, t.TempDir(), "dump.bin", big))
	r.Eventually("the file in the chat", func() bool { return len(api.uploads()) >= 1 })
	r.Eventually("the spool empty", func() bool { return upSpoolLen(states["laptop"]) == 0 })
	r.Never("a second upload", 500*time.Millisecond, func() bool { return len(api.uploads()) > 1 })
	if u := api.uploads()[0]; u.method != "sendDocument" || !bytes.Equal(u.data, big) {
		t.Fatalf("%s, %d bytes", u.method, len(u.data))
	}
	if api.said("⚠️") {
		t.Fatalf("%q", api.texts)
	}
}

func TestAPictureTelegramWillNotShowAsAPhotoArrivesAsADocument(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	api.answer = func(n int, method string) (int, string) {
		if method == "sendPhoto" {
			return 400, "Bad Request: PHOTO_INVALID_DIMENSIONS"
		}
		return 0, ""
	}
	r, states := upRigFor(t, api, testrig.Options{}, "laptop")
	r.StartAll()
	data := upBytes(20_000, 4)
	runSendFile(t, bin, states["laptop"], upWrite(t, t.TempDir(), "banner.png", data))
	r.Eventually("the file in the chat", func() bool { return len(api.uploads()) == 1 })
	r.Eventually("the spool empty", func() bool { return upSpoolLen(states["laptop"]) == 0 })
	if u := api.uploads()[0]; u.method != "sendDocument" || u.filename != "banner.png" || !bytes.Equal(u.data, data) {
		t.Fatalf("%s %s", u.method, u.filename)
	}
	if api.said("⚠️") {
		t.Fatalf("the user was told of a problem that did not matter: %q", api.texts)
	}
}

func TestATelegramThatFailsTwiceStillGetsTheWholeFileOnce(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	api.answer = func(n int, method string) (int, string) {
		if n <= 2 {
			return 502, "Bad Gateway"
		}
		return 0, ""
	}
	r, states := upRigFor(t, api, testrig.Options{}, "laptop")
	r.StartAll()
	data := upBytes(400_000, 5)
	runSendFile(t, bin, states["laptop"], upWrite(t, t.TempDir(), "flaky.bin", data))
	r.Eventually("the file in the chat", func() bool { return len(api.uploads()) == 1 })
	r.Eventually("the spool empty", func() bool { return upSpoolLen(states["laptop"]) == 0 })
	r.Never("a second upload", 600*time.Millisecond, func() bool { return len(api.uploads()) > 1 })
	if !bytes.Equal(api.uploads()[0].data, data) || api.attempts() != 3 {
		t.Fatalf("%d bytes after %d attempts", len(api.uploads()[0].data), api.attempts())
	}
	if api.said("⚠️") {
		t.Fatalf("%q", api.texts)
	}
}

func TestATelegramThatRefusesTheFileTellsTheUserOnceAndTheAgentStops(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	api.answer = func(n int, method string) (int, string) { return 400, "Bad Request: file is too big" }
	r, states := upRigFor(t, api, testrig.Options{}, "laptop")
	r.StartAll()
	runSendFile(t, bin, states["laptop"], upWrite(t, t.TempDir(), "huge.bin", upBytes(10_000, 6)))
	r.Eventually("the chat to be told why", func() bool {
		return api.said("⚠️ laptop: Telegram refused the file huge.bin (Bad Request: file is too big)")
	})
	r.Eventually("the spool empty", func() bool { return upSpoolLen(states["laptop"]) == 0 })
	time.Sleep(500 * time.Millisecond)
	if n := api.attempts(); n != 1 {
		t.Fatalf("%d attempts for a file that is refused for good", n)
	}
}

// A locked machine sends no file; the queue keeps it and the chat gets it after `agent unlock`.
func TestAFileQueuedOnALockedMachineWaitsForTheUnlock(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	r, states := upRigFor(t, api, testrig.Options{}, "laptop")
	a := r.Agents["laptop"]
	if err := os.WriteFile(a.LockFile, []byte("locked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.StartAll()
	data := upBytes(100_000, 7)
	runSendFile(t, bin, states["laptop"], upWrite(t, t.TempDir(), "plans.txt", data))
	r.Never("a file leaving a locked machine", time.Second, func() bool { return len(api.uploads()) > 0 })
	if upSpoolLen(states["laptop"]) != 2 {
		t.Fatalf("%d items in the queue", upSpoolLen(states["laptop"]))
	}
	if err := os.Remove(a.LockFile); err != nil {
		t.Fatal(err)
	}
	r.Eventually("the file after the unlock", func() bool { return len(api.uploads()) == 1 })
	r.Eventually("the spool empty", func() bool { return upSpoolLen(states["laptop"]) == 0 })
	if !bytes.Equal(api.uploads()[0].data, data) {
		t.Fatal("the file is not what was queued")
	}
}

// A file the agent refuses to queue never reaches the hub, so there is nothing to upload and nothing to say in the chat.
func TestASecretIsRefusedOnTheMachineAndNeverReachesTheChat(t *testing.T) {
	bin := buildUpAgent(t)
	api := newUpAPI(t)
	r, states := upRigFor(t, api, testrig.Options{}, "laptop")
	r.StartAll()
	dir := t.TempDir()
	for _, p := range []string{upWrite(t, dir, ".env", []byte("TOKEN=abc")), upWrite(t, dir, "id_rsa", []byte("key"))} {
		cmd := exec.Command("env", "-i", "PB_STATE_DIR="+states["laptop"], bin, "send-file", p)
		if err := cmd.Run(); err == nil {
			t.Fatalf("%s was accepted", p)
		}
	}
	link := filepath.Join(dir, "cat.png")
	if err := os.Symlink(filepath.Join(dir, ".env"), link); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("env", "-i", "PB_STATE_DIR="+states["laptop"], bin, "send-file", link).Run(); err == nil {
		t.Fatal("a link to the environment file was accepted")
	}
	r.Never("anything in the chat", 600*time.Millisecond, func() bool { return len(api.uploads()) > 0 || upSpoolLen(states["laptop"]) > 0 })
}
