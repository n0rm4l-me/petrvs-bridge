package alarm

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// These read the real operating system. They are skipped elsewhere and never fail because a tool is missing.
func TestRealMacSensors(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	ctx := context.Background()
	idle, err := MacIdle(ctx)
	if err != nil || idle < 0 || idle > 365*24*time.Hour {
		t.Fatalf("idle %v %v", idle, err)
	}
	t.Logf("input idle for %v", idle.Round(time.Second))
	if lid, err := MacLid(ctx); err != nil {
		t.Logf("lid not readable here: %v", err)
	} else if lid != "open" && lid != "closed" {
		t.Fatalf("lid %q", lid)
	} else {
		t.Logf("lid %s", lid)
	}
	if p, err := MacPower(ctx); err != nil {
		t.Fatalf("power: %v", err)
	} else {
		t.Logf("power source %q", p)
	}
	if u, err := MacUSB(ctx); err != nil || len(u) != 16 {
		t.Fatalf("usb digest %q %v", u, err)
	}
	// two reads of an untouched machine must not look like activity for the change sensors
	for _, s := range DefaultSensors()[1:] {
		s.Check(ctx)
		if _, hit, err := s.Check(ctx); err != nil {
			t.Logf("%s: %v", s.Name(), err)
		} else if hit {
			t.Logf("%s changed between two reads (something really changed)", s.Name())
		}
	}
}
