// Package health lets a program that listens on nothing tell its container runtime whether it is alive and ready. The program
// touches two files in a directory (a Beacon); the runtime runs a second command (`hub probe live` or `hub probe ready`) that
// calls Check and reads their age. No port is opened, and what the files say is a few bytes of nothing: only their dates matter.
package health

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Kinds of check.
const (
	Live  = "live"  // the main loops still make progress; if not, the process should be restarted
	Ready = "ready" // the program is doing its work now (it is not starting, draining or cut off from what it serves)
)

// Beacon writes the two files.
type Beacon struct{ dir string }

// NewBeacon makes the directory (private to the user) if it is not there. The path must be absolute.
func NewBeacon(dir string) (*Beacon, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("health directory %q: the path must be absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Beacon{dir: dir}, nil
}

// Alive records that the program is alive now.
func (b *Beacon) Alive() error { return touch(filepath.Join(b.dir, Live)) }

// Ready records that the program is ready now (true), or that it is not (false: the file is removed at once, so that the
// next probe fails without waiting for the file to age).
func (b *Beacon) Ready(ok bool) error {
	p := filepath.Join(b.dir, Ready)
	if ok {
		return touch(p)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// Check reports whether the file of that kind in dir was touched less than maxAge before now. It is what a probe runs.
func Check(dir, kind string, maxAge time.Duration, now time.Time) error {
	if kind != Live && kind != Ready {
		return fmt.Errorf("unknown check %q: use %s or %s", kind, Live, Ready)
	}
	fi, err := os.Stat(filepath.Join(dir, kind))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("not %s: there is no %s file in %s", kind, kind, dir)
	}
	if err != nil {
		return err
	}
	if age := now.Sub(fi.ModTime()); age > maxAge {
		return fmt.Errorf("not %s: the %s file is %s old, the limit is %s", kind, kind, age.Round(time.Second), maxAge)
	}
	return nil
}
