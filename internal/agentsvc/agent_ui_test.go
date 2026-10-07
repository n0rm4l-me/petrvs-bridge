package agentsvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// uiScreen is a screen the test sets.
type uiScreen struct {
	mu    sync.Mutex
	text  string
	err   error
	reads int
}

func (s *uiScreen) set(text string) { s.mu.Lock(); s.text, s.err = text, nil; s.mu.Unlock() }
func (s *uiScreen) Capture(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.text, s.err
}
func (s *uiScreen) PaneCommand(context.Context) (string, error) { return "claude", nil }
func (s *uiScreen) Start(_ context.Context, _, _ string) error  { return nil }
func (s *uiScreen) count() int                                  { s.mu.Lock(); defer s.mu.Unlock(); return s.reads }

func keyFor(key, prompt string) *wire.Payload {
	now := time.Now()
	p := &wire.Payload{ID: "k-" + key + prompt, Type: wire.TypeKey, Body: key, TS: now.Unix(), Exp: now.Add(time.Minute).Unix()}
	if prompt != "" {
		p.Meta = map[string]string{wire.MetaPrompt: prompt}
	}
	return p
}

func hashOf(t *testing.T, scr string) string {
	t.Helper()
	p, ok := screen.ExtractPrompt(scr)
	if !ok {
		t.Fatalf("no prompt on the screen:\n%s", scr)
	}
	return p.Hash
}

// press runs a key command the way the agent does, and returns what was pressed.
func press(t *testing.T, a *Agent, ex *recExec, key, prompt string) string {
	t.Helper()
	before := ex.got()
	a.execute(context.Background(), keyFor(key, prompt))
	return strings.TrimPrefix(strings.TrimPrefix(ex.got(), before), ",")
}

func uiAgent(t *testing.T, scr *uiScreen) (*Agent, *recExec) {
	t.Helper()
	a, ex, _ := withSession(t, func(c *Config) {
		if scr != nil {
			c.Screen = scr
		}
	})
	return a, ex
}

// The main safety property of the buttons: an old notice must not answer a new prompt. The two prompts here differ only
// in the command, and have the same choices.
func TestAKeyForAnEarlierPromptIsNotPressedForALaterOne(t *testing.T) {
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	old := hashOf(t, screentest.Bash(1, "ls /tmp"))

	scr.set(screentest.Bash(1, "ls /tmp"))
	if got := press(t, a, ex, "1", old); got != "k:1" {
		t.Fatalf("the prompt is on the screen and the key was not pressed: %q", got)
	}
	scr.set(screentest.Bash(1, "rm -rf /tmp/x"))
	if got := press(t, a, ex, "1", old); got != "" {
		t.Fatalf("the old button pressed %q for another command", got)
	}
	if got := press(t, a, ex, "Escape", old); got != "" {
		t.Fatalf("the old button pressed %q for another command", got)
	}
	// the new prompt's own button works
	if got := press(t, a, ex, "1", hashOf(t, scr.text)); got != "k:1" {
		t.Fatalf("%q", got)
	}
	if a.refused.Load() != 2 {
		t.Errorf("refused %d", a.refused.Load())
	}
}

// Two commands that differ in a value the chat is not shown (it is hidden as a secret) look the same there, so the only
// thing that keeps the old button from the new command is that the hash is taken before anything is hidden.
func TestPromptsThatLookTheSameOnceSecretsAreHiddenAreStillTold(t *testing.T) {
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	first, second := screentest.Bash(1, "deploy --token AAAA1111 prod"), screentest.Bash(1, "deploy --token BBBB2222 prod")
	p1, _ := screen.ExtractPrompt(first)
	p2, _ := screen.ExtractPrompt(second)
	if p1.Text != p2.Text {
		t.Fatalf("the fixture is wrong, the notices differ:\n%s\n%s", p1.Text, p2.Text)
	}
	scr.set(second)
	if got := press(t, a, ex, "1", p1.Hash); got != "" {
		t.Fatalf("pressed %q", got)
	}
}

