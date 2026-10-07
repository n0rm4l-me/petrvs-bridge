package hooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Stop hook also queues the pictures that Claude read during the turn, with `agent send-file`. These tests run the
// script with a stand-in for the agent that records how it was called.

const (
	imgHuman  = `{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"question"}}`
	imgResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
)

// imgRead is an assistant line that calls the tool `tool` on a file, as Claude Code writes it.
func imgTool(tool string, input string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":%q,"input":%s}]}}`, tool, input)
}

func imgRead(path string) string { return imgTool("Read", fmt.Sprintf(`{"file_path":%q}`, path)) }

func imgAnswer(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":%q}]}}`, text)
}

type imgRig struct {
	t          *testing.T
	dir        string
	transcript string
	env        []string
}

// fakeAgent records every call (one line per call, the arguments) and keeps what `send -` is given. FAKE_FAIL names the
// subcommand that exits with 1, FAKE_NOISE makes `send-file` print.
const fakeAgent = `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
[ -z "$FAKE_NOISE" ] || [ "$1" != send-file ] || { echo "noise on stdout"; echo "noise on stderr" >&2; }
if [ "$1" = send ]; then cat > "$FAKE_SENT.$$"; fi
[ "$1" != "$FAKE_FAIL" ] || exit 1
exit 0
`

func newImgRig(t *testing.T) *imgRig {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent"), []byte(fakeAgent), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &imgRig{t: t, dir: dir, transcript: filepath.Join(dir, "session.jsonl")}
	r.env = []string{"PB_STATE_DIR=" + filepath.Join(dir, "state"), "AGENT_BIN=" + filepath.Join(dir, "agent"),
		"FAKE_LOG=" + filepath.Join(dir, "calls.log"), "FAKE_SENT=" + filepath.Join(dir, "sent")}
	return r
}

