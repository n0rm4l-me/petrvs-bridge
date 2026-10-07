package sysinfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Outputs of `pmset -g batt` as macOS prints them.
const (
	pmsetDischarging = "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=4653155)\t61%; discharging; 5:12 remaining present: true\n"
	pmsetCharging    = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=4653155)\t83%; charging; 1:10 remaining present: true\n"
	pmsetFinishing   = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=4653155)\t99%; finishing charge; 0:05 remaining present: true\n"
	pmsetCharged     = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=4653155)\t100%; charged; 0:00 remaining present: true\n"
	pmsetHeldByMac   = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=4653155)\t80%; AC attached; not charging present: true\n"
	pmsetDesktop     = "Now drawing from 'AC Power'\n"
	pmsetRemoved     = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=4653155)\t0%; discharging; (no estimate) present: false\n"
)

func TestPmsetBatteryStates(t *testing.T) {
	for name, c := range map[string]struct {
		out      string
		pct      int
		charging bool
		ok       bool
	}{
		"discharging":                    {pmsetDischarging, 61, false, true},
		"charging":                       {pmsetCharging, 83, true, true},
		"finishing charge":               {pmsetFinishing, 99, true, true},
		"charged":                        {pmsetCharged, 100, false, true},
		"on the cable but not charging":  {pmsetHeldByMac, 80, false, true},
		"a desktop has no battery":       {pmsetDesktop, 0, false, false},
		"a battery that is not present":  {pmsetRemoved, 0, false, false},
		"nothing":                        {"", 0, false, false},
		"garbage":                        {"InternalBattery but no number\n", 0, false, false},
		"a percentage beyond 100":        {" -InternalBattery-0\t340%; charging;\n", 0, false, false},
		"an id full of digits is no pct": {" -InternalBattery-0 (id=4653155)\tunknown; charging\n", 0, false, false},
	} {
		pct, ch, ok := ParsePmset(c.out)
		if pct != c.pct || ch != c.charging || ok != c.ok {
			t.Errorf("%s: got %d %v %v, want %d %v %v", name, pct, ch, ok, c.pct, c.charging, c.ok)
		}
	}
}

