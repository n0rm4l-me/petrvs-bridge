package agentsvc

import (
	"context"

	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
)

// typeBlock says why a line must not be typed into the session right now.
type typeBlock int

const (
	typeFree   typeBlock = iota // nothing is known against it
	typeShell                   // a shell, not Claude Code, is in the foreground
	typePrompt                  // Claude Code shows a dialog with numbered choices
)

// typingBlock looks at the pane before a line of text is typed into it. Two things go wrong otherwise:
//   - Claude Code has exited and a shell prompt is in the foreground: the shell would run the words as a command;
//   - a dialog with numbered choices is on the screen: the text is typed into it and the Enter that follows picks the
//     highlighted choice, usually "1. Yes".
//
// The check needs the screen interface: without it, or when tmux cannot be asked, nothing is known and the text goes on
// (the typing then fails by itself if the session is gone). A pane that shows a shell as its program but looks like Claude
// Code's own screen (a wrapper script) is given the text, as auto-heal would not type into it either.
func (a *Agent) typingBlock(ctx context.Context) typeBlock {
	if a.cfg.Screen == nil {
		return typeFree
	}
	st := a.readPaneState(ctx)
	if st.kind == paneNoSession {
		return typeFree
	}
	cctx, cancel := context.WithTimeout(ctx, paneTimeout)
	defer cancel()
	scr, err := a.cfg.Screen.Capture(cctx)
	if st.kind == paneShell && (err != nil || !looksLikeClaudeCode(scr)) {
		return typeShell
	}
	if err == nil {
		if _, ok := screen.ExtractPrompt(scr); ok {
			return typePrompt
		}
	}
	return typeFree
}

// checkTextTarget stops a text from the chat from being typed into a shell or into a dialog.
func (a *Agent) checkTextTarget(ctx context.Context) error {
	switch a.typingBlock(ctx) {
	case typeShell:
		return &userError{"refused: a shell, not Claude Code, is in the foreground of the session, so the text was not typed (it would have been run as a command). /continue starts Claude Code."}
	case typePrompt:
		return &userError{"a prompt is waiting for an answer, so the text was not typed: answer it with the buttons or /key N (/screen shows it), then send the text again"}
	}
	return nil
}

// checkFileLine is checkTextTarget for the line that tells the session about a file from the chat. The file is saved by
// then and stays saved; the message says that the session was not told.
func (a *Agent) checkFileLine(ctx context.Context) error {
	switch a.typingBlock(ctx) {
	case typeShell:
		return &userError{"the file was saved in the inbox, but a shell, not Claude Code, is in the foreground of the session, so the session was not told about it (the line would have been run as a command). /continue starts Claude Code."}
	case typePrompt:
		return &userError{"the file was saved in the inbox, but a prompt is waiting for an answer, so the session was not told about it: answer the prompt with the buttons or /key N (/screen shows it), then tell Claude Code about the file"}
	}
	return nil
}
