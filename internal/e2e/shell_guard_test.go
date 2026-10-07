package e2e

import (
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// Words meant for Claude Code must not be run by a shell: when Claude Code has exited and a shell prompt is in the
// foreground, the text is not typed, and the chat is told why. Once Claude Code runs again the text goes through.
func TestTextIsNotTypedIntoAShellThatReplacedClaudeCode(t *testing.T) {
	r, a := one(t, testrig.Options{Screen: true})
	r.WaitSent("laptop is connected")
	a.Screen.SetCommand("zsh")
	a.Screen.Set("petr@laptop ~ % \n")
	r.Say("delete the old logs")
	r.WaitSent("a shell, not Claude Code")
	r.Never("text typed into the shell", 500*time.Millisecond, func() bool { return len(a.Exec.Texts()) != 0 })
	// Claude Code is back
	a.Screen.SetCommand("claude")
	a.Screen.Set(testrig.IdleScreen)
	r.Say("delete the old logs")
	r.Eventually("typed once Claude Code runs", func() bool { return len(a.Exec.Texts()) == 1 })
}

// A pane whose program name is a shell but whose screen is Claude Code's (a wrapper script runs it) is not refused.
func TestTextGoesToAShellNamedPaneThatShowsClaudeCode(t *testing.T) {
	r, a := one(t, testrig.Options{Screen: true})
	r.WaitSent("laptop is connected")
	a.Screen.SetCommand("sh")
	r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
}

// Without a screen interface nothing is known about the pane and the text goes on, as before.
func TestTextGoesOnWhenThePaneCannotBeLookedAt(t *testing.T) {
	r, a := one(t, testrig.Options{})
	r.WaitSent("laptop is connected")
	r.Say("hello")
	r.Eventually("typed", func() bool { return len(a.Exec.Texts()) == 1 })
}
