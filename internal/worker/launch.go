package worker

import (
	"context"
	"io"
	"os/exec"
	"sync"

	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

// ExecLauncher starts the assistant as a child process of this program.
func ExecLauncher(command string) Launcher {
	return func(ctx context.Context, args []string, dir string) (Process, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		tail := &tailBuffer{max: 2048}
		cmd.Stderr = tail
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, tail: tail}, nil
	}
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	tail   *tailBuffer
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.Reader     { return p.stdout }
func (p *execProcess) Wait() error           { return p.cmd.Wait() }
func (p *execProcess) Stderr() string        { return p.tail.String() }
func (p *execProcess) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// SpoolOutbox puts what the chat should get into the agent's spool, which keeps it until the hub has confirmed it.
type SpoolOutbox struct {
	Spool *spool.Spool
	// OnError, if set, is told when an item could not be queued.
	OnError func(error)
}

// Reply queues the answer of a turn.
func (o SpoolOutbox) Reply(body string) { o.add(spool.Item{Type: wire.TypeReply, Body: body}) }

// Notice queues a notice with its buttons.
func (o SpoolOutbox) Notice(body string, buttons []wire.Button) {
	o.add(spool.Item{Type: wire.TypeNotice, Body: body, Buttons: buttons})
}

func (o SpoolOutbox) add(it spool.Item) {
	if err := o.Spool.AddItem(it); err != nil && o.OnError != nil {
		o.OnError(err)
	}
}
