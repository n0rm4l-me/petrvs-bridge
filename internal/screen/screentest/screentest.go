// Package screentest draws screens of Claude Code for tests: the permission dialog, a question with choices, an idle input
// box, and an answer that holds a numbered list. It is used by tests only.
package screentest

import (
	"fmt"
	"strings"
)

// Rule is the full-width line above a dialog and around the input box; Dashed is the line inside a dialog.
var (
	Rule   = strings.Repeat("─", 80)
	Dashed = strings.Repeat("╌", 80)
)

// Lines joins lines into a screen.
func Lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func choice(cursor, n int, text string) string {
	mark := "  "
	if n == cursor {
		mark = "❯ "
	}
	return fmt.Sprintf(" %s%d. %s", mark, n, text)
}

// Bash is a permission prompt as Claude Code draws it, under some conversation. cursor is the highlighted choice (1..3);
// moving it is what someone pressing Down on the laptop does.
func Bash(cursor int, command string) string {
	return Lines(
		"⏺ Let me look at the directory.",
		"  earlier output with private-text in it",
		Rule,
		" Bash command",
		" List the files",
		Dashed,
		" │ "+command,
		Dashed,
		" Do you want to proceed?",
		choice(cursor, 1, "Yes"),
		choice(cursor, 2, "Yes, and switch to auto mode"),
		choice(cursor, 3, "No"),
		" Esc to cancel · Tab to amend",
	)
}

// Question is a question with choices, as asked by the assistant.
func Question(cursor int) string {
	return Lines(
		"⏺ I need to know something.",
		Rule,
		" Which colour do you want?",
		choice(cursor, 1, "Red"),
		choice(cursor, 2, "Blue"),
		choice(cursor, 3, "Type something else"),
		" Enter to select · Esc to cancel",
	)
}

// Many is a dialog with n choices whose labels label gives.
func Many(n int, label func(i int) string) string {
	ls := []string{"⏺ Pick one.", Rule, " Which one?"}
	for i := 1; i <= n; i++ {
		ls = append(ls, choice(1, i, label(i)))
	}
	return Lines(append(ls, " Enter to select · Esc to cancel")...)
}

// Idle is Claude Code waiting for the next request, with modeLine under the input box.
func Idle(modeLine string) string {
	return Lines("⏺ All done.", "", Rule, " ❯ ", Rule, "  "+modeLine)
}

// ListInAnswer is an answer that holds a numbered list, with the input box below it: not a prompt.
func ListInAnswer() string {
	return Lines(
		"⏺ Steps to take:",
		"  1. first",
		"  2. second",
		"  3. third",
		"",
		Rule, " ❯ ", Rule,
		"  ⏸ manual mode on (shift+tab to cycle)",
	)
}

// Resume is the list Claude Code shows for /resume: a cursor on a list of sessions, with no numbered choices.
func Resume() string {
	return Lines(
		Rule,
		" Resume Session",
		"",
		" ❯ fix the build (2 hours ago)",
		"   write the docs (yesterday)",
		"   try the new parser (3 days ago)",
		"",
		" ↑↓ to navigate · Enter to select · Esc to cancel",
	)
}
