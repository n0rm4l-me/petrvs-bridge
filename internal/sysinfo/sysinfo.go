// Package sysinfo reads the few facts about the machine that the status card shows: battery, uptime and free disk
// space. Each read is bounded in time and cached, so a command that hangs (pmset on a machine under heavy load, statfs
// on a dead network mount) cannot hold up the heartbeat that carries the facts.
//
// Everything that touches the system goes through the fields of Probe, so the parsing and the caching are tested with
// sample outputs of both platforms.
package sysinfo

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults: a read gets two seconds, and its result (also a failure) is kept for thirty.
const (
	DefaultTimeout = 2 * time.Second
	DefaultTTL     = 30 * time.Second
)

// Runner runs a program and returns what it printed. It must stop when ctx ends.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Probe reads the facts. New gives one that reads this machine; tests replace the fields.
type Probe struct {
	GOOS     string
	Run      Runner
	ReadFile func(path string) ([]byte, error)
	Glob     func(pattern string) ([]string, error)
	// Statfs returns the free and the total bytes of the file system that holds path.
	Statfs  func(path string) (free, total uint64, ok bool)
	Now     func() time.Time
	Timeout time.Duration
	TTL     time.Duration

	mu       sync.Mutex
	cache    map[string]entry
	inflight map[string]bool
}

type entry struct {
	at time.Time
	v  any
}

// New returns a probe of this machine.
func New() *Probe {
	return &Probe{GOOS: runtime.GOOS, Run: execRunner, ReadFile: readFile, Glob: filepath.Glob, Statfs: statfs, Now: time.Now, Timeout: DefaultTimeout, TTL: DefaultTTL}
}

var std = New()

// Battery returns the charge in percent and whether the battery is charging. ok is false on a machine without a
// battery, and when the battery cannot be read in time.
func Battery(ctx context.Context) (pct int, charging bool, ok bool) { return std.Battery(ctx) }

// Uptime returns how long the machine has been up.
func Uptime() (time.Duration, bool) { return std.Uptime() }

// DiskFree returns the free and the total bytes of the file system that holds path.
func DiskFree(path string) (free, total uint64, ok bool) { return std.DiskFree(path) }

// shells are the programs that show that a terminal has been left at a prompt.
var shells = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true, "ksh": true, "tcsh": true,
	"csh": true, "login": true, "ash": true, "mksh": true, "nu": true, "pwsh": true}

// IsShell says whether prog, the name of the program in the foreground of a terminal, is a shell. A login shell is named
// with a leading dash ("-zsh"), and some systems give the whole path.
func IsShell(prog string) bool {
	return shells[strings.ToLower(strings.TrimPrefix(filepath.Base(strings.TrimSpace(prog)), "-"))]
}

// FactsFunc returns the function that cmd/agent gives the agent as its source of facts: the keys bat, up and disk,
// those that could be read. diskPath says which file system to measure.
func FactsFunc(diskPath string) func() map[string]string {
	return func() map[string]string { return std.Facts(context.Background(), diskPath) }
}

type batteryValue struct {
	pct      int
	charging bool
	ok       bool
}

type uptimeValue struct {
	d  time.Duration
	ok bool
}

type diskValue struct {
	free, total uint64
	ok          bool
}

// Battery is the method behind the function of the same name.
func (p *Probe) Battery(ctx context.Context) (int, bool, bool) {
	v := cached(p, ctx, "battery", func(ctx context.Context) batteryValue {
		switch p.GOOS {
		case "darwin":
			out, err := p.run(ctx, "pmset", "-g", "batt")
			if err != nil {
				return batteryValue{}
			}
			pct, ch, ok := ParsePmset(string(out))
			return batteryValue{pct, ch, ok}
		case "linux":
			pct, ch, ok := p.linuxBattery()
			return batteryValue{pct, ch, ok}
		}
		return batteryValue{}
	})
	return v.pct, v.charging, v.ok
}

