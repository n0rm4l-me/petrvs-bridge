package agentsvc

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func machSampleFacts() map[string]string {
	return map[string]string{"bat": "83% charging", "up": "3d4h", "disk": "212G free of 460G"}
}

func machHeartbeatMeta(t *testing.T, r *machRig) map[string]string {
	t.Helper()
	meta := map[string]string{"locked": "false", "host": "laptop", "os": "darwin"}
	r.a.addFacts(context.Background(), meta)
	if err := (&wire.Payload{ID: "h", Type: wire.TypeHeartbeat, Meta: meta, Exp: 1}).Validate(true); err != nil {
		t.Fatalf("the hub would refuse the heartbeat: %v", err)
	}
	return meta
}

func TestTheHeartbeatCarriesTheFactsOfTheStatusCard(t *testing.T) {
	r := newMachRig(t, Extras{Facts: machSampleFacts})
	r.a.cfg.Spool.Add(wire.TypeReply, "waiting")
	r.a.cfg.Spool.Add(wire.TypeNotice, "waiting too")
	meta := machHeartbeatMeta(t, r)
	want := map[string]string{"bat": "83% charging", "up": "3d4h", "disk": "212G free of 460G", "tmux": "ok", "proc": "claude", "mode": "auto", "q": "2",
		"locked": "false", "host": "laptop", "os": "darwin"}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("%s = %q, want %q", k, meta[k], v)
		}
	}
	if len(meta) != len(want) {
		t.Errorf("%v", meta)
	}
}

func TestFactsOfAMissingSessionAndOfAShell(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.pane.set(func(f *machPane) { f.gone = true })
	meta := machHeartbeatMeta(t, r)
	if meta["tmux"] != "missing" || meta["proc"] != "" || meta["mode"] != "" {
		t.Fatalf("%v", meta)
	}
	if _, ok := meta["proc"]; ok {
		t.Fatal("an unknown program is left out")
	}
	r = newMachRig(t, Extras{})
	r.pane.shell("zsh")
	meta = machHeartbeatMeta(t, r)
	if meta["tmux"] != "ok" || meta["proc"] != "zsh" {
		t.Fatalf("%v", meta)
	}
	if _, ok := meta["mode"]; ok {
		t.Fatal("a mode was read from a shell")
	}
}

func TestClaudeCodeUnderAShellNameIsRecognisedByItsMode(t *testing.T) {
	// A wrapper script runs Claude Code: the pane's program is bash, the screen is Claude Code's.
	r := newMachRig(t, Extras{})
	r.pane.set(func(f *machPane) { f.prog = "bash" })
	meta := machHeartbeatMeta(t, r)
	if meta["proc"] != "bash" || meta["mode"] != "auto" {
		t.Fatalf("%v", meta)
	}
}

func TestTheModeIsReadAgainAtOnceWhenClaudeCodeComesBack(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.a.mach.modeEvery = time.Hour
	r.pane.shell("zsh")
	if _, ok := machHeartbeatMeta(t, r)["mode"]; ok {
		t.Fatal("a mode at a shell")
	}
	r.pane.claude()
	if meta := machHeartbeatMeta(t, r); meta["mode"] != "auto" {
		t.Fatalf("the mode of the restarted Claude Code waited for the hour: %v", meta)
	}
}

func TestFactsWithoutAScreenOrASpoolOrASourceAreJustLeftOut(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.a.cfg.Screen, r.a.cfg.Spool = nil, nil
	meta := machHeartbeatMeta(t, r)
	for _, k := range []string{"tmux", "cc", "mode", "q", "bat", "up", "disk"} {
		if _, ok := meta[k]; ok {
			t.Errorf("%s was reported", k)
		}
	}
	if len(meta) != 3 {
		t.Errorf("%v", meta)
	}
}

func TestAPartialSourceGivesPartialFacts(t *testing.T) {
	// A desktop has no battery.
	r := newMachRig(t, Extras{Facts: func() map[string]string { return map[string]string{"up": "9h1m"} }})
	meta := machHeartbeatMeta(t, r)
	if _, ok := meta["bat"]; ok || meta["up"] != "9h1m" {
		t.Fatalf("%v", meta)
	}
}

