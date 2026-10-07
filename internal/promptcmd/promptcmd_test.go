package promptcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

const (
	permission = `{"message":"Claude needs your permission","notification_type":"permission_prompt"}`
	plain      = "⏳ Claude needs your permission. Answer with /key N (the number of your choice), /enter or /esc."
)

type queue struct {
	items []spool.Item
	err   error
}

func (q *queue) AddItem(it spool.Item) error {
	if q.err != nil {
		return q.err
	}
	q.items = append(q.items, it)
	return nil
}

// pane is a tmux pane that shows the screens one after the other (the last one stays), or fails.
type pane struct {
	screens []string
	err     error
	asked   []string
}

func (p *pane) capture(_ context.Context, id string) (string, error) {
	p.asked = append(p.asked, id)
	if p.err != nil {
		return "", p.err
	}
	i := min(len(p.asked)-1, len(p.screens)-1)
	if i < 0 {
		return "", nil
	}
	return p.screens[i], nil
}

// notify runs the command as the hook does, in pane %1 showing the screens, and returns what it queued.
func notify(t *testing.T, payload string, p *pane) []spool.Item {
	t.Helper()
	q := &queue{}
	o := Options{Stdin: strings.NewReader(payload), Queue: q, Tries: 4, Pause: time.Millisecond}
	if p != nil {
		o.Pane, o.Capture = "%1", p.capture
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for _, it := range q.items {
		if it.Type != wire.TypeNotice {
			t.Errorf("queued a %s", it.Type)
		}
		// Whatever is queued must be something the hub accepts: a payload it rejects is lost without a trace.
		pl := &wire.Payload{ID: "x", Type: it.Type, Body: it.Body, Buttons: it.Buttons, TS: 1, Exp: 2}
		if err := pl.Validate(true); err != nil {
			t.Errorf("the hub would reject it: %v: %+v", err, it.Buttons)
		}
	}
	return q.items
}

func one(t *testing.T, items []spool.Item) spool.Item {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("queued %d notices: %+v", len(items), items)
	}
	return items[0]
}

// Claude Code sends idle_prompt a minute after every finished turn, and auth_success when a login completes: neither
// needs an answer, and forwarding them filled the chat with "waiting for your input" that no prompt stood behind.
func TestNotificationsThatNeedNoAnswerAreNotForwarded(t *testing.T) {
	for _, typ := range []string{"idle_prompt", "auth_success"} {
		for name, p := range map[string]*pane{"no pane": nil, "a prompt on the screen": {screens: []string{screentest.Bash(1, "ls")}}} {
			if got := notify(t, fmt.Sprintf(`{"hook_event_name":"Notification","message":"Claude is waiting for your input","notification_type":%q}`, typ), p); len(got) != 0 {
				t.Errorf("%s (%s) was forwarded: %+v", typ, name, got)
			}
			if p != nil && len(p.asked) != 0 {
				t.Errorf("%s (%s): the screen was read for a notification that is dropped", typ, name)
			}
		}
	}
}

func TestNotificationsThatNeedAnAnswerAreForwarded(t *testing.T) {
	for name, payload := range map[string]string{
		"permission prompt":           `{"message":"Claude needs your permission","notification_type":"permission_prompt"}`,
		"elicitation dialog":          `{"message":"Claude needs your permission","notification_type":"elicitation_dialog"}`,
		"a type from a later version": `{"message":"Claude needs your permission","notification_type":"something_new"}`,
		"no type (older Claude Code)": `{"message":"Claude needs your permission"}`,
	} {
		it := one(t, notify(t, payload, nil))
		if it.Body != plain || len(it.Buttons) != 0 {
			t.Errorf("%s: %+v", name, it)
		}
	}
}

func TestANotificationWithoutAMessageIsNotForwarded(t *testing.T) {
	for _, in := range []string{`{"notification_type":"permission_prompt"}`, `{"message":"  \n ","notification_type":"permission_prompt"}`, `not json`, ``, `{"message":5}`, `[]`} {
		if got := notify(t, in, nil); len(got) != 0 {
			t.Errorf("%q: %+v", in, got)
		}
	}
}

