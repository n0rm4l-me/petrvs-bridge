package agentsvc

import (
	"context"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// This file holds the seams where the parts that go beyond typing text and keys attach to the agent: files from the
// chat, buttons and the screen, the status facts, auto-heal and the stuck-request signal. Each part replaces the bodies
// below in a file of its own; the rest of the agent only calls these.

// Screen reads and starts the session that the agent types into. It is optional: without it the agent cannot look at
// the prompt a key answers, send the screen, tell the permission mode, notice that the assistant has stopped or start it.
type Screen interface {
	// Capture returns the text on the screen of the session's pane. It returns screen.ErrNoSession if there is none.
	Capture(ctx context.Context) (string, error)
	// PaneCommand returns the program in the foreground of the pane ("claude", "opencode", "zsh", ...), or screen.ErrNoSession.
	PaneCommand(ctx context.Context) (string, error)
	// Start creates the session running command in workdir. The command comes from the local settings.
	Start(ctx context.Context, workdir, command string) error
}

// Heal says whether and how the agent starts the assistant again when it has stopped.
type Heal struct {
	Enabled bool
	// Command is run in the session, in Workdir, to start the assistant, for example "claude --continue" or "opencode".
	Command string
	// Grace is how long the pane must show a shell (or the session be missing) before the agent acts, so that a restart
	// by hand is not raced.
	Grace time.Duration
	// MaxTries starts are allowed within Window; after that the agent gives up and says so until the assistant runs again.
	MaxTries int
	Window   time.Duration
}

// Extras holds the settings of the parts beyond text and keys. The zero value turns them off or uses defaults.
type Extras struct {
	// AssistantName is what the chat calls the assistant in status messages (default "Claude Code").
	AssistantName string
	// Workdir is the directory the assistant runs in; auto-heal starts it there.
	Workdir string
	// InboxDir is where files from the chat are saved. Empty turns receiving files off.
	InboxDir string
	// StateDir is the agent's own state directory, which `agent send-file` refuses to send from.
	StateDir string
	// MaxFileBytes bounds a file in either direction (default filetx.DefaultMaxBytes).
	MaxFileBytes int64
	Heal         Heal
	// StuckAfter is how long a request may go without a reply before the agent says so; 0 turns the check off.
	StuckAfter time.Duration
	// Facts adds the machine facts of the status card (battery, uptime, disk) to each heartbeat.
	Facts func() map[string]string
}

// userError is a refusal whose text is for the user: execute sends it to the hub as it is.
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

// initExt prepares the state of the parts; it runs at the end of New.
func (a *Agent) initExt() {
	a.down.init(a)
	a.ui.init(a)
	a.mach.init(a)
}

// afterCommand is called after a command has been carried out.
func (a *Agent) afterCommand(ctx context.Context, p *wire.Payload) {
	switch p.Type {
	case wire.TypeKey:
		a.afterKey(ctx, p)
	case wire.TypeText:
		a.afterText(ctx, p)
	}
	a.noteCommand(p)
}
