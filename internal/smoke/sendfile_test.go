package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/filetx"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// `agent send-file` is what the assistant and the Stop hook run to put a file into the chat. These tests run the built
// binary as a hook would: under `env -i`, with the state directory and nothing else.

func sfBuildAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agent")
	must(t, exec.Command("go", "build", "-o", bin, "github.com/n0rm4l-me/petrvs-bridge/cmd/agent"))
	return bin
}

// sfRun is runEmpty with a limit on the time: a command that waits on a pipe must fail the test, not hang it.
func sfRun(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "env", append(append([]string{"-i"}, env...), append([]string{bin}, args...)...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("agent %v did not finish in time", args)
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

type sfQueued struct {
	Type    wire.Type         `json:"type"`
	Body    string            `json:"body"`
	Meta    map[string]string `json:"meta"`
	Buttons []wire.Button     `json:"buttons"`
}

// sfQueuedItems reads the spool in queue order.
func sfQueuedItems(t *testing.T, state string) []sfQueued {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(state, "spool", "*.json"))
	sort.Strings(files)
	var out []sfQueued
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var q sfQueued
		if err := json.Unmarshal(b, &q); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, q)
	}
	return out
}

func sfWriteFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sfRandomFile(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestSendFileQueuesThePartsAndThenTheClosingItem(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	data := sfRandomFile(300_000, 1)
	path := sfWriteFile(t, filepath.Join(dir, "shot.png"), data)

	out, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send-file", "-caption", "the screen", path)
	if code != 0 || strings.TrimSpace(out) != "queued shot.png: 300000 bytes in 3 parts" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	items := sfQueuedItems(t, state)
	if len(items) != 4 {
		t.Fatalf("%d items queued", len(items))
	}
	var got []byte
	tr := items[3].Meta[wire.MetaTransfer]
	for i, it := range items[:3] {
		chunk, err := filetx.DecodePart(it.Body)
		if it.Type != wire.TypePart || err != nil || it.Meta[wire.MetaTransfer] != tr || it.Meta[wire.MetaIndex] != strconv.Itoa(i) || len(it.Meta) != 2 {
			t.Fatalf("item %d: %+v (%v)", i, it, err)
		}
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the parts do not add up to the file")
	}
	end := items[3]
	want := map[string]string{wire.MetaTransfer: tr, wire.MetaParts: "3", wire.MetaName: "shot.png", wire.MetaSize: "300000", wire.MetaSHA: filetx.SHA(data), wire.MetaKind: "photo"}
	if end.Type != wire.TypeFile || end.Body != "the screen" || fmt.Sprint(end.Meta) != fmt.Sprint(want) || len(tr) != 12 || strings.Trim(tr, "0123456789abcdef") != "" {
		t.Fatalf("closing item %+v, want %v", end, want)
	}
}

func TestSendFileDocumentsAndSmallFiles(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "notes.txt"), []byte("one line\n"))
	if _, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send-file", path); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	items := sfQueuedItems(t, state)
	if len(items) != 2 || items[1].Meta[wire.MetaKind] != "document" || items[1].Body != "" || items[1].Meta[wire.MetaParts] != "1" {
		t.Fatalf("%+v", items)
	}
}

func TestSendFileTakesFlagsAfterThePathAndPathsThatLookLikeFlags(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "a.txt"), []byte("x"))
	if _, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send-file", path, "-caption", "after"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if items := sfQueuedItems(t, state); len(items) != 2 || items[1].Body != "after" {
		t.Fatalf("%+v", items)
	}
	dashed := sfWriteFile(t, filepath.Join(dir, "-caption"), []byte("x"))
	t.Chdir(dir)
	state2 := t.TempDir()
	if out, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state2}, "send-file", "--", filepath.Base(dashed)); code != 0 || !strings.Contains(out, "queued -caption") {
		t.Fatalf("exit %d: %q %q", code, out, errs)
	}
}