// A phone cannot approve what it cannot see. The prompt is not in the transcript while it waits (Claude Code writes the
// tool call only after the answer), so the notice carries the prompt from the screen of the pane: title, command, the
// question and the numbered choices, without the lines that only decorate it and without the conversation above.
func TestThePermissionNoticeShowsThePromptFromTheScreenWithAButtonForEachChoice(t *testing.T) {
	p := &pane{screens: []string{screentest.Bash(1, "ls -la /tmp")}}
	it := one(t, notify(t, permission, p))
	want := "⏳ Claude needs your permission:\n" + screentest.Lines(
		" Bash command",
		" List the files",
		" │ ls -la /tmp",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and switch to auto mode",
		"   3. No",
		" Esc to cancel · Tab to amend",
	) + "\n" + answerHint
	if it.Body != want {
		t.Fatalf("got %q\nwant %q", it.Body, want)
	}
	if len(p.asked) != 1 || p.asked[0] != "%1" {
		t.Errorf("the pane was read as %q", p.asked)
	}
	pr, _ := screen.ExtractPrompt(screentest.Bash(1, "ls -la /tmp"))
	wantButtons := []wire.Button{
		{Label: "1. Yes", Key: "1", Prompt: pr.Hash},
		{Label: "2. Yes, and switch to auto mo…", Key: "2", Prompt: pr.Hash},
		{Label: "3. No", Key: "3", Prompt: pr.Hash},
		{Label: "✖ Esc", Key: "Escape", Prompt: pr.Hash},
	}
	if fmt.Sprint(it.Buttons) != fmt.Sprint(wantButtons) {
		t.Fatalf("buttons %+v\nwant    %+v", it.Buttons, wantButtons)
	}
}

// The buttons are bound to the prompt on the screen when the notice was made: the hash of a prompt for another command
// differs, and the highlight moving does not change it.
func TestTheButtonsCarryTheHashOfThePromptAndNothingElse(t *testing.T) {
	hash := func(s string) string {
		it := one(t, notify(t, permission, &pane{screens: []string{s}}))
		h := it.Buttons[0].Prompt
		for _, b := range it.Buttons {
			if b.Prompt != h || h == "" {
				t.Errorf("the buttons of one notice carry different hashes: %+v", it.Buttons)
			}
		}
		return h
	}
	a, b, c := hash(screentest.Bash(1, "ls /tmp")), hash(screentest.Bash(3, "ls /tmp")), hash(screentest.Bash(1, "ls /tmp2"))
	if a != b {
		t.Error("the highlight moved and the hash with it")
	}
	if a == c {
		t.Error("two prompts that differ only in the command have the same hash")
	}
}

// A question with choices is a prompt too, whichever type Claude Code gives it.
func TestAQuestionWithChoicesIsShownToo(t *testing.T) {
	for name, payload := range map[string]string{
		"elicitation dialog": `{"message":"Claude needs your input","notification_type":"elicitation_dialog"}`,
		"permission prompt":  `{"message":"Claude needs your input","notification_type":"permission_prompt"}`,
		"no type":            `{"message":"Claude needs your input"}`,
	} {
		it := one(t, notify(t, payload, &pane{screens: []string{screentest.Question(1)}}))
		if !strings.HasPrefix(it.Body, "⏳ Claude needs your input:\n Which colour do you want?\n ❯ 1. Red\n   2. Blue\n") || strings.Contains(it.Body, "I need to know") {
			t.Errorf("%s: %q", name, it.Body)
		}
		if len(it.Buttons) != 4 || it.Buttons[1].Label != "2. Blue" || it.Buttons[3].Key != "Escape" {
			t.Errorf("%s: %+v", name, it.Buttons)
		}
	}
}

// With no prompt on the screen (an idle input box, say), or with no tmux to ask, nothing from the screen is sent.
func TestWithoutAPromptOnTheScreenTheNoticeIsPlain(t *testing.T) {
	for name, p := range map[string]*pane{
		"an idle input box":       {screens: []string{screentest.Idle("⏸ manual mode on · ? for shortcuts")}},
		"tmux fails":              {screens: []string{screentest.Bash(1, "ls")}, err: errors.New("no server running")},
		"an empty screen":         {screens: []string{""}},
		"a numbered list in text": {screens: []string{screentest.ListInAnswer()}},
	} {
		it := one(t, notify(t, permission, p))
		if it.Body != plain || len(it.Buttons) != 0 {
			t.Errorf("%s: %+v", name, it)
		}
	}
	if it := one(t, notify(t, permission, nil)); it.Body != plain || len(it.Buttons) != 0 {
		t.Errorf("no pane: %+v", it)
	}
}

