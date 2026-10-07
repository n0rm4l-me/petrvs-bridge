package screen

import (
	"strconv"
	"strings"
	"testing"
)

var (
	rule   = strings.Repeat("─", 80)
	dashed = strings.Repeat("╌", 80)
)

func screenOf(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// bashDialog is a permission prompt as Claude Code 2.1.292 draws it (captured from a real session), under some
// conversation. The highlighted choice and the command vary.
func bashDialog(cursor int, command string) string {
	opt := func(n int, text string) string {
		mark := "  "
		if n == cursor {
			mark = "❯ "
		}
		return " " + mark + strings.TrimSpace(strings.Repeat(" ", 0)+strconv.Itoa(n)+". "+text)
	}
	return screenOf(
		"⏺ Let me look at the directory.",
		"  1. an earlier list in the answer",
		"  2. that is not a prompt",
		rule,
		" Bash command",
		" Tip: auto mode handles these prompts for you — choose \"switch to auto mode\"",
		" below",
		" Check bridge repo status, binaries, processes and log",
		dashed,
		" │ "+command,
		dashed,
		" Contains backslash-escaped whitespace",
		" Do you want to proceed?",
		opt(1, "Yes"),
		opt(2, "Yes, and switch to auto mode · auto mode handles these prompts for you"),
		opt(3, "No"),
		" Esc to cancel · Tab to amend",
	)
}

func TestAPermissionPromptIsFoundWithItsChoices(t *testing.T) {
	p, ok := ExtractPrompt(bashDialog(1, "ls -la /tmp"))
	if !ok {
		t.Fatal("the prompt was not found")
	}
	if len(p.Options) != 3 || p.Options[0] != (Option{"1", "Yes"}) || p.Options[2] != (Option{"3", "No"}) ||
		!strings.HasPrefix(p.Options[1].Label, "Yes, and switch to auto mode") {
		t.Fatalf("options: %+v", p.Options)
	}
	for _, want := range []string{"Bash command", "│ ls -la /tmp", "Do you want to proceed?", "❯ 1. Yes", "Esc to cancel"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("the text lacks %q:\n%s", want, p.Text)
		}
	}
	for _, not := range []string{"╌", "earlier list", "that is not a prompt", "Let me look"} {
		if strings.Contains(p.Text, not) {
			t.Errorf("the text has %q, which is not part of the dialog:\n%s", not, p.Text)
		}
	}
	if len(p.Hash) != 16 || strings.Trim(p.Hash, "0123456789abcdef") != "" {
		t.Errorf("hash %q", p.Hash)
	}
	if p.Shortened || p.Redacted {
		t.Errorf("a short dialog without secrets is marked %+v", p)
	}
}

// The hash tells one prompt from another. It must not move with the highlight (someone may press Down on the laptop),
// nor with the tip line, but it must change with the command: a button of an old notice may not answer a new prompt.
func TestTheHashFollowsTheCommandAndNothingElse(t *testing.T) {
	a, _ := ExtractPrompt(bashDialog(1, "ls -la /tmp"))
	b, _ := ExtractPrompt(bashDialog(2, "ls -la /tmp"))
	if a.Hash != b.Hash {
		t.Errorf("moving the highlight changed the hash: %s %s", a.Hash, b.Hash)
	}
	tip := strings.Replace(bashDialog(1, "ls -la /tmp"), "Tip: auto mode handles", "Tip: something else handles", 1)
	c, _ := ExtractPrompt(tip)
	if a.Hash != c.Hash {
		t.Errorf("another tip changed the hash: %s %s", a.Hash, c.Hash)
	}
	d, _ := ExtractPrompt(bashDialog(1, "rm -rf /tmp/x"))
	if a.Hash == d.Hash {
		t.Error("a prompt for another command has the same hash")
	}
	// the same prompt seen twice
	again, _ := ExtractPrompt(bashDialog(1, "ls -la /tmp"))
	if a.Hash != again.Hash {
		t.Error("the hash is not stable")
	}
}

func TestAQuestionWithChoicesIsAPrompt(t *testing.T) {
	p, ok := ExtractPrompt(screenOf("⏺ I need to know something.", rule, " Which colour do you want?", " ❯ 1. Red", "   2. Blue", "   3. Type something else", " Enter to select · Esc to cancel"))
	if !ok || len(p.Options) != 3 || p.Options[1].Label != "Blue" || !strings.Contains(p.Text, "Which colour") {
		t.Fatalf("%v %+v", ok, p)
	}
}