func TestSendFileCutsALongCaptionAndKeepsItValid(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "a.txt"), []byte("x"))
	long := strings.Repeat("é", 2000)
	if _, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send-file", "-caption", "  \n"+long, path); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	body := sfQueuedItems(t, state)[1].Body
	if len(body) > 1000 || len(body) < 998 || !strings.HasPrefix(body, "éé") || strings.ToValidUTF8(body, "") != body {
		t.Fatalf("a caption of %d bytes", len(body))
	}
}

func TestSendFileUsageErrorsExitWithTwo(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "a.txt"), []byte("x"))
	env := []string{"PB_STATE_DIR=" + state}
	for name, args := range map[string][]string{
		"no path":         {"send-file"},
		"only a flag":     {"send-file", "-caption", "x"},
		"two paths":       {"send-file", path, path},
		"an unknown flag": {"send-file", "-nope", path},
	} {
		if _, _, code := sfRun(t, bin, env, args...); code != 2 {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	for _, v := range []string{"abc", "-1", "99999999999"} {
		if _, errs, code := sfRun(t, bin, append(env, "PB_MAX_FILE_BYTES="+v), "send-file", path); code != 2 || !strings.Contains(errs, "PB_MAX_FILE_BYTES") {
			t.Errorf("PB_MAX_FILE_BYTES=%s: exit %d, %q", v, code, errs)
		}
	}
	if _, _, code := sfRun(t, bin, env, "send-file", "-h"); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
	if n := len(sfQueuedItems(t, state)); n != 0 {
		t.Fatalf("%d items queued by commands that failed", n)
	}
}

func TestSendFileRefusesWhatIsNotAFileOfTheRightSize(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	env := []string{"PB_STATE_DIR=" + state, "PB_MAX_FILE_BYTES=1000"}
	empty := sfWriteFile(t, filepath.Join(dir, "empty.txt"), nil)
	big := sfWriteFile(t, filepath.Join(dir, "big.bin"), make([]byte, 1001))
	fits := sfWriteFile(t, filepath.Join(dir, "fits.bin"), make([]byte, 1000))
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.png")
	os.Symlink(filepath.Join(dir, "nowhere"), dangling)
	loop := filepath.Join(dir, "loop.png")
	os.Symlink(loop, loop)
	linkToDir := filepath.Join(dir, "linkdir.png")
	os.Symlink(sub, linkToDir)

	for name, c := range map[string]struct{ path, want string }{
		"an empty file":       {empty, "is empty"},
		"above the limit":     {big, "the most that can be sent is 1000"},
		"a directory":         {sub, "not a regular file"},
		"a link to one":       {linkToDir, "not a regular file"},
		"a pipe":              {fifo, "not a regular file"},
		"a missing file":      {filepath.Join(dir, "missing.txt"), "cannot read"},
		"a dangling link":     {dangling, "cannot read"},
		"a link that loops":   {loop, "cannot read"},
		"a device":            {"/dev/null", "not a regular file"},
		"a path with nothing": {dir + "/", "not a regular file"},
	} {
		out, errs, code := sfRun(t, bin, env, "send-file", c.path)
		if code != 1 || out != "" || !strings.Contains(errs, c.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
	if n := len(sfQueuedItems(t, state)); n != 0 {
		t.Fatalf("%d items queued by refused files", n)
	}
	if _, errs, code := sfRun(t, bin, env, "send-file", fits); code != 0 {
		t.Fatalf("a file of exactly the limit: exit %d: %s", code, errs)
	}
}

func TestSendFileRefusesSecretsHoweverTheyAreReached(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	env := []string{"PB_STATE_DIR=" + state}
	key := sfWriteFile(t, filepath.Join(dir, ".ssh", "id_ed25519"), []byte("private"))
	dotenv := sfWriteFile(t, filepath.Join(dir, "project", ".env"), []byte("TOKEN=x"))
	inside := sfWriteFile(t, filepath.Join(dir, ".aws", "config.txt"), []byte("x"))
	named := sfWriteFile(t, filepath.Join(dir, "my-credentials.json"), []byte("{}"))
	pem := sfWriteFile(t, filepath.Join(dir, "notes.txt"), []byte("just some notes\n-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"))
	harmless := sfWriteFile(t, filepath.Join(dir, "harmless.txt"), []byte("hello"))
	ownState := sfWriteFile(t, filepath.Join(state, "spool", "not-a-message.txt"), []byte("hello"))
	ownKeys := sfWriteFile(t, filepath.Join(state, "keys", "agent.txt"), []byte("hello"))
	disguised := filepath.Join(dir, "holiday.png")
	os.Symlink(key, disguised) // a link named like a photo that points at a key
	viaDir := filepath.Join(dir, "shortcut")
	os.Symlink(filepath.Join(dir, ".ssh"), viaDir) // a link to the directory of keys
	named2 := filepath.Join(dir, "id_rsa")
	os.Symlink(harmless, named2) // a link named like a key that points at something harmless: refused too, to be safe
	stateLink := filepath.Join(dir, "state-link.txt")
	os.Symlink(ownState, stateLink)

	for name, c := range map[string]struct{ path, want string }{
		"a key":                    {key, "id_ed25519"},
		"an environment file":      {dotenv, ".env"},
		"inside a secrets folder":  {inside, ".aws"},
		"named like credentials":   {named, "credential"},
		"a private key in notes":   {pem, "private key"},
		"a link to a key":          {disguised, "id_ed25519"},
		"a link to the key folder": {filepath.Join(viaDir, "id_ed25519"), "id_ed25519"},
		"a link named like a key":  {named2, "id_rsa"},
		"the agent's own files":    {ownState, state},
		"the agent's own folder":   {ownKeys, state},
		"a link to its own files":  {stateLink, state},
	} {
		out, errs, code := sfRun(t, bin, env, "send-file", c.path)
		if code != 1 || out != "" || !strings.Contains(errs, c.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
	if n := len(sfQueuedItems(t, state)); n != 0 {
		t.Fatalf("%d items of refused files were queued", n)
	}
	if _, errs, code := sfRun(t, bin, env, "send-file", harmless); code != 0 {
		t.Fatalf("a harmless file was refused: %s", errs)
	}
}

// The identity and the alarm secret of the agent are named in the environment of the agent, which a hook does not always
// have; when it does, they are not sent whatever they are called.
func TestSendFileRefusesTheIdentityAndTheAlarmSecretWhenTheyAreNamed(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	id := sfWriteFile(t, filepath.Join(dir, "keys", "laptop.txt"), []byte("x"))
	sibling := sfWriteFile(t, filepath.Join(dir, "keys", "other.txt"), []byte("x"))
	secret := sfWriteFile(t, filepath.Join(dir, "alarm.txt"), []byte("x"))
	env := []string{"PB_STATE_DIR=" + state, "PB_IDENTITY=" + id, "PB_ALARM_SECRET_FILE=" + secret}
	for _, p := range []string{id, sibling, secret} {
		if _, errs, code := sfRun(t, bin, env, "send-file", p); code != 1 {
			t.Errorf("%s was not refused (exit %d, %q)", p, code, errs)
		}
	}
}

func TestSendFileFollowsALinkToAnOrdinaryFileAndKeepsTheLinksName(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	target := sfWriteFile(t, filepath.Join(dir, "deep", "report-final-v3.pdf"), []byte("%PDF"))
	link := filepath.Join(dir, "latest.pdf")
	os.Symlink(target, link)
	out, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send-file", link)
	if code != 0 || !strings.Contains(out, "queued latest.pdf") {
		t.Fatalf("exit %d: %q %q", code, out, errs)
	}
}

func TestSendFileRefusesTheEleventhFileInAMinute(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "a.txt"), []byte("x"))
	env := []string{"PB_STATE_DIR=" + state}
	for i := 1; i <= 10; i++ {
		if _, errs, code := sfRun(t, bin, env, "send-file", path); code != 0 {
			t.Fatalf("file %d: exit %d: %s", i, code, errs)
		}
	}
	out, errs, code := sfRun(t, bin, env, "send-file", path)
	if code != 1 || out != "" || !strings.Contains(errs, "at most 10 a minute") {
		t.Fatalf("the eleventh: exit %d, %q %q", code, out, errs)
	}
	if n := len(sfQueuedItems(t, state)); n != 20 {
		t.Fatalf("%d items; the refused file left something behind", n)
	}
	// Files sent long enough ago no longer count; the record is a plain file in the state directory.
	times := filepath.Join(state, "send-file-times")
	var old strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&old, "%d\n", time.Now().Add(-61*time.Second).UnixNano())
	}
	if err := os.WriteFile(times, []byte(old.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := sfRun(t, bin, env, "send-file", path); code != 0 {
		t.Fatalf("after a minute: exit %d: %s", code, errs)
	}
	if st, err := os.Stat(times); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the record of times: %v %v", st, err)
	}
	// Times from the future do not block for ever either (the clock was set back).
	var future strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&future, "%d\n", time.Now().Add(time.Hour).UnixNano())
	}
	os.WriteFile(times, []byte(future.String()), 0o600)
	if _, errs, code := sfRun(t, bin, env, "send-file", path); code != 0 {
		t.Fatalf("after the clock was set back: exit %d: %s", code, errs)
	}
	// Junk in the record is ignored, not fatal.
	os.WriteFile(times, []byte("not a number\n\x00\x01\n"), 0o600)
	if _, errs, code := sfRun(t, bin, env, "send-file", path); code != 0 {
		t.Fatalf("with junk in the record: exit %d: %s", code, errs)
	}
}

