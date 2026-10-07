package e2e

import (
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen/screentest"
	"github.com/n0rm4l-me/petrvs-bridge/internal/testrig"
)

// Enter after a typed text picks the highlighted choice of a dialog ("1. Yes"). While a dialog waits, no text from the chat
// is typed, /sessions from the menu included; the chat is told how to answer, and once the dialog is gone the same text
// goes through.
func TestNoTextIsTypedWhileAPromptWaitsForAnAnswer(t *testing.T) {
	r, a := one(t, testrig.Options{Screen: true})
	r.WaitSent("laptop is connected")
	a.Screen.Set(screentest.Bash(1, "rm -rf build"))
	r.Say("yes, go on")
	r.WaitSent("a prompt is waiting for an answer, so the text was not typed")
	r.Say("/sessions")
	r.Never("text typed into a dialog", 500*time.Millisecond, func() bool { return len(a.Exec.Texts()) != 0 || len(a.Exec.Keys()) != 0 })
	a.Screen.Set(testrig.IdleScreen)
	r.Say("yes, go on")
	r.Eventually("typed once the dialog is gone", func() bool { return len(a.Exec.Texts()) == 1 })
}