// pic makes a file that exists and returns its path.
func (r *imgRig) pic(name string) string {
	r.t.Helper()
	p := filepath.Join(r.dir, "files", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *imgRig) write(lines ...string) {
	r.t.Helper()
	f, err := os.OpenFile(r.transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

func (r *imgRig) run(extra ...string) string {
	r.t.Helper()
	cmd := exec.Command("sh", "../../examples/claude-stop-hook.sh")
	cmd.Env = append(append(os.Environ(), r.env...), extra...)
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"transcript_path":%q}`, r.transcript))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("the hook failed: %v: %s", err, out.String())
	}
	return out.String()
}

// calls returns the arguments of each call of the agent, in order.
func (r *imgRig) calls() []string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "calls.log"))
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (r *imgRig) marks() int {
	files, _ := filepath.Glob(filepath.Join(r.dir, "state", "hook-*"))
	return len(files)
}

func TestThePicturesClaudeReadGoToTheChatAfterTheAnswer(t *testing.T) {
	r := newImgRig(t)
	a, b := r.pic("shot.png"), r.pic("Photo.JPG")
	r.write(imgHuman, imgRead(a), imgResult, imgRead(b), imgResult, imgAnswer("Here is what I saw."))
	if out := r.run(); out != "" {
		t.Fatalf("the hook printed %q", out)
	}
	want := []string{"send -", "send-file -- " + a, "send-file -- " + b}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestEveryKindOfPictureIsForwardedAndNothingElse(t *testing.T) {
	r := newImgRig(t)
	var lines []string
	lines = append(lines, imgHuman)
	// three at most, so one hook run looks at one group at a time
	for _, name := range []string{"a.png", "b.jpg", "c.jpeg"} {
		lines = append(lines, imgRead(r.pic(name)))
	}
	lines = append(lines, imgAnswer("done"))
	r.write(lines...)
	r.run()
	if got := r.calls(); len(got) != 4 {
		t.Fatalf("%q", got)
	}

	r = newImgRig(t)
	lines = []string{imgHuman}
	for _, name := range []string{"d.webp", "e.gif", "f.PNG"} {
		lines = append(lines, imgRead(r.pic(name)))
	}
	lines = append(lines, imgAnswer("done"))
	r.write(lines...)
	r.run()
	if got := r.calls(); len(got) != 4 {
		t.Fatalf("%q", got)
	}

	r = newImgRig(t)
	r.write(imgHuman,
		imgRead(r.pic("notes.txt")), imgRead(r.pic("photo.png.txt")), imgRead(r.pic("archive.zip")), imgRead(r.pic("png")),
		imgTool("Bash", fmt.Sprintf(`{"file_path":%q}`, r.pic("not-read.png"))),
		imgTool("Write", fmt.Sprintf(`{"file_path":%q}`, r.pic("written.png"))),
		imgTool("Read", `{"path":"/x/other-key.png"}`),
		imgAnswer("done"))
	r.run()
	if got := r.calls(); len(got) != 1 || got[0] != "send -" {
		t.Fatalf("something that is no picture read by Claude was forwarded: %q", got)
	}
}

func TestOnlyThePicturesThatStillExistAreForwarded(t *testing.T) {
	r := newImgRig(t)
	gone := filepath.Join(r.dir, "files", "deleted.png")
	dir := filepath.Join(r.dir, "files", "a-folder.png")
	os.MkdirAll(dir, 0o755)
	here := r.pic("here.png")
	r.write(imgHuman, imgRead(gone), imgRead(dir), imgRead(here), imgAnswer("done"))
	r.run()
	if got := r.calls(); len(got) != 2 || got[1] != "send-file -- "+here {
		t.Fatalf("%q", got)
	}
}

func TestAtMostThreeDifferentPicturesAreForwardedPerTurn(t *testing.T) {
	r := newImgRig(t)
	var lines []string
	lines = append(lines, imgHuman)
	var paths []string
	for i := 1; i <= 6; i++ {
		p := r.pic(fmt.Sprintf("p%d.png", i))
		paths = append(paths, p)
		lines = append(lines, imgRead(p))
		if i == 2 {
			lines = append(lines, imgRead(paths[0]), imgRead(paths[1])) // read again: not sent again
		}
	}
	lines = append(lines, imgAnswer("done"))
	r.write(lines...)
	r.run()
	want := []string{"send -", "send-file -- " + paths[0], "send-file -- " + paths[1], "send-file -- " + paths[2]}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestOnePictureReadTwiceInATurnIsSentOnce(t *testing.T) {
	r := newImgRig(t)
	p := r.pic("same.png")
	r.write(imgHuman, imgRead(p), imgRead(p), imgRead(p), imgAnswer("done"))
	r.run()
	if got := r.calls(); len(got) != 2 {
		t.Fatalf("%q", got)
	}
}

// What the hook marks as done it does not send again, pictures included; the next turn sends only its own.
func TestThePicturesOfATurnAreSentOnceAndThoseOfTheNextTurnAreItsOwn(t *testing.T) {
	r := newImgRig(t)
	first, second := r.pic("first.png"), r.pic("second.png")
	r.write(imgHuman, imgRead(first), imgAnswer("one"))
	r.run()
	r.run() // the next Stop of the same turn
	if got := r.calls(); len(got) != 2 {
		t.Fatalf("after two runs: %q", got)
	}
	r.write(imgHuman, imgRead(second), imgAnswer("two"))
	r.run()
	want := []string{"send -", "send-file -- " + first, "send -", "send-file -- " + second}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
	// a turn without a new question (a background task) sends what is new, and its pictures
	third := r.pic("third.png")
	r.write(imgRead(third), imgAnswer("three"))
	r.run()
	if got := r.calls(); len(got) != 6 || got[5] != "send-file -- "+third {
		t.Fatalf("%q", got)
	}
	if r.marks() != 1 {
		t.Fatalf("%d marks", r.marks())
	}
}

func TestPicturesReadBeforeTheLastQuestionAreNotForwarded(t *testing.T) {
	r := newImgRig(t)
	old, fresh := r.pic("old.png"), r.pic("fresh.png")
	r.write(imgHuman, imgRead(old), imgAnswer("earlier answer"), imgHuman, imgRead(fresh), imgAnswer("new answer"))
	r.run()
	want := []string{"send -", "send-file -- " + fresh}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

// Nothing is sent, and nothing marked, until the answer is there; the pictures wait with it.
func TestPicturesWaitForTheAnswerTheSameWayTheTextDoes(t *testing.T) {
	r := newImgRig(t)
	p := r.pic("wait.png")
	r.write(imgHuman, imgRead(p))
	r.run()
	if got := r.calls(); len(got) != 0 || r.marks() != 0 {
		t.Fatalf("calls %q, marks %d", got, r.marks())
	}
	r.write(imgResult, imgAnswer("now it is there"))
	r.run()
	want := []string{"send -", "send-file -- " + p}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestTheSwitchTurnsTheForwardingOff(t *testing.T) {
	for _, v := range []string{"off", "OFF", "0", "false", "no"} {
		r := newImgRig(t)
		r.write(imgHuman, imgRead(r.pic("a.png")), imgAnswer("done"))
		r.run("PB_FORWARD_IMAGES=" + v)
		if got := r.calls(); len(got) != 1 || got[0] != "send -" || r.marks() != 1 {
			t.Errorf("PB_FORWARD_IMAGES=%s: calls %q, marks %d", v, got, r.marks())
		}
	}
	for _, v := range []string{"", "on", "yes", "1"} {
		r := newImgRig(t)
		r.write(imgHuman, imgRead(r.pic("a.png")), imgAnswer("done"))
		r.run("PB_FORWARD_IMAGES=" + v)
		if got := r.calls(); len(got) != 2 {
			t.Errorf("PB_FORWARD_IMAGES=%q: calls %q", v, got)
		}
	}
}

// `agent send-file` refuses secrets, big files and a flood; none of that is the hook's business, and it must not stop the
// turn from being marked or make noise in Claude Code.
func TestARefusalFromTheAgentIsIgnoredAndMakesNoNoise(t *testing.T) {
	r := newImgRig(t)
	r.write(imgHuman, imgRead(r.pic("a.png")), imgRead(r.pic("b.png")), imgAnswer("done"))
	if out := r.run("FAKE_FAIL=send-file", "FAKE_NOISE=1"); strings.Contains(out, "noise") {
		t.Fatalf("the output of the agent reached Claude Code: %q", out)
	}
	if got := r.calls(); len(got) != 3 || r.marks() != 1 {
		t.Fatalf("calls %q, marks %d: the second picture must be tried although the first was refused", got, r.marks())
	}
}

// If the answer could not be queued the turn is not marked, so the next run tries again; the pictures go with the answer.
func TestNoPicturesAreSentWhenTheAnswerCouldNotBeQueued(t *testing.T) {
	r := newImgRig(t)
	p := r.pic("a.png")
	r.write(imgHuman, imgRead(p), imgAnswer("done"))
	r.run("FAKE_FAIL=send")
	if got := r.calls(); len(got) != 1 || r.marks() != 0 {
		t.Fatalf("calls %q, marks %d", got, r.marks())
	}
	r.run()
	want := []string{"send -", "send -", "send-file -- " + p}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

// What is in the transcript is what Claude wrote and what a tool's input held: a path can be anything.
func TestOddPathsAreHandedOverWhole_OrNotAtAll(t *testing.T) {
	r := newImgRig(t)
	spaced := r.pic("my holiday 'quoted' $HOME `x` (1).png")
	dashed := r.pic("-rf.png")
	newline := filepath.Join(r.dir, "files", "two\nlines.png")
	os.WriteFile(newline, []byte("x"), 0o644)
	r.write(imgHuman,
		`this line is not JSON {{{`,
		imgTool("Read", `{"file_path":12345}`),
		imgTool("Read", `{"file_path":{"nested":"x.png"}}`),
		imgTool("Read", `"a string"`),
		`{"type":"assistant","message":{"role":"assistant","content":"just a string"}}`,
		imgRead(newline),
		imgRead(spaced),
		imgRead(dashed),
		imgAnswer("done"))
	r.run()
	want := []string{"send -", "send-file -- " + spaced, "send-file -- " + dashed}
	if got := r.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestATurnWithoutPicturesCallsTheAgentOnlyForTheAnswer(t *testing.T) {
	r := newImgRig(t)
	r.write(imgHuman, imgAnswer("plain"))
	r.run()
	if got := r.calls(); len(got) != 1 || got[0] != "send -" {
		t.Fatalf("%q", got)
	}
}