// The prompt may not be drawn when the notification fires: the screen is read again for a moment, but not for a pane that
// cannot be read at all.
func TestThePromptIsWaitedForButNotForeverAndNotOnAnError(t *testing.T) {
	late := &pane{screens: []string{screentest.Idle("auto mode on"), screentest.Idle("auto mode on"), screentest.Bash(1, "ls")}}
	if it := one(t, notify(t, permission, late)); len(it.Buttons) != 4 || len(late.asked) != 3 {
		t.Errorf("after %d reads: %+v", len(late.asked), it)
	}
	never := &pane{screens: []string{screentest.Idle("auto mode on")}}
	if it := one(t, notify(t, permission, never)); len(it.Buttons) != 0 || len(never.asked) != 4 {
		t.Errorf("after %d reads: %+v", len(never.asked), it)
	}
	broken := &pane{err: errors.New("boom")}
	if it := one(t, notify(t, permission, broken)); len(it.Buttons) != 0 || len(broken.asked) != 1 {
		t.Errorf("a pane that cannot be read was read %d times", len(broken.asked))
	}
}

// $TMUX_PANE is not trusted beyond what tmux puts there.
func TestOnlyARealPaneIdIsLookedAt(t *testing.T) {
	for _, id := range []string{"", "1", "%", "%1;x", "-t", "%12345678901", "%1 %2", "=sess:", "%1\n"} {
		p := &pane{screens: []string{screentest.Bash(1, "ls")}}
		q := &queue{}
		err := Run(context.Background(), Options{Stdin: strings.NewReader(permission), Queue: q, Pane: id, Capture: p.capture, Tries: 1})
		if err != nil || len(q.items) != 1 || len(p.asked) != 0 || len(q.items[0].Buttons) != 0 {
			t.Errorf("pane %q: read %v, queued %+v, %v", id, p.asked, q.items, err)
		}
	}
}

// Best effort: a command can carry a secret, and the notice goes to a chat. Values after secret-like words are hidden,
// quoted ones whole, and the password of a URL; a long prompt is cut.
func TestTheNoticeHidesSecretsAndCutsLongPrompts(t *testing.T) {
	cases := map[string][]string{
		`curl -H "Authorization: Bearer abc123def" https://example.org`: {"abc123def"},
		`run --token s3cr3t --flag`:                                     {"s3cr3t"},
		`run password="two words here" next`:                            {"two words", "words here"},
		`run --token 'a b c' next`:                                      {"a b c"},
		`psql postgres://user:hunter2@db/x`:                             {"hunter2"},
		`export API_KEY=k-12345`:                                        {"k-12345"},
	}
	for command, leaks := range cases {
		it := one(t, notify(t, permission, &pane{screens: []string{screentest.Bash(1, command)}}))
		if !strings.Contains(it.Body, "<hidden>") {
			t.Errorf("%q: %q", command, it.Body)
			continue
		}
		for _, leak := range leaks {
			if strings.Contains(it.Body, leak) {
				t.Errorf("%q: the notice shows %q: %q", command, leak, it.Body)
			}
		}
	}
	it := one(t, notify(t, permission, &pane{screens: []string{screentest.Bash(1, strings.Repeat("x", 2500))}}))
	if !strings.Contains(it.Body, "characters left out]") || utf8.RuneCountInString(it.Body) > 1800+400 {
		t.Errorf("not cut (%d runes)", utf8.RuneCountInString(it.Body))
	}
}

// A command that is too long to show cannot be judged from the chat. Its tail may be the dangerous part, and the
// question and the choices come after it, so they are kept and the middle is left out; and the only button is Esc, so
// that a tap cannot approve what was not shown.
func TestATooLongCommandOffersOnlyEscAndSaysWhy(t *testing.T) {
	var frame []string
	for i := 0; i < 100; i++ {
		frame = append(frame, fmt.Sprintf("echo step-%03d padding padding padding;", i))
	}
	long := strings.Join(frame, " ") + " rm -rf ~/the-tail"
	scr := screentest.Bash(1, long)
	pr, _ := screen.ExtractPrompt(scr)
	it := one(t, notify(t, permission, &pane{screens: []string{scr}}))
	for _, want := range []string{"characters left out]", "Do you want to proceed?", "❯ 1. Yes", "3. No", tooLong, longHint} {
		if !strings.Contains(it.Body, want) {
			t.Errorf("the notice lacks %q:\n%s", want, it.Body)
		}
	}
	if strings.Contains(it.Body, answerHint) {
		t.Error("the notice points to buttons that are not there")
	}
	if len(it.Buttons) != 1 || it.Buttons[0] != (wire.Button{Label: "✖ Esc", Key: "Escape", Prompt: pr.Hash}) {
		t.Fatalf("buttons %+v", it.Buttons)
	}
	// the same when the hub asks for the screen
	n := Snapshot("laptop", scr)
	if len(n.Buttons) != 1 || n.Buttons[0].Key != "Escape" || !strings.Contains(n.Body, tooLong) {
		t.Errorf("the snapshot: %+v", n)
	}
}