// Uptime is the method behind the function of the same name.
func (p *Probe) Uptime() (time.Duration, bool) {
	v := cached(p, context.Background(), "uptime", func(ctx context.Context) uptimeValue {
		switch p.GOOS {
		case "darwin":
			out, err := p.run(ctx, "sysctl", "-n", "kern.boottime")
			if err != nil {
				return uptimeValue{}
			}
			d, ok := ParseBoottime(string(out), p.now())
			return uptimeValue{d, ok}
		case "linux":
			b, err := p.ReadFile("/proc/uptime")
			if err != nil {
				return uptimeValue{}
			}
			d, ok := ParseProcUptime(string(b))
			return uptimeValue{d, ok}
		}
		return uptimeValue{}
	})
	return v.d, v.ok
}

// DiskFree is the method behind the function of the same name.
func (p *Probe) DiskFree(path string) (uint64, uint64, bool) {
	v := cached(p, context.Background(), "disk:"+path, func(context.Context) diskValue {
		free, total, ok := p.Statfs(path)
		return diskValue{free, total, ok && total > 0 && free <= total}
	})
	return v.free, v.total, v.ok
}

// Facts returns bat, up and disk in the words of the status card ("83% charging", "3d4h", "212G free of 460G"). A fact
// that is not available is left out.
func (p *Probe) Facts(ctx context.Context, diskPath string) map[string]string {
	out := map[string]string{}
	if pct, ch, ok := p.Battery(ctx); ok {
		out["bat"] = FormatBattery(pct, ch)
	}
	if d, ok := p.Uptime(); ok {
		out["up"] = FormatUptime(d)
	}
	if diskPath != "" {
		if free, total, ok := p.DiskFree(diskPath); ok {
			out["disk"] = FormatDisk(free, total)
		}
	}
	return out
}

func (p *Probe) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Probe) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultTimeout
}

func (p *Probe) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.Run == nil {
		return nil, fmt.Errorf("sysinfo: no runner")
	}
	return p.Run(ctx, name, args...)
}

// cached returns the result of f for key, reading again only when the kept one is older than the TTL. f gets at most
// Timeout: when it does not come back in time the answer is the zero value (not available), which is kept like any other
// so that a read that hangs costs one timeout per TTL, not one per heartbeat. A read that is still running when the TTL
// has passed is not started again; the caller gets "not available" until it has come back.
func cached[T any](p *Probe, ctx context.Context, key string, f func(context.Context) T) T {
	var zero T
	if ctx.Err() != nil {
		return zero
	}
	p.mu.Lock()
	if e, ok := p.cache[key]; ok {
		ttl := p.TTL
		if ttl <= 0 {
			ttl = DefaultTTL
		}
		if p.now().Sub(e.at) < ttl {
			p.mu.Unlock()
			return e.v.(T)
		}
	}
	if p.inflight[key] {
		p.mu.Unlock()
		return zero
	}
	if p.inflight == nil {
		p.inflight = map[string]bool{}
	}
	p.inflight[key] = true
	p.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	done := make(chan T, 1)
	go func() {
		v := f(cctx)
		p.mu.Lock()
		delete(p.inflight, key)
		p.mu.Unlock()
		done <- v
	}()
	select {
	case v := <-done:
		if ctx.Err() == nil { // a caller that gave up must not leave a failure behind for the next thirty seconds
			p.keep(key, v)
		}
		return v
	case <-cctx.Done():
		if ctx.Err() == nil {
			p.keep(key, zero)
		}
		return zero
	}
}

func (p *Probe) keep(key string, v any) {
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]entry{}
	}
	p.cache[key] = entry{p.now(), v}
	p.mu.Unlock()
}

