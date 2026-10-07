package agentsvc

import (
	"context"
	"sync"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// What watches the machine: the facts of the status card (facts.go), auto-heal of Claude Code (heal.go), the
// stuck-request signal (stuck.go). This file holds the seams and the state they share.

type machState struct {
	// mu guards the clock of auto-heal and the request that waits for a reply. It is never held across a call that can
	// block (tmux, the spool), and nothing that holds it takes a.mu.
	mu     sync.Mutex
	heal   healClock
	req    *stuckReq
	reqSeq uint64

	// wg counts the background loops. Run waits for them before it returns (and so do the tests), so that a loop does not
	// write into a directory that is being removed.
	wg sync.WaitGroup

	// startMu lets one start of Claude Code happen at a time: auto-heal and /continue must not both type the command into
	// the same pane.
	startMu sync.Mutex

	factsMu   sync.Mutex
	facts     map[string]string // what Ext.Facts last returned
	factsBusy bool              // a call of Ext.Facts has not come back yet
	modeAt    time.Time         // when the screen was last read for the permission mode (real time)
	mode      string
	modeKind  paneStateKind // what ran in the pane then

	// How often the background work looks; set from the settings in init so that tests with short times run quickly.
	healEvery, stuckEvery, modeEvery time.Duration
}

func (m *machState) init(a *Agent) {
	e := a.cfg.Ext
	m.healEvery = pollEvery(healPoll, e.Heal.Grace)
	m.stuckEvery = pollEvery(stuckPoll, e.StuckAfter)
	m.modeEvery = modePoll
}

func (m *machState) background(f func()) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		f()
	}()
}

// startExtras starts the background work (auto-heal, the stuck-request check).
func (a *Agent) startExtras(ctx context.Context) {
	e := a.cfg.Ext
	if e.Heal.Enabled {
		switch {
		case a.cfg.Screen == nil:
			a.log.Warn("auto-heal is on but the agent cannot see its tmux session, so it stays off")
		case e.Workdir == "":
			a.log.Warn("auto-heal is on but no working directory is set, so it stays off")
		default:
			a.mach.background(func() { a.loop(ctx, a.mach.healEvery, false, a.healTick) })
		}
	}
	if e.StuckAfter > 0 {
		a.mach.background(func() { a.loop(ctx, a.mach.stuckEvery, false, a.stuckTick) })
	}
}

// noteSpooled is told of each spooled message that has just been sent to the hub.
func (a *Agent) noteSpooled(it spool.Item) {
	if a.cfg.Ext.StuckAfter <= 0 {
		return
	}
	switch it.Type {
	case wire.TypeReply:
		a.stuckAnswered()
	case wire.TypeNotice:
		if waitsForUser(it) {
			a.stuckWaiting(it)
		}
	}
}

// noteCommand is told of each command that was carried out. The clock of a text does not start here but before it is typed
// (see execute): a reply can be spooled before this call, and a clock started after it would never be stopped.
func (a *Agent) noteCommand(p *wire.Payload) {
	if a.cfg.Ext.StuckAfter <= 0 {
		return
	}
	if p.Type == wire.TypeKey {
		a.stuckKey(p.Body)
	}
}

// executeStart is the command to start Claude Code in the session if it is not running there.
func (a *Agent) executeStart(ctx context.Context, p *wire.Payload) error {
	return a.continueClaude(ctx)
}

// queueNotice tells the hub something through the spool, which keeps it until the hub has confirmed it; without a
// spool, or with a full one, it is sent once and may be lost.
func (a *Agent) queueNotice(ctx context.Context, body string, buttons []wire.Button) {
	if ctx.Err() != nil { // the agent is stopping
		return
	}
	if a.cfg.Spool != nil {
		err := a.cfg.Spool.AddItem(spool.Item{Type: wire.TypeNotice, Body: body, Buttons: buttons})
		if err == nil {
			a.wakeSpool()
			return
		}
		a.log.Warn("could not queue a notice", "err", err)
	}
	a.send(ctx, &wire.Payload{Type: wire.TypeNotice, Body: body, Buttons: buttons})
}