func TestAPromptWithHiddenValuesKeepsItsButtonsAndSaysSo(t *testing.T) {
	it := one(t, notify(t, permission, &pane{screens: []string{screentest.Bash(1, "export TOKEN=abc123;ls")}}))
	if len(it.Buttons) != 4 || !strings.Contains(it.Body, "│ export TOKEN=<hidden>;ls\n") {
		t.Fatalf("%+v", it)
	}
	if !strings.Contains(it.Body, "\n"+hiddenNote+"\n\n"+answerHint) || strings.Contains(it.Body, tooLong) {
		t.Errorf("%q", it.Body)
	}
	if plain := one(t, notify(t, permission, &pane{screens: []string{screentest.Bash(1, "ls")}})); strings.Contains(plain.Body, hiddenNote) {
		t.Errorf("nothing was hidden, and the notice says it was: %q", plain.Body)
	}
}

// The labels of the buttons come from the screen too, and nothing hides them but us.
func TestTheButtonsHideSecretsToo(t *testing.T) {
	scr := screentest.Many(3, func(i int) string {
		return []string{"Yes", "Yes, and don't ask again for curl with token=abc123secret", "No"}[i-1]
	})
	it := one(t, notify(t, permission, &pane{screens: []string{scr}}))
	for _, b := range it.Buttons {
		if strings.Contains(b.Label, "abc123secret") {
			t.Errorf("a button shows the secret: %q", b.Label)
		}
	}
	if strings.Contains(it.Body, "abc123secret") {
		t.Errorf("the text shows the secret: %q", it.Body)
	}
}

func TestPunctuationAtTheEndOfTheMessageIsNotDoubled(t *testing.T) {
	msg := func(m string) string { return fmt.Sprintf(`{"message":%q,"notification_type":"permission_prompt"}`, m) }
	it := one(t, notify(t, msg("Claude needs your permission to use Bash."), &pane{screens: []string{screentest.Bash(1, "ls")}}))
	if !strings.HasPrefix(it.Body, "⏳ Claude needs your permission to use Bash:\n Bash command\n") {
		t.Errorf("%q", it.Body)
	}
	for in, want := range map[string]string{
		"Claude needs your permission to use Bash.": "⏳ Claude needs your permission to use Bash. " + plainHint,
		"Claude needs you!?":                        "⏳ Claude needs you. " + plainHint,
		"Waiting...":                                "⏳ Waiting. " + plainHint,
		"two\nlines":                                "⏳ two lines. " + plainHint,
	} {
		if it := one(t, notify(t, msg(in), nil)); it.Body != want {
			t.Errorf("%q: %q", in, it.Body)
		}
	}
}

// A prompt has at most 8 choices and a notice at most 8 buttons; Esc must stay among them, since it is the way out.
// A label is at most 40 bytes on the wire, and a character can be four of them.
func TestButtonsStayWithinWhatTheWireCarries(t *testing.T) {
	eight := screentest.Many(8, func(i int) string { return fmt.Sprintf("choice %d", i) })
	it := one(t, notify(t, permission, &pane{screens: []string{eight}}))
	if len(it.Buttons) != wire.MaxButtons || it.Buttons[7].Key != "Escape" || it.Buttons[6].Key != "7" {
		t.Errorf("%+v", it.Buttons)
	}
	if !strings.Contains(it.Body, "8. choice 8") {
		t.Error("the choice without a button is not in the text")
	}
	wide := screentest.Many(3, func(i int) string { return strings.Repeat("日本語のとても長い選択肢", 5) })
	it = one(t, notify(t, permission, &pane{screens: []string{wide}}))
	for _, b := range it.Buttons {
		if len(b.Label) > 40 || !utf8.ValidString(b.Label) {
			t.Errorf("label %q is %d bytes", b.Label, len(b.Label))
		}
	}
	if len(it.Buttons) != 4 || !strings.HasPrefix(it.Buttons[0].Label, "1. 日本語") {
		t.Errorf("%+v", it.Buttons)
	}
}