// ParsePmset reads the output of `pmset -g batt`. A line of the internal battery looks like
//
//	-InternalBattery-0 (id=4653155)	83%; charging; 1:10 remaining present: true
//
// The states are "charging", "finishing charge", "discharging", "charged" and, with the cable in but no charge going in
// (optimised charging), "AC attached; not charging". A desktop has no such line, and a battery that is not present is
// not reported.
func ParsePmset(out string) (pct int, charging bool, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "InternalBattery") || strings.Contains(line, "present: false") {
			continue
		}
		i := strings.IndexByte(line, '%')
		if i < 1 {
			continue
		}
		j := i
		for j > 0 && line[j-1] >= '0' && line[j-1] <= '9' {
			j--
		}
		n, err := strconv.Atoi(line[j:i])
		if err != nil || n < 0 || n > 100 {
			continue
		}
		for _, f := range strings.Split(line[i+1:], ";") {
			switch strings.ToLower(strings.TrimSpace(f)) {
			case "charging", "finishing charge":
				charging = true
			}
		}
		return n, charging, true
	}
	return 0, false, false
}

var boottimeRe = regexp.MustCompile(`\bsec\s*=\s*(\d+)`)

// ParseBoottime reads `sysctl -n kern.boottime`, for example `{ sec = 1759843200, usec = 123456 } Tue Oct  7 09:20:00 2026`,
// and returns how long before now that was.
func ParseBoottime(out string, now time.Time) (time.Duration, bool) {
	m := boottimeRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	sec, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || sec <= 0 {
		return 0, false
	}
	d := now.Sub(time.Unix(sec, 0))
	if d < 0 || d > 100*365*24*time.Hour {
		return 0, false
	}
	return d, true
}

// ParseProcUptime reads /proc/uptime, for example "350735.47 234388.90": the seconds since boot come first.
func ParseProcUptime(s string) (time.Duration, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, false
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec > 100*365*24*3600 {
		return 0, false
	}
	return time.Duration(sec * float64(time.Second)), true
}

// ParseLinuxBattery combines what /sys/class/power_supply/BAT*/capacity and status say for the batteries of a machine:
// the mean charge, and charging if any of them is. A capacity that is not a percentage is ignored.
func ParseLinuxBattery(capacities, statuses []string) (pct int, charging bool, ok bool) {
	sum, n := 0, 0
	for _, c := range capacities {
		v, err := strconv.Atoi(strings.TrimSpace(c))
		if err != nil || v < 0 || v > 100 {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, false, false
	}
	for _, s := range statuses {
		if strings.EqualFold(strings.TrimSpace(s), "charging") {
			charging = true
		}
	}
	return (sum + n/2) / n, charging, true
}

func (p *Probe) linuxBattery() (int, bool, bool) {
	dirs, err := p.Glob("/sys/class/power_supply/BAT*")
	if err != nil || len(dirs) == 0 {
		return 0, false, false
	}
	var caps, states []string
	for _, d := range dirs {
		if b, err := p.ReadFile(d + "/present"); err == nil && strings.TrimSpace(string(b)) == "0" {
			continue
		}
		c, err := p.ReadFile(d + "/capacity")
		if err != nil {
			continue
		}
		caps = append(caps, string(c))
		if s, err := p.ReadFile(d + "/status"); err == nil {
			states = append(states, string(s))
		}
	}
	return ParseLinuxBattery(caps, states)
}

// FormatBattery writes "83% charging" or "61%".
func FormatBattery(pct int, charging bool) string {
	if charging {
		return fmt.Sprintf("%d%% charging", pct)
	}
	return fmt.Sprintf("%d%%", pct)
}

// FormatUptime writes "3d4h", "4h12m" or "12m".
func FormatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int(d%(24*time.Hour)) / int(time.Hour)
	mins := int(d%time.Hour) / int(time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// FormatDisk writes "212G free of 460G" (powers of 1024, like df -h).
func FormatDisk(free, total uint64) string {
	return size(free) + " free of " + size(total)
}

func size(n uint64) string {
	const k = 1024.0
	v := float64(n)
	for _, u := range []string{"B", "K", "M", "G", "T"} {
		if v < k || u == "T" {
			if u == "B" {
				return fmt.Sprintf("%dB", n)
			}
			if v >= 10 || v == math.Trunc(v) {
				return fmt.Sprintf("%.0f%s", v, u)
			}
			return fmt.Sprintf("%.1f%s", v, u)
		}
		v /= k
	}
	return ""
}
