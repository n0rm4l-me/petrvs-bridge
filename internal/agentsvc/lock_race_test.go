package agentsvc

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
)

// After `agent unlock` removed the lock file, the agent still believes in the old lock until something asks. A lockdown
// that arrives then, while something does ask (the heartbeat, a command about to run) and the file cannot be written,
// must still leave the agent locked: the lock in memory is the one that counts when the disk refuses.
func TestALockdownIsNotUndoneByAConcurrentCheckWhenTheFileCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	id, _ := identity.Generate("laptop")
	hub, _ := identity.Generate("hub")
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	lockFile := filepath.Join(dir, "locked")
	a, err := New(Config{Identity: id, Hub: hub.Public(), Transport: transport.NewMem(), Exec: noExec{}, LockFile: lockFile,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 300; run++ {
		// The state after `agent unlock`: locked on disk as far as the agent knows, but the file is gone.
		a.mu.Lock()
		a.locked, a.lockOnDisk = true, true
		a.mu.Unlock()

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					a.Locked()
				}
			}
		}()
		a.lock(context.Background())
		close(stop)
		wg.Wait()

		if !a.Locked() {
			t.Fatalf("run %d: the agent is unlocked after a lockdown", run)
		}
	}
}