func TestAScreenWithoutAPromptIsNotOne(t *testing.T) {
	for name, s := range map[string]string{
		"an idle input box":     screenOf("⏺ All done.", rule, " ❯ ", rule, "  ⏵⏵ auto mode on (shift+tab to cycle)"),
		"a numbered list above": screenOf("⏺ Steps:", "  1. first", "  2. second", "  3. third", rule, " ❯ ", rule, "  ⏸ manual mode on"),
		"an empty screen":       "",
		"only one choice":       screenOf(rule, " Continue?", " ❯ 1. Yes"),
		"choices that skip":     screenOf(rule, " Pick", " 1. a", " 3. b"),
	} {
		if p, ok := ExtractPrompt(s); ok {
			t.Errorf("%s: taken for a prompt: %+v", name, p)
		}
	}
}

func TestWithoutARuleTheBottomOfTheScreenIsLookedAt(t *testing.T) {
	p, ok := ExtractPrompt(screenOf("some output", " Which one?", " ❯ 1. one", "   2. two"))
	if !ok || len(p.Options) != 2 {
		t.Fatalf("%v %+v", ok, p)
	}
}

func TestChoicesAreNumberedInARowAndProseAfterThemIsIgnored(t *testing.T) {
	p, ok := ExtractPrompt(screenOf(rule, " Do it?", " ❯ 1. Yes", "   2. No", " Note:", " 1. a stray numbered line"))
	if !ok || len(p.Options) != 2 {
		t.Fatalf("%v %+v", ok, p.Options)
	}
}

func TestSecretsInAPromptAreHiddenAndALongPromptIsCut(t *testing.T) {
	for cmd, leaks := range map[string][]string{
		`curl -H "Authorization: Bearer abc123def" https://example.org`: {"abc123def"},
		`run --token s3cr3t --flag`:                                     {"s3cr3t"},
		`run password="two words here" next`:                            {"two words", "words here"},
		`run --token 'a b c' next`:                                      {"a b c"},
		`psql postgres://user:hunter2@db/x`:                             {"hunter2"},
		`export API_KEY=k-12345`:                                        {"k-12345"},
	} {
		p, ok := ExtractPrompt(bashDialog(1, cmd))
		if !ok || !strings.Contains(p.Text, "<hidden>") {
			t.Errorf("%q: %v %q", cmd, ok, p.Text)
			continue
		}
		for _, leak := range leaks {
			if strings.Contains(p.Text, leak) {
				t.Errorf("%q: %q is shown", cmd, leak)
			}
		}
	}
	p, _ := ExtractPrompt(bashDialog(1, strings.Repeat("x", 3000)))
	if n := len([]rune(p.Text)); n > MaxPromptChars || !p.Shortened || !strings.Contains(p.Text, "characters left out]") {
		t.Errorf("not cut: %d characters, shortened %v", n, p.Shortened)
	}
	// the hash is over the whole dialog, not the cut text: commands that differ only after the cut still differ
	a, _ := ExtractPrompt(bashDialog(1, strings.Repeat("x", 3000)+"a"))
	b, _ := ExtractPrompt(bashDialog(1, strings.Repeat("x", 3000)+"b"))
	if a.Hash == b.Hash {
		t.Error("two commands that differ after the cut have one hash")
	}
}

func TestThePermissionModeIsReadFromTheStatusLine(t *testing.T) {
	for line, want := range map[string]string{
		"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents": ModeAuto,
		"  ⏸ manual mode on · ? for shortcuts":                  ModeManual,
		"  ⏵⏵ accept edits on (shift+tab to cycle)":             ModeAcceptEdits,
		"  ⏸ plan mode on (shift+tab to cycle)":                 ModePlan,
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)":       ModeBypass,
		"  ⏵⏵ don't ask on (shift+tab to cycle)":                ModeDontAsk,
		"  something unrelated":                                 "",
	} {
		s := screenOf("⏺ text that says manual mode on is not the status", rule, " ❯ ", rule, line)
		if got := Mode(s); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
	if Mode("") != "" {
		t.Error("an empty screen has a mode")
	}
}

func TestBusyMeansTheStatusLineOffersToInterrupt(t *testing.T) {
	if !Busy(screenOf("✻ Thinking… (12s · esc to interrupt)", rule, " ❯ ", rule, "  ⏵⏵ auto mode on")) {
		t.Error("a working screen is not busy")
	}
	if Busy(screenOf("⏺ Done.", rule, " ❯ ", rule, "  ⏵⏵ auto mode on")) {
		t.Error("an idle screen is busy")
	}
}

func TestBottomLinesAreTheLastNonEmptyOnesWithSecretsHidden(t *testing.T) {
	s := screenOf("one", "", "two", "token=abc123", "", "four")
	if got := BottomLines(s, 3); got != "two\ntoken=<hidden>\nfour" {
		t.Errorf("%q", got)
	}
	if got := BottomLines(s, 50); !strings.HasPrefix(got, "one\n") {
		t.Errorf("%q", got)
	}
}
