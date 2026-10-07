package screen

import (
	"fmt"
	"strings"
	"testing"
)

// permissionDialog draws the permission dialog of bashDialog around the given lines of the frame, which the caller
// writes as they are to appear (with their bars, or without). The choices and the hint are below.
func permissionDialog(cursor int, frame ...string) string {
	ls := []string{"⏺ Let me do that.", rule, " Bash command", " Does the thing", dashed}
	ls = append(ls, frame...)
	ls = append(ls, dashed, " Do you want to proceed?")
	for i, label := range []string{"Yes", "Yes, and switch to auto mode", "No"} {
		mark := "  "
		if i+1 == cursor {
			mark = "❯ "
		}
		ls = append(ls, fmt.Sprintf(" %s%d. %s", mark, i+1, label))
	}
	return screenOf(append(ls, " Esc to cancel · Tab to amend")...)
}

func labels(p Prompt) string {
	var out []string
	for _, o := range p.Options {
		out = append(out, o.Key+"="+o.Label)
	}
	return strings.Join(out, "|")
}

const realChoices = "1=Yes|2=Yes, and switch to auto mode|3=No"

// A numbered list inside the command is not the choices: the choices are the last numbered run before the hint line. The
// buttons must say what the keys do, or a button that reads "Deny this" would press Yes.
func TestANumberedListInTheCommandDoesNotShadowTheChoices(t *testing.T) {
	for name, frame := range map[string][]string{
		"a heredoc with its lines at the left": {" │ cat > notes.txt <<'EOF'", "1. Deny this", "2. Cancel", "3. Ignore", " │ EOF"},
		"a heredoc indented":                   {" │ cat <<'EOF'", "  1. Deny this", "  2. Cancel", " │ EOF"},
		"a list that starts the command":       {"   1. Deny this", "   2. Cancel", " │ rm -rf /tmp/x"},
		"a list that has one entry":            {"   1. Deny this", " │ rm -rf /tmp/x"},
		"a list behind the bar":                {" │ 1. Deny this", " │ 2. Cancel"},
	} {
		p, ok := ExtractPrompt(permissionDialog(1, frame...))
		if !ok || labels(p) != realChoices {
			t.Errorf("%s: %v %s", name, ok, labels(p))
		}
	}
}

func TestANumberedPlanInTheDialogDoesNotShadowTheChoices(t *testing.T) {
	p, ok := ExtractPrompt(screenOf(
		"⏺ I have a plan.", rule,
		" Ready to code?",
		" Here is Claude's plan:",
		"  1. Create the module",
		"  2. Add the tests",
		"  3. Deploy it",
		"",
		" Would you like to proceed?",
		" ❯ 1. Yes, and auto-accept edits",
		"   2. Yes, and manually approve edits",
		"   3. No, keep planning",
		" ctrl-g to edit the plan"))
	if !ok || labels(p) != "1=Yes, and auto-accept edits|2=Yes, and manually approve edits|3=No, keep planning" {
		t.Fatalf("%v %s", ok, labels(p))
	}
}

func TestOnlyTheFirstEightChoicesBecomeOptions(t *testing.T) {
	p, ok := ExtractPrompt(screenOf(rule, " Which?", " 1. a", " 2. b", " 3. c", " 4. d", " 5. e", " 6. f", " 7. g", " 8. h", " 9. i", " 10. j"))
	if !ok || len(p.Options) != MaxOptions || p.Options[7].Key != "8" {
		t.Fatalf("%v %s", ok, labels(p))
	}
}

// What is left out of a long dialog is the middle. The question and the choices are at the end, and the user answers on
// the strength of them.
func TestALongDialogLosesItsMiddleAndKeepsTheQuestionAndTheChoices(t *testing.T) {
	var frame []string
	for i := 0; i < 120; i++ {
		frame = append(frame, fmt.Sprintf(" │ echo line-%03d padding padding padding", i))
	}
	frame = append(frame, " │ rm -rf ~/the-dangerous-tail")
	p, ok := ExtractPrompt(permissionDialog(1, frame...))
	if !ok || !p.Shortened {
		t.Fatalf("%v %+v", ok, p)
	}
	if n := len([]rune(p.Text)); n > MaxPromptChars {
		t.Errorf("%d characters", n)
	}
	for _, want := range []string{"Bash command", "echo line-000", "characters left out]", "rm -rf ~/the-dangerous-tail", "Do you want to proceed?", "❯ 1. Yes", "3. No", "Esc to cancel"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("the text lacks %q:\n%s", want, p.Text)
		}
	}
	if strings.Contains(p.Text, "echo line-060") {
		t.Errorf("the middle is still there:\n%s", p.Text)
	}
	if labels(p) != realChoices {
		t.Errorf("%s", labels(p))
	}
	// the number says how much is missing
	var left int
	i := strings.Index(p.Text, "[")
	if _, err := fmt.Sscanf(p.Text[i:], "[%d characters left out]", &left); err != nil || left < 1000 {
		t.Errorf("%q: %v", p.Text[i:], err)
	}
}

func TestTheMiddleIsCutOnlyWhenTheDialogIsTooLong(t *testing.T) {
	p, _ := ExtractPrompt(permissionDialog(1, " │ "+strings.Repeat("y", 1000)))
	if p.Shortened || strings.Contains(p.Text, "left out") || !strings.Contains(p.Text, strings.Repeat("y", 1000)) {
		t.Errorf("a dialog of %d characters was cut", len([]rune(p.Text)))
	}
	// one line longer than the limit: the choices still show
	p, _ = ExtractPrompt(permissionDialog(1, " │ "+strings.Repeat("z", 5000)))
	if !p.Shortened || len([]rune(p.Text)) > MaxPromptChars || !strings.HasSuffix(p.Text, "Esc to cancel · Tab to amend") || !strings.Contains(p.Text, "❯ 1. Yes") {
		t.Errorf("shortened %v, %d characters:\n%s", p.Shortened, len([]rune(p.Text)), p.Text)
	}
}

func TestRedactedSaysThatSomethingWasHidden(t *testing.T) {
	p, _ := ExtractPrompt(permissionDialog(1, " │ export TOKEN=abc123"))
	if !p.Redacted || !strings.Contains(p.Text, "TOKEN=<hidden>") {
		t.Errorf("%+v", p)
	}
	p, _ = ExtractPrompt(permissionDialog(1, " │ ls -la"))
	if p.Redacted {
		t.Errorf("nothing was hidden: %+v", p)
	}
}