// Someone moving the highlight on the laptop must not make a valid button fail.
func TestMovingTheHighlightDoesNotBreakAValidTap(t *testing.T) {
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	h := hashOf(t, screentest.Bash(1, "ls"))
	for cursor := 1; cursor <= 3; cursor++ {
		scr.set(screentest.Bash(cursor, "ls"))
		if got := press(t, a, ex, "3", h); got != "k:3" {
			t.Fatalf("highlight on %d: %q", cursor, got)
		}
	}
	scr.set(screentest.Bash(2, "ls"))
	if got := press(t, a, ex, "Escape", h); got != "k:Escape" {
		t.Fatalf("%q", got)
	}
}

func TestAKeyIsRefusedWhenThePromptIsGoneOrWasNeverOne(t *testing.T) {
	h := hashOf(t, screentest.Bash(1, "ls"))
	for name, text := range map[string]string{
		"an idle screen":                      screentest.Idle("auto mode on"),
		"an empty screen":                     "",
		"a numbered list in an answer":        screentest.ListInAnswer(),
		"another dialog":                      screentest.Question(1),
		"the list of sessions":                screentest.Resume(),
		"the same dialog with one line added": strings.Replace(screentest.Bash(1, "ls"), " Do you want to proceed?", " Do you really want to proceed?", 1),
	} {
		scr := &uiScreen{}
		a, ex := uiAgent(t, scr)
		scr.set(text)
		if got := press(t, a, ex, "1", h); got != "" {
			t.Errorf("%s: pressed %q", name, got)
		}
	}
}

// Only Escape and the number of one of the prompt's own choices answer a prompt. Enter would take whatever is highlighted
// when it arrives; BTab changes the permission mode; the rest move the highlight.
func TestOnlyEscapeAndTheNumbersOfTheChoicesAnswerAPrompt(t *testing.T) {
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	scr.set(screentest.Bash(1, "ls"))
	h := hashOf(t, scr.text)
	for _, key := range []string{"Enter", "BTab", "Up", "Down", "Tab", "4", "5", "9", "0", "x", "", "1 ", "Escape ", "escape"} {
		if got := press(t, a, ex, key, h); got != "" {
			t.Errorf("key %q was pressed for a prompt: %q", key, got)
		}
	}
	for _, key := range []string{"1", "2", "3", "Escape"} {
		if got := press(t, a, ex, key, h); got != "k:"+key {
			t.Errorf("key %q: %q", key, got)
		}
	}
}

func TestWithoutAScreenAKeyForAPromptIsNotPressed(t *testing.T) {
	a, ex := uiAgent(t, nil)
	if got := press(t, a, ex, "1", "0123abcd"); got != "" {
		t.Fatalf("pressed %q with no way to look at the screen", got)
	}
	scr := &uiScreen{err: errors.New("tmux broke")}
	a, ex = uiAgent(t, scr)
	if got := press(t, a, ex, "1", "0123abcd"); got != "" {
		t.Fatalf("pressed %q although the screen could not be read", got)
	}
	scr = &uiScreen{err: screen.ErrNoSession}
	a, ex = uiAgent(t, scr)
	if got := press(t, a, ex, "1", "0123abcd"); got != "" {
		t.Fatalf("pressed %q although the session is gone", got)
	}
}

func TestTheRefusalSaysWhatToDo(t *testing.T) {
	scr := &uiScreen{}
	a, _ := uiAgent(t, scr)
	scr.set(screentest.Idle("auto mode on"))
	err := a.checkPrompt(context.Background(), keyFor("1", "0123abcd"))
	var ue *userError
	if !errors.As(err, &ue) || !strings.Contains(ue.msg, "no longer on the screen") || !strings.Contains(ue.msg, "/screen") {
		t.Fatalf("%v", err)
	}
	scr.err = errors.New("boom")
	err = a.checkPrompt(context.Background(), keyFor("1", "0123abcd"))
	if !errors.As(err, &ue) || !strings.Contains(ue.msg, "cannot be read") || strings.Contains(ue.msg, "boom") {
		t.Fatalf("%v", err)
	}
}

// A key that answers no prompt (a key typed in the chat, a button that moves in a list) is pressed without looking at
// the screen, as before.
func TestAKeyWithoutAPromptIsPressedWithoutLookingAtTheScreen(t *testing.T) {
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	for _, key := range []string{"Enter", "Up", "BTab", "2"} {
		if got := press(t, a, ex, key, ""); got != "k:"+key {
			t.Errorf("%s: %q", key, got)
		}
	}
	if scr.count() != 0 {
		t.Errorf("the screen was read %d times", scr.count())
	}
}

