package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/promptcmd"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
)

// promptTimeout bounds the whole command: a hook must not hold Claude Code up.
const promptTimeout = 10 * time.Second

// prompt is what the Notification hook runs. It reads the hook's JSON from standard input and queues a notice that shows
// the prompt on the screen of the pane in $TMUX_PANE, with a button per choice. Like `send` it needs only the state
// directory (and tmux on PATH).
func prompt(args []string, getenv func(string) string, stdin io.Reader) int {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	dir := getenv("PB_STATE_DIR")
	if dir == "" {
		cfg, code := parse(nil, localEnv(getenv))
		if cfg == nil {
			return code
		}
		dir = cfg.StateDir
	}
	// The buttons are bound to the prompt by a hash keyed with the install's key, and the agent checks it with the same key.
	// A hook that cannot read the key would make buttons that the agent refuses, so it looks at no pane and sends the plain
	// notice, which is still the news that Claude Code is waiting.
	pane := getenv("TMUX_PANE")
	if key, err := screen.LoadKey(dir); err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot read the prompt key, so the notice has no buttons:", err)
		pane = ""
	} else {
		screen.SetHashKey(key)
	}
	sp, err := spool.Open(dir + "/spool")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), promptTimeout)
	defer cancel()
	if err := promptcmd.Run(ctx, promptcmd.Options{Stdin: stdin, Pane: pane, Queue: sp}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
