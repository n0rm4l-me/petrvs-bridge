package smoke

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The real hub binary with a health directory: its probes pass while it runs, and SIGTERM ends it in order and with exit code 0.
func TestTheRealHubIsProbedAndStopsOnSIGTERM(t *testing.T) {
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("PUBSUB_EMULATOR_HOST not set")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for _, c := range []string{"hub", "bridgekeys"} {
		must(t, exec.Command("go", "build", "-o", filepath.Join(bin, c), "github.com/n0rm4l-me/petrvs-bridge/cmd/"+c))
	}
	keys, agents := filepath.Join(dir, "keys"), filepath.Join(dir, "agents")
	os.MkdirAll(agents, 0o700)
	for _, n := range []string{"hub", "laptop"} {
		must(t, exec.Command(filepath.Join(bin, "bridgekeys"), "gen", "-name", n, "-out", keys))
	}
	must(t, exec.Command("cp", filepath.Join(keys, "laptop.pub.json"), agents))
	const token = "1:probe-token"
	srv := httptest.NewServer((&fakeTelegram{}).handler(token))
	t.Cleanup(srv.Close)

	healthDir := filepath.Join(dir, "health")
	env := []string{"PB_PROJECT=pb-smoke", "PB_PREFIX=probe" + time.Now().Format("150405"), "PB_CREATE_RESOURCES=true",
		"PB_IDENTITY=" + filepath.Join(keys, "hub.identity.json"), "PB_AGENTS_DIR=" + agents, "PB_CHAT_ID=1001",
		"TELEGRAM_BOT_TOKEN=" + token, "PB_TELEGRAM_API_BASE=" + srv.URL, "PB_POLL_TIMEOUT=1s",
		"PB_HEALTH_DIR=" + healthDir, "PB_SHUTDOWN_TIMEOUT=5s"}
	hub := start(t, filepath.Join(bin, "hub"), env)
	exited := make(chan error, 1)
	go func() { exited <- hub.cmd.Wait() }()

	probe := func(kind string) error {
		c := exec.Command(filepath.Join(bin, "hub"), "probe", kind)
		c.Env = append(os.Environ(), "PB_HEALTH_DIR="+healthDir) // no other setting of the hub: the probe needs none
		return c.Run()
	}
	for _, kind := range []string{"live", "ready"} {
		eventually(t, "`hub probe "+kind+"` to pass\n"+hub.output(), 30*time.Second, func() bool { return probe(kind) == nil })
	}

	start := time.Now()
	if err := hub.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the hub did not exit with 0 after SIGTERM: %v\n%s", err, hub.output())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the hub did not stop within 10 s of SIGTERM\n%s", hub.output())
	}
	if took := time.Since(start); took > 7*time.Second {
		t.Errorf("the hub took %v to stop", took)
	}
	for _, want := range []string{"hub stopping", "hub stopped"} {
		if !strings.Contains(hub.output(), want) {
			t.Errorf("the log lacks %q:\n%s", want, hub.output())
		}
	}
	if err := probe("ready"); err == nil {
		t.Error("the hub was still ready after it stopped")
	}
}