func TestScreenRequestsAreLimitedToOneASecond(t *testing.T) {
	a, _ := uiAgent(t, nil)
	if !a.ui.allowScreen() {
		t.Fatal("the first request was refused")
	}
	if a.ui.allowScreen() {
		t.Fatal("a second request within the second was answered")
	}
	a.ui.mu.Lock()
	a.ui.lastScreen = time.Now().Add(-2 * time.Second)
	a.ui.mu.Unlock()
	if !a.ui.allowScreen() {
		t.Fatal("a request after the second was refused")
	}
}

// A button under a snapshot of the screen that showed no prompt is tied to no prompt. Enter or a number from it, pressed
// long after, would approve a prompt the user has not seen; the keys that cannot approve anything still go through.
func TestEnterFromAnOldSnapshotDoesNotApproveAPromptThatCameLater(t *testing.T) {
	fromSnapshot := func(key string) *wire.Payload {
		p := keyFor(key, "")
		p.Meta = map[string]string{wire.MetaEcho: "screen"}
		return p
	}
	run := func(a *Agent, ex *recExec, p *wire.Payload) string {
		before := ex.got()
		a.execute(context.Background(), p)
		return strings.TrimPrefix(strings.TrimPrefix(ex.got(), before), ",")
	}
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	a.ui.modeWait = time.Millisecond // the snapshot that follows each of these keys is not what is tested here

	scr.set(screentest.Resume()) // what the snapshot showed: a list, no prompt
	for _, key := range []string{"Enter", "Down", "2", "Escape"} {
		if got := run(a, ex, fromSnapshot(key)); got != "k:"+key {
			t.Errorf("%s with no prompt on the screen: %q", key, got)
		}
	}

	scr.set(screentest.Bash(1, "rm -rf /tmp/x")) // a prompt has come since
	for _, key := range []string{"Enter", "1", "2", "9"} {
		if got := run(a, ex, fromSnapshot(key)); got != "" {
			t.Errorf("%s from an old snapshot answered a prompt: %q", key, got)
		}
	}
	for _, key := range []string{"Up", "Down", "Tab", "Escape"} {
		if got := run(a, ex, fromSnapshot(key)); got != "k:"+key {
			t.Errorf("%s cannot approve anything and must go through: %q", key, got)
		}
	}
	// what the user types in the chat is their own decision
	if got := run(a, ex, keyFor("Enter", "")); got != "k:Enter" {
		t.Errorf("/enter: %q", got)
	}

	// a screen that cannot be read: nothing is approved blindly
	scr.err = errors.New("tmux broke")
	if got := run(a, ex, fromSnapshot("Enter")); got != "" {
		t.Errorf("Enter with the screen unreadable: %q", got)
	}
	if got := run(a, ex, fromSnapshot("Up")); got != "k:Up" {
		t.Errorf("Up: %q", got)
	}
}

// The hooks and the agent share a key (the state directory's prompt-key): a button made with another key, or none, is a
// button for a prompt the agent cannot vouch for, and it is refused even when the same dialog is on the screen.
func TestAButtonMadeWithAnotherKeyIsRefused(t *testing.T) {
	scr := &uiScreen{}
	a, ex := uiAgent(t, scr)
	scr.set(screentest.Bash(1, "ls"))
	unkeyed := hashOf(t, scr.text)

	t.Cleanup(func() { screen.SetHashKey(nil) })
	screen.SetHashKey([]byte("0123456789abcdef0123456789abcdef"))
	keyed := hashOf(t, scr.text)
	if keyed == unkeyed {
		t.Fatal("the key does not change the hash")
	}
	if got := press(t, a, ex, "1", unkeyed); got != "" {
		t.Fatalf("a button made without the key pressed %q", got)
	}
	screen.SetHashKey([]byte("another key another key another."))
	if got := press(t, a, ex, "1", keyed); got != "" {
		t.Fatalf("a button made with another key pressed %q", got)
	}
	screen.SetHashKey([]byte("0123456789abcdef0123456789abcdef"))
	if got := press(t, a, ex, "1", keyed); got != "k:1" {
		t.Fatalf("the button made with the agent's key: %q", got)
	}
}