func TestASourceOfFactsThatHangsDoesNotHoldUpTheHeartbeat(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	r := newMachRig(t, Extras{Facts: func() map[string]string { calls.Add(1); <-release; return machSampleFacts() }})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	meta := map[string]string{"locked": "false"}
	begin := time.Now()
	r.a.addFacts(ctx, meta)
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("the heartbeat waited %v", took)
	}
	if _, ok := meta["bat"]; ok {
		t.Fatal("facts from a call that never returned")
	}
	if meta["tmux"] != "ok" {
		t.Fatalf("the rest of the facts were lost: %v", meta)
	}
	// The next heartbeats do not start more calls while the first one hangs.
	for i := 0; i < 5; i++ {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		r.a.addFacts(c, map[string]string{})
		cancel()
	}
	if calls.Load() != 1 {
		t.Fatalf("%d calls of a source that is still running", calls.Load())
	}
}

func TestASlowSourceIsReportedWithItsLastResultOnceItHasComeBack(t *testing.T) {
	var slow atomic.Bool
	gate := make(chan struct{})
	r := newMachRig(t, Extras{Facts: func() map[string]string {
		if slow.Load() {
			<-gate
		}
		return machSampleFacts()
	}})
	meta := machHeartbeatMeta(t, r)
	if meta["bat"] == "" {
		t.Fatal("no first result")
	}
	slow.Store(true)
	c, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	meta = map[string]string{}
	r.a.addFacts(c, meta)
	if meta["bat"] != "83% charging" {
		t.Fatalf("the last known result was not used: %v", meta)
	}
	close(gate)
}

func TestASourceThatPanicsIsSurvived(t *testing.T) {
	r := newMachRig(t, Extras{Facts: func() map[string]string { panic("boom") }})
	meta := machHeartbeatMeta(t, r)
	if meta["tmux"] != "ok" {
		t.Fatalf("%v", meta)
	}
	// And it is called again the next time.
	machHeartbeatMeta(t, r)
}

func TestFactsStayWithinTheLimitsOfTheWireAndNeverReplaceWhatIsThere(t *testing.T) {
	big := map[string]string{"locked": "true", "host": "evil"} // must not override
	for i := 0; i < 40; i++ {
		big["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = strings.Repeat("é", 300)
	}
	big[strings.Repeat("k", 40)] = "long key"
	r := newMachRig(t, Extras{Facts: func() map[string]string { return big }})
	meta := machHeartbeatMeta(t, r)
	if len(meta) > 16 {
		t.Fatalf("%d entries", len(meta))
	}
	if meta["locked"] != "false" || meta["host"] != "laptop" {
		t.Fatalf("a fact replaced a key of the heartbeat: %v", meta)
	}
	for k, v := range meta {
		if len(v) > 256 || len(k) > 32 {
			t.Errorf("%s: %d bytes", k, len(v))
		}
		if !utf8.ValidString(v) {
			t.Errorf("%s: a character was cut in half", k)
		}
	}
	if meta["tmux"] != "ok" || meta["q"] == "" {
		t.Fatalf("the regular facts lost their place to the extra ones: %v", meta)
	}
}

func TestFactsAreOneLineEach(t *testing.T) {
	r := newMachRig(t, Extras{Facts: func() map[string]string { return map[string]string{"disk": "212G\nfree of\t460G\x00"} }})
	meta := machHeartbeatMeta(t, r)
	if meta["disk"] != "212G free of 460G" {
		t.Fatalf("%q", meta["disk"])
	}
}

func TestTheModeIsReadFromTheScreenAtMostOnceInAWhile(t *testing.T) {
	r := newMachRig(t, Extras{})
	r.a.mach.modeEvery = time.Hour
	for i := 0; i < 6; i++ {
		if meta := machHeartbeatMeta(t, r); meta["mode"] != "auto" {
			t.Fatalf("%v", meta)
		}
	}
	if r.pane.captures != 1 {
		t.Fatalf("%d captures for 6 heartbeats", r.pane.captures)
	}
	r.a.mach.modeEvery = 0
	r.pane.set(func(f *machPane) { f.screen = strings.ReplaceAll(machIdleScreen, "auto mode on", "plan mode on") })
	if meta := machHeartbeatMeta(t, r); meta["mode"] != "plan" {
		t.Fatalf("%v", meta)
	}
}

func TestATmuxThatHangsOnlyCostsTheFactsThatNeedIt(t *testing.T) {
	old := paneTimeout
	paneTimeout = 40 * time.Millisecond
	t.Cleanup(func() { paneTimeout = old })
	r := newMachRig(t, Extras{Facts: machSampleFacts})
	r.pane.set(func(f *machPane) { f.hang = true })
	begin := time.Now()
	meta := machHeartbeatMeta(t, r)
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("%v", took)
	}
	if _, ok := meta["tmux"]; ok {
		t.Fatalf("a tmux that did not answer was reported as ok or missing: %v", meta)
	}
	if meta["bat"] == "" || meta["q"] == "" {
		t.Fatalf("%v", meta)
	}
}
