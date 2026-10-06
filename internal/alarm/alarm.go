// Package alarm watches a machine for signs that someone touched it (keyboard,
// mouse, trackpad, lid, power cable, USB) and reports them while it is armed.
//
// Watching costs almost nothing, but reporting is rate limited per sensor so a
// person typing does not flood the chat: the first event is reported at once,
// then at most one report per cooldown with a count of what was held back.
// Commands typed by the agent itself go through tmux, not through the input
// devices, so they do not set the alarm off.
package alarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Event is one observation.
type Event struct {
	Sensor string
	Detail string
}

// Sensor watches one thing.
type Sensor interface {
	Name() string
	// Check reports whether the thing changed since the previous check.
	Check(ctx context.Context) (Event, bool, error)
}

// Resetter is implemented by sensors that keep a baseline, which must be retaken when the alarm is armed.
type Resetter interface{ Reset() }

// Config configures an alarm.
type Config struct {
	Sensors  []Sensor
	Poll     time.Duration // default 1s
	Cooldown time.Duration // default 60s
	// StateFile keeps the armed state across restarts; it exists while the alarm is armed.
	StateFile string
	Now       func() time.Time
	// Notify is called for a reportable event with the number of events held back since the last report.
	Notify func(ctx context.Context, e Event, suppressed int)
}

// Alarm is the armed/disarmed state plus the polling loop.
type Alarm struct {
	cfg Config

	mu         sync.Mutex
	armed      bool
	last       map[string]time.Time
	suppressed map[string]int
	errs       map[string]error
}

// New loads the persisted state, if any.
func New(cfg Config) (*Alarm, error) {
	if len(cfg.Sensors) == 0 {
		return nil, errors.New("alarm: at least one sensor is required")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &Alarm{cfg: cfg, last: map[string]time.Time{}, suppressed: map[string]int{}, errs: map[string]error{}}
	if cfg.StateFile != "" {
		if _, err := os.Stat(cfg.StateFile); err == nil {
			a.armed = true
		}
	}
	return a, nil
}

// SetNotify replaces the notification callback.
func (a *Alarm) SetNotify(f func(ctx context.Context, e Event, suppressed int)) {
	a.mu.Lock()
	a.cfg.Notify = f
	a.mu.Unlock()
}

// Armed reports the state.
func (a *Alarm) Armed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed
}

// Arm turns the alarm on and takes a fresh baseline, so the person who armed it is not reported.
func (a *Alarm) Arm() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed = true
	a.last, a.suppressed, a.errs = map[string]time.Time{}, map[string]int{}, map[string]error{}
	for _, s := range a.cfg.Sensors {
		if r, ok := s.(Resetter); ok {
			r.Reset()
		}
	}
	if a.cfg.StateFile != "" {
		return os.WriteFile(a.cfg.StateFile, []byte("armed\n"), 0o600)
	}
	return nil
}

// Disarm turns the alarm off.
func (a *Alarm) Disarm() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed = false
	if a.cfg.StateFile != "" {
		if err := os.Remove(a.cfg.StateFile); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Blind lists the sensors that failed on their last check: while they fail, the alarm cannot see what they watch.
func (a *Alarm) Blind() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, s := range a.cfg.Sensors {
		if a.errs[s.Name()] != nil {
			out = append(out, s.Name())
		}
	}
	return out
}

// Status describes the alarm in one line.
func (a *Alarm) Status() string {
	state := "disarmed"
	if a.Armed() {
		state = "armed"
	}
	var names []string
	for _, s := range a.cfg.Sensors {
		names = append(names, s.Name())
	}
	out := fmt.Sprintf("alarm %s, watching: %v", state, names)
	if b := a.Blind(); len(b) > 0 {
		out += fmt.Sprintf(" (cannot read: %v)", b)
	}
	return out
}

// Tick does one polling pass. It is exported so tests can drive the alarm without waiting.
func (a *Alarm) Tick(ctx context.Context) {
	if !a.Armed() {
		return
	}
	for _, s := range a.cfg.Sensors {
		ev, hit, err := s.Check(ctx)
		a.mu.Lock()
		if err != nil {
			a.errs[s.Name()] = err
		} else {
			delete(a.errs, s.Name())
		}
		if err != nil || !hit || !a.armed {
			a.mu.Unlock()
			continue
		}
		now := a.cfg.Now()
		name := s.Name()
		last, seen := a.last[name]
		if seen && now.Sub(last) < a.cfg.Cooldown {
			a.suppressed[name]++
			a.mu.Unlock()
			continue
		}
		held := a.suppressed[name]
		a.last[name], a.suppressed[name] = now, 0
		notify := a.cfg.Notify
		a.mu.Unlock()
		if notify != nil {
			notify(ctx, ev, held)
		}
	}
}

// Run polls until ctx ends.
func (a *Alarm) Run(ctx context.Context) {
	t := time.NewTicker(a.cfg.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Tick(ctx)
		}
	}
}