func TestSendFileCountsFilesSentAtTheSameTimeTogether(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "a.txt"), []byte("x"))
	env := []string{"PB_STATE_DIR=" + state}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, code := sfRun(t, bin, env, "send-file", path)
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[0] != 10 || codes[1] != 6 {
		t.Fatalf("exit codes %v: the limit is per state directory, however many hooks run at once", codes)
	}
	if n := len(sfQueuedItems(t, state)); n != 20 {
		t.Fatalf("%d items", n)
	}
}

func TestSendFileRefusesWhenTheQueueIsFullAndLeavesItAsItWas(t *testing.T) {
	bin := sfBuildAgent(t)
	state, dir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	room := spool.MaxFiles - spool.FileReserve
	for i := 0; i < room-2; i++ { // room for one part and no closing item after it
		name := fmt.Sprintf("%020d-1-%08d.json", i+1, i)
		if err := os.WriteFile(filepath.Join(state, "spool", name), []byte(`{"type":"reply","body":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := sfWriteFile(t, filepath.Join(dir, "two-parts.bin"), make([]byte, 200_000)) // 2 parts and the closing item
	out, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send-file", path)
	if code != 1 || out != "" || !strings.Contains(errs, "queue is full") {
		t.Fatalf("exit %d: %q %q", code, out, errs)
	}
	if n := len(sfQueuedItems(t, state)); n != room-2 {
		t.Fatalf("%d items; the refused file left parts behind", n)
	}
	// replies still fit: the room kept for them is not used by files
	if _, errs, code := sfRun(t, bin, []string{"PB_STATE_DIR=" + state}, "send", "-type", "notice", "still room for a notice"); code != 0 {
		t.Fatalf("a notice was refused: %s", errs)
	}
}

func TestSendFileWorksLikeSendWithTheDefaultStateDirectory(t *testing.T) {
	bin := sfBuildAgent(t)
	home, dir := t.TempDir(), t.TempDir()
	path := sfWriteFile(t, filepath.Join(dir, "a.txt"), []byte("x"))
	if _, errs, code := sfRun(t, bin, []string{"HOME=" + home}, "send-file", path); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if items := sfQueuedItems(t, filepath.Join(home, ".local", "state", "petrvs-bridge")); len(items) != 2 {
		t.Fatalf("%d items", len(items))
	}
}
