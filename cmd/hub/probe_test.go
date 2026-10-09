package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/health"
)

func probeWith(dir string, args ...string) (int, string) {
	var out bytes.Buffer
	env := func(k string) string {
		if k == "PB_HEALTH_DIR" {
			return dir
		}
		return ""
	}
	return probe(args, env, time.Now(), &out), out.String()
}

// The kubelet runs `hub probe live` and `hub probe ready` in the container, with the settings of the container.
func TestProbePassesWhenTheHubHasTouchedItsFiles(t *testing.T) {
	dir := t.TempDir()
	b, _ := health.NewBeacon(dir)
	_ = b.Alive()
	_ = b.Ready(true)
	for _, kind := range []string{"live", "ready"} {
		if code, out := probeWith(dir, kind); code != 0 || out != "" {
			t.Errorf("%s: exit %d, said %q", kind, code, out)
		}
	}
}

func TestProbeFailsWithAReasonWhenTheHubIsNotThere(t *testing.T) {
	code, out := probeWith(t.TempDir(), "ready")
	if code != 1 || !strings.Contains(out, "not ready") {
		t.Fatalf("exit %d, said %q", code, out)
	}
}

func TestProbeFailsWhenTheHubStoppedBeating(t *testing.T) {
	dir := t.TempDir()
	b, _ := health.NewBeacon(dir)
	_ = b.Alive()
	var out bytes.Buffer
	code := probe([]string{"-dir", dir, "-max-age", "30s", "live"}, noEnv, time.Now().Add(2*time.Minute), &out)
	if code != 1 || !strings.Contains(out.String(), "old") {
		t.Fatalf("exit %d, said %q", code, out.String())
	}
}

func TestProbeNeedsADirectoryAndAKind(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		args []string
	}{{"", []string{"live"}}, {t.TempDir(), nil}, {t.TempDir(), []string{"live", "ready"}}, {t.TempDir(), []string{"-max-age", "0s", "live"}}} {
		if code, _ := probeWith(tc.dir, tc.args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", tc.args, code)
		}
	}
	if code, out := probeWith(t.TempDir(), "started"); code != 1 || !strings.Contains(out, "unknown check") {
		t.Errorf("an unknown kind: exit %d, said %q", code, out)
	}
}

// `run` hands the word probe to the probe before it reads any setting of the hub, which the container's probe does not have.
func TestProbeNeedsNoSettingsOfTheHub(t *testing.T) {
	dir := t.TempDir()
	b, _ := health.NewBeacon(dir)
	_ = b.Alive()
	env := func(k string) string {
		if k == "PB_HEALTH_DIR" {
			return dir
		}
		return ""
	}
	if code := run([]string{"probe", "live"}, env); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