func TestANoticeThatCannotBeQueuedIsAnError(t *testing.T) {
	q := &queue{err: spool.ErrFull}
	err := Run(context.Background(), Options{Stdin: strings.NewReader(permission), Queue: q})
	if !errors.Is(err, spool.ErrFull) {
		t.Fatalf("%v", err)
	}
}

func TestInputIsBounded(t *testing.T) {
	huge := `{"message":"` + strings.Repeat("a", 2<<20) + `","notification_type":"permission_prompt"}`
	if got := notify(t, huge, nil); len(got) != 0 {
		t.Fatalf("queued %d bytes for an input past the limit", len(got[0].Body))
	}
	long := fmt.Sprintf(`{"message":%q}`, strings.Repeat("word ", 5000))
	if it := one(t, notify(t, long, nil)); utf8.RuneCountInString(it.Body) > 600 {
		t.Errorf("a long message was not cut: %d runes", utf8.RuneCountInString(it.Body))
	}
}

// The screen as the agent sends it on request: the prompt with buttons when there is one, else the bottom lines with
// buttons to move, and no hash on those (there is no prompt to tie them to).
func TestASnapshotIsThePromptOrTheBottomOfTheScreen(t *testing.T) {
	n := Snapshot("laptop", screentest.Bash(1, "ls"))
	if !strings.HasPrefix(n.Body, "⏳ Claude Code is waiting for an answer:\n Bash command") || len(n.Buttons) != 4 || n.Buttons[0].Prompt == "" {
		t.Errorf("%+v", n)
	}

	n = Snapshot("laptop", screentest.Resume())
	if !strings.HasPrefix(n.Body, "📺 laptop's screen:\n") || !strings.Contains(n.Body, "Resume Session") || !strings.Contains(n.Body, "fix the build") {
		t.Errorf("%q", n.Body)
	}
	var keys []string
	for _, b := range n.Buttons {
		if b.Prompt != "" {
			t.Errorf("a button of the screen is tied to a prompt: %+v", b)
		}
		keys = append(keys, b.Label+"="+b.Key)
	}
	if got := strings.Join(keys, " "); got != "↑=Up ↓=Down ⏎=Enter ✖ Esc=Escape" {
		t.Errorf("%s", got)
	}

	// an answer with a numbered list is not a prompt, so there is no button that would type a number
	n = Snapshot("laptop", screentest.ListInAnswer())
	if !strings.HasPrefix(n.Body, "📺") || !strings.Contains(n.Body, "1. first") || n.Buttons[0].Key != "Up" {
		t.Errorf("%+v", n)
	}
	for _, b := range n.Buttons {
		if b.Key >= "1" && b.Key <= "9" {
			t.Errorf("a number button for a list in an answer: %+v", b)
		}
	}
}

func TestASnapshotKeepsTheBottomHidesSecretsAndSurvivesAnEmptyScreen(t *testing.T) {
	var ls []string
	for i := 1; i <= 60; i++ {
		ls = append(ls, fmt.Sprintf("line %d", i))
	}
	ls = append(ls, "export TOKEN=hunter2secret")
	n := ForScreen("laptop", screentest.Lines(ls...))
	if strings.Contains(n.Body, "hunter2secret") || !strings.Contains(n.Body, "<hidden>") {
		t.Errorf("secret: %q", n.Body)
	}
	if strings.Contains(n.Body, "line 30\n") || !strings.Contains(n.Body, "line 60") || strings.Count(n.Body, "\n") != 25 {
		t.Errorf("not the bottom 25 lines: %q", n.Body)
	}

	wide := screentest.Lines(strings.Repeat("w", 400), strings.Repeat("x", 400), strings.Repeat("y", 400), strings.Repeat("z", 400), strings.Repeat("v", 400),
		strings.Repeat("u", 400), strings.Repeat("t", 400), strings.Repeat("s", 400), strings.Repeat("r", 400), strings.Repeat("q", 400))
	n = ForScreen("laptop", wide)
	if utf8.RuneCountInString(n.Body) > 3700 || !strings.Contains(n.Body, "qqqq") || strings.Contains(n.Body, "wwww") {
		t.Errorf("not kept to the bottom: %d runes", utf8.RuneCountInString(n.Body))
	}

	if n := ForScreen("laptop", "  \n\n"); !strings.Contains(n.Body, "(the screen is empty)") || len(n.Buttons) != 4 {
		t.Errorf("%+v", n)
	}
}
