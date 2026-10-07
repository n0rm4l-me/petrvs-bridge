package agentsvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// callbackExec types nothing. When a text is typed it calls hook, which stands for the assistant answering at once and the
// Stop hook spooling the reply before the typing call has returned, and it returns err.
type callbackExec struct {
	recExec
	hook func()
	err  error
}

func (c *callbackExec) SendText(ctx context.Context, s string) error {
	c.recExec.SendText(ctx, s)
	if c.hook != nil {
		c.hook()
	}
	return c.err
}

// The clock used to start after "done" was sent, so a reply that was spooled before that left a clock nothing would stop,
// and three alerts for a request that had been answered.
func TestAReplySpooledBeforeTheTypingReturnsLeavesNoClock(t *testing.T) {
	r := machStuckRig(t)
	r.a.cfg.Exec = &callbackExec{hook: r.reply}
	r.a.execute(context.Background(), cmd(1, "please fix the build"))
	if r.a.mach.req != nil {
		t.Fatal("the request is still pending although its reply was spooled before the command finished")
	}
	r.stuck(24 * time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("an alert for a request that was answered: %q", n[0].Body)
	}
}

func TestATextThatIsRunThroughExecuteStartsTheClock(t *testing.T) {
	r := machStuckRig(t)
	r.a.execute(context.Background(), cmd(1, "please fix the build"))
	if r.a.mach.req == nil || r.a.mach.req.preview != "please fix the build" {
		t.Fatalf("%+v", r.a.mach.req)
	}
	r.stuck(machStuckAfter)
	if n := r.stuckNotices(t); len(n) != 1 {
		t.Fatalf("%d alerts", len(n))
	}
}

// A text that was not typed (refused by the guard, or tmux failed) will not be answered: it must not start a clock, and the
// request that was pending before it is still pending.
func TestATextThatWasNotTypedStartsNoClockAndLeavesThePendingOne(t *testing.T) {
	ctx := context.Background()
	t.Run("refused by the guard", func(t *testing.T) {
		r := machStuckRig(t)
		r.pane.shell("zsh")
		r.a.execute(ctx, cmd(1, "delete the logs"))
		if r.a.mach.req != nil || r.ex.got() != "" {
			t.Fatalf("request %+v, typed %q", r.a.mach.req, r.ex.got())
		}
	})
	t.Run("tmux fails", func(t *testing.T) {
		r := machStuckRig(t)
		r.a.cfg.Exec = &callbackExec{err: errors.New("boom")}
		r.a.execute(ctx, cmd(1, "delete the logs"))
		if r.a.mach.req != nil {
			t.Fatal("a request for a text that was not typed")
		}
		r.stuck(24 * time.Hour)
		if n := r.stuckNotices(t); len(n) != 0 {
			t.Fatalf("%q", n[0].Body)
		}
	})
	t.Run("an older request stays", func(t *testing.T) {
		r := machStuckRig(t)
		r.ask("the older request")
		r.a.cfg.Exec = &callbackExec{err: errors.New("boom")}
		r.a.execute(ctx, cmd(1, "the new one"))
		if r.a.mach.req == nil || r.a.mach.req.preview != "the older request" {
			t.Fatalf("%+v", r.a.mach.req)
		}
	})
}

func TestAReplyThatIsQueuedButNotYetSentHoldsBackTheAlert(t *testing.T) {
	r := machStuckRig(t)
	r.ask("x")
	if err := r.sp.AddItem(spool.Item{Type: wire.TypeReply, Body: "the answer, waiting to be sent"}); err != nil {
		t.Fatal(err)
	}
	r.stuck(machStuckAfter)
	r.stuck(time.Hour)
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("an alert queued behind the reply: %q", n[0].Body)
	}
	// Something else in the spool is no reply.
	es, _ := r.sp.List()
	for _, e := range es {
		r.sp.Remove(e.Name)
	}
	r.sp.AddItem(spool.Item{Type: wire.TypeNotice, Body: "something else"})
	r.stuck(time.Second)
	if n := r.stuckNotices(t); len(n) != 1 {
		t.Fatalf("%d alerts once no reply was queued", len(n))
	}
}

// An interrupt gives no reply, and neither does a prompt that is turned down with Esc; "✖ Esc" is a button of every prompt.
func TestEscapeWhileAPromptWaitsDropsTheRequestToo(t *testing.T) {
	r := machStuckRig(t)
	r.ask("deploy")
	r.a.noteSpooled(machPromptNotice())
	r.a.noteCommand(&wire.Payload{Type: wire.TypeKey, Body: "Escape"})
	if r.a.mach.req != nil {
		t.Fatal("the request lives on after Esc")
	}
	for i := 0; i < 5; i++ {
		r.stuck(machStuckAfter)
	}
	if n := r.stuckNotices(t); len(n) != 0 {
		t.Fatalf("%d false alerts after a tap on Esc", len(n))
	}
}