func TestBoottimeAndProcUptime(t *testing.T) {
	now := time.Unix(1759843200+3*86400+4*3600+1800, 0)
	d, ok := ParseBoottime("{ sec = 1759843200, usec = 123456 } Tue Oct  7 09:20:00 2026\n", now)
	if !ok || d != 3*24*time.Hour+4*time.Hour+30*time.Minute {
		t.Fatalf("%v %v", d, ok)
	}
	for _, bad := range []string{"", "no seconds here", "{ sec = 0, usec = 0 }", "{ sec = 99999999999, usec = 0 }", "{ sec = abc }"} {
		if _, ok := ParseBoottime(bad, now); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
	if d, ok := ParseProcUptime("350735.47 234388.90\n"); !ok || d.Round(time.Second) != 350735*time.Second {
		t.Fatalf("%v %v", d, ok)
	}
	for _, bad := range []string{"", "   ", "abc 1", "-5 1", "NaN 1", "Inf 1"} {
		if _, ok := ParseProcUptime(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestLinuxBatteryFromCapacityAndStatus(t *testing.T) {
	for name, c := range map[string]struct {
		caps, states []string
		pct          int
		charging, ok bool
	}{
		"one battery, discharging":   {[]string{"61\n"}, []string{"Discharging\n"}, 61, false, true},
		"charging":                   {[]string{"83\n"}, []string{"Charging\n"}, 83, true, true},
		"full":                       {[]string{"100\n"}, []string{"Full\n"}, 100, false, true},
		"not charging":               {[]string{"80\n"}, []string{"Not charging\n"}, 80, false, true},
		"two batteries average":      {[]string{"50\n", "81\n"}, []string{"Discharging\n", "Charging\n"}, 66, true, true},
		"a bad capacity is kept out": {[]string{"abc\n", "40\n"}, []string{"Unknown\n"}, 40, false, true},
		"no capacity at all":         {nil, nil, 0, false, false},
		"only nonsense":              {[]string{"500\n", "-3\n"}, nil, 0, false, false},
	} {
		pct, ch, ok := ParseLinuxBattery(c.caps, c.states)
		if pct != c.pct || ch != c.charging || ok != c.ok {
			t.Errorf("%s: got %d %v %v", name, pct, ch, ok)
		}
	}
}

// linuxProbe builds a probe over a fake /sys and /proc that live in a temp directory.
func linuxProbe(t *testing.T, files map[string]string) *Probe {
	t.Helper()
	p := &Probe{GOOS: "linux", Timeout: time.Second, TTL: time.Minute, Now: time.Now}
	p.ReadFile = func(path string) ([]byte, error) {
		v, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(v), nil
	}
	p.Glob = func(pattern string) ([]string, error) {
		var out []string
		for k := range files {
			if strings.HasSuffix(k, "/capacity") {
				dir := strings.TrimSuffix(k, "/capacity")
				if ok, _ := filepath.Match(pattern, dir); ok {
					out = append(out, dir)
				}
			}
		}
		return out, nil
	}
	return p
}

func TestLinuxPathsAreReadFromTheSysAndProcFiles(t *testing.T) {
	p := linuxProbe(t, map[string]string{
		"/sys/class/power_supply/BAT0/capacity": "72\n",
		"/sys/class/power_supply/BAT0/status":   "Charging\n",
		"/proc/uptime":                          "93784.55 1000.00\n",
	})
	pct, ch, ok := p.Battery(context.Background())
	if pct != 72 || !ch || !ok {
		t.Fatalf("%d %v %v", pct, ch, ok)
	}
	d, ok := p.Uptime()
	if !ok || d.Round(time.Second) != 93785*time.Second {
		t.Fatalf("%v %v", d, ok)
	}
	if got := FormatUptime(d); got != "1d2h" {
		t.Fatalf("%q", got)
	}
}

func TestALinuxDesktopHasNoBatteryAndAPresentFlagOfZeroIsHonoured(t *testing.T) {
	if _, _, ok := linuxProbe(t, map[string]string{"/proc/uptime": "5 5"}).Battery(context.Background()); ok {
		t.Fatal("a desktop reported a battery")
	}
	p := linuxProbe(t, map[string]string{
		"/sys/class/power_supply/BAT0/capacity": "0\n",
		"/sys/class/power_supply/BAT0/present":  "0\n",
	})
	if _, _, ok := p.Battery(context.Background()); ok {
		t.Fatal("a battery that is not present was reported")
	}
}

// darwinProbe answers the two commands the probe runs from the given outputs and counts the runs.
func darwinProbe(pmset, boottime string, runs *atomic.Int32) *Probe {
	return &Probe{GOOS: "darwin", Timeout: time.Second, TTL: time.Minute, Now: time.Now,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			runs.Add(1)
			switch name + " " + strings.Join(args, " ") {
			case "pmset -g batt":
				return []byte(pmset), nil
			case "sysctl -n kern.boottime":
				return []byte(boottime), nil
			}
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		}}
}

func TestMacCommandsAreRunWithFixedArguments(t *testing.T) {
	var runs atomic.Int32
	p := darwinProbe(pmsetCharging, fmt.Sprintf("{ sec = %d, usec = 1 }", time.Now().Add(-26*time.Hour).Unix()), &runs)
	if pct, ch, ok := p.Battery(context.Background()); pct != 83 || !ch || !ok {
		t.Fatalf("%d %v %v", pct, ch, ok)
	}
	if d, ok := p.Uptime(); !ok || FormatUptime(d) != "1d2h" {
		t.Fatalf("%v %v", d, ok)
	}
}

func TestResultsAreKeptForThirtySecondsAndThenRead(t *testing.T) {
	var runs atomic.Int32
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	p := darwinProbe(pmsetDischarging, "", &runs)
	p.TTL = DefaultTTL
	p.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	move := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	for i := 0; i < 5; i++ {
		p.Battery(context.Background())
	}
	if runs.Load() != 1 {
		t.Fatalf("%d runs within the TTL", runs.Load())
	}
	move(29 * time.Second)
	p.Battery(context.Background())
	if runs.Load() != 1 {
		t.Fatal("read again before the TTL")
	}
	move(2 * time.Second)
	p.Battery(context.Background())
	if runs.Load() != 2 {
		t.Fatalf("%d runs after the TTL", runs.Load())
	}
}

func TestACommandThatHangsCostsOneTimeoutPerTTLAndNothingAfter(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	defer close(release)
	p := &Probe{GOOS: "darwin", Timeout: 60 * time.Millisecond, TTL: time.Minute, Now: time.Now,
		// Ignores its context, like a program stuck in the kernel.
		Run: func(context.Context, string, ...string) ([]byte, error) { runs.Add(1); <-release; return nil, nil }}
	start := time.Now()
	if _, _, ok := p.Battery(context.Background()); ok {
		t.Fatal("a hanging read gave a value")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("waited %v for a command that hangs", took)
	}
	start = time.Now()
	for i := 0; i < 20; i++ {
		p.Battery(context.Background())
	}
	if took := time.Since(start); took > 50*time.Millisecond || runs.Load() != 1 {
		t.Fatalf("the failure was not kept: %v, %d runs", took, runs.Load())
	}
}

func TestAHungReadIsNotStartedAgainWhileItIsStillRunning(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	defer close(release)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	p := &Probe{GOOS: "darwin", Timeout: 30 * time.Millisecond, TTL: time.Second,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Run: func(context.Context, string, ...string) ([]byte, error) { runs.Add(1); <-release; return nil, nil }}
	p.Battery(context.Background())
	mu.Lock()
	now = now.Add(time.Hour)
	mu.Unlock()
	start := time.Now()
	p.Battery(context.Background())
	if runs.Load() != 1 || time.Since(start) > 20*time.Millisecond {
		t.Fatalf("a second reader was started: %d runs, %v", runs.Load(), time.Since(start))
	}
}

func TestACommandThatFailsGivesNoValue(t *testing.T) {
	p := &Probe{GOOS: "darwin", Now: time.Now, Run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not found") }}
	if _, _, ok := p.Battery(context.Background()); ok {
		t.Fatal("a failed command gave a battery")
	}
	if _, ok := p.Uptime(); ok {
		t.Fatal("a failed command gave an uptime")
	}
}

func TestACancelledCallerDoesNotLeaveAFailureInTheCache(t *testing.T) {
	var runs atomic.Int32
	p := darwinProbe(pmsetCharging, "", &runs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Battery(ctx)
	if pct, _, ok := p.Battery(context.Background()); !ok || pct != 83 {
		t.Fatalf("%d %v", pct, ok)
	}
}

func TestAnUnknownSystemGivesNothing(t *testing.T) {
	p := &Probe{GOOS: "plan9", Now: time.Now}
	if _, _, ok := p.Battery(context.Background()); ok {
		t.Fatal("battery")
	}
	if _, ok := p.Uptime(); ok {
		t.Fatal("uptime")
	}
	if got := p.Facts(context.Background(), ""); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestDiskFreeOfThisMachineAndOfAMissingPath(t *testing.T) {
	free, total, ok := DiskFree(t.TempDir())
	if !ok || total == 0 || free > total {
		t.Fatalf("%d of %d %v", free, total, ok)
	}
	if _, _, ok := New().DiskFree(filepath.Join(t.TempDir(), "no", "such", "dir")); ok {
		t.Fatal("a path that does not exist was measured")
	}
}

func TestDiskNumbersThatMakeNoSenseAreRejected(t *testing.T) {
	for name, c := range map[string][2]uint64{"free above total": {10, 5}, "no total": {0, 0}} {
		p := &Probe{Now: time.Now, Statfs: func(string) (uint64, uint64, bool) { return c[0], c[1], true }}
		if _, _, ok := p.DiskFree("/x"); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFactsAreWrittenInTheWordsOfTheCard(t *testing.T) {
	p := darwinProbe(pmsetCharging, fmt.Sprintf("{ sec = %d, usec = 1 }", time.Now().Add(-(3*24+4)*time.Hour-time.Minute).Unix()), new(atomic.Int32))
	p.Statfs = func(path string) (uint64, uint64, bool) { return 212 << 30, 460 << 30, path == "/work" }
	got := p.Facts(context.Background(), "/work")
	want := map[string]string{"bat": "83% charging", "up": "3d4h", "disk": "212G free of 460G"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != 3 {
		t.Errorf("%v", got)
	}
	// A desktop: no battery, and no disk path means no disk fact.
	q := darwinProbe(pmsetDesktop, "", new(atomic.Int32))
	if got := q.Facts(context.Background(), ""); len(got) != 0 {
		t.Errorf("a desktop with nothing to measure reported %v", got)
	}
}

func TestFormats(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0m", 59 * time.Second: "0m", 12 * time.Minute: "12m", 4*time.Hour + 12*time.Minute: "4h12m",
		24 * time.Hour: "1d0h", 3*24*time.Hour + 4*time.Hour + 59*time.Minute: "3d4h", -time.Hour: "0m",
	} {
		if got := FormatUptime(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	if FormatBattery(61, false) != "61%" || FormatBattery(83, true) != "83% charging" {
		t.Error("battery")
	}
	for _, c := range []struct {
		free, total uint64
		want        string
	}{
		{212 << 30, 460 << 30, "212G free of 460G"},
		{3 << 30, 10 << 30, "3G free of 10G"},
		{(11 << 30) / 2, 500 << 30, "5.5G free of 500G"},
		{512 << 20, 2 << 30, "512M free of 2G"},
		{(18 << 40) / 10, 2 << 40, "1.8T free of 2T"},
		{0, 1 << 30, "0B free of 1G"},
		{900, 1000, "900B free of 1000B"},
	} {
		if got := FormatDisk(c.free, c.total); got != c.want {
			t.Errorf("%d/%d: %q, want %q", c.free, c.total, got, c.want)
		}
	}
}

func TestShellNames(t *testing.T) {
	for _, prog := range []string{"zsh", "bash", "sh", "fish", "dash", "ksh", "tcsh", "login", "-zsh", "/bin/bash", " ZSH ", "-bash"} {
		if !IsShell(prog) {
			t.Errorf("%q is a shell", prog)
		}
	}
	for _, prog := range []string{"", "claude", "node", "2.1.92", "vim", "python3", "zshrc", "bash-completion", "claude-code"} {
		if IsShell(prog) {
			t.Errorf("%q is no shell", prog)
		}
	}
}
