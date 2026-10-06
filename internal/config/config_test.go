package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudebridge/internal/approval"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func hubEnv() map[string]string {
	return map[string]string{"CB_PROJECT": "p", "CB_IDENTITY": "/x/hub.identity.json", "CB_AGENTS_DIR": "/x/agents", "CB_CHAT_ID": "4242", "TELEGRAM_BOT_TOKEN": "1:secret"}
}

func TestHubDefaults(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if h.Prefix != "cb" || h.UserID != 4242 || h.OfflineAfter != 3*time.Minute || h.TextTTL != 10*time.Minute || h.KeyTTL != time.Minute ||
		h.PollTimeout != 25*time.Second || h.LogLevel != "info" || h.CreateResources || h.Token != "1:secret" {
		t.Fatalf("%+v", h)
	}
}

func TestHubFlagsOverrideTheEnvironment(t *testing.T) {
	e := hubEnv()
	e["CB_PREFIX"] = "fromenv"
	h, err := ParseHub([]string{"-prefix", "fromflag", "-user-id", "7", "-text-ttl", "5m"}, envOf(e))
	if err != nil || h.Prefix != "fromflag" || h.UserID != 7 || h.TextTTL != 5*time.Minute {
		t.Fatalf("%v %+v", err, h)
	}
}

func TestHubReportsEveryProblemAtOnce(t *testing.T) {
	_, err := ParseHub(nil, envOf(map[string]string{}))
	if err == nil {
		t.Fatal("empty configuration accepted")
	}
	for _, want := range []string{"identity", "project", "agents-dir", "chat-id", "bot token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestTheTokenIsNeverAFlag(t *testing.T) {
	e := hubEnv()
	delete(e, "TELEGRAM_BOT_TOKEN")
	if _, err := ParseHub([]string{"-token", "1:secret"}, envOf(e)); err == nil {
		t.Fatal("a token flag was accepted")
	}
	if _, err := ParseHub([]string{"-telegram-bot-token=1:x"}, envOf(e)); err == nil {
		t.Fatal("a token flag was accepted")
	}
}

func TestTokenFromAFileNeedsPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "token")
	os.WriteFile(f, []byte("9:fromfile\n"), 0o644)
	e := hubEnv()
	delete(e, "TELEGRAM_BOT_TOKEN")
	e["CB_TELEGRAM_TOKEN_FILE"] = f
	if _, err := ParseHub(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("a world-readable token file was accepted: %v", err)
	}
	os.Chmod(f, 0o600)
	h, err := ParseHub(nil, envOf(e))
	if err != nil || h.Token != "9:fromfile" {
		t.Fatalf("%v %+v", err, h)
	}
}

func TestTheEmulatorMakesTheProjectOptional(t *testing.T) {
	e := hubEnv()
	delete(e, "CB_PROJECT")
	if _, err := ParseHub(nil, envOf(e)); err == nil {
		t.Fatal("no project and no emulator was accepted")
	}
	e["PUBSUB_EMULATOR_HOST"] = "127.0.0.1:8085"
	if _, err := ParseHub(nil, envOf(e)); err != nil {
		t.Fatal(err)
	}
}

func TestBadValuesAreRejectedWithTheirNames(t *testing.T) {
	e := hubEnv()
	e["CB_OFFLINE_AFTER"] = "soon"
	e["CB_KEY_TTL"] = "-5s"
	e["CB_CHAT_ID"] = "abc"
	e["CB_CREATE_RESOURCES"] = "maybe"
	e["CB_LOG_LEVEL"] = "loud"
	e["CB_PREFIX"] = "Bad Prefix"
	_, err := ParseHub(nil, envOf(e))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"CB_OFFLINE_AFTER", "CB_KEY_TTL", "CB_CHAT_ID", "CB_CREATE_RESOURCES", "log-level", "prefix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func agentEnv(t *testing.T) map[string]string {
	return map[string]string{"CB_PROJECT": "p", "CB_IDENTITY": "/x/laptop.identity.json", "CB_NAME": "laptop", "CB_HUB_PUB": "/x/hub.pub.json", "CB_STATE_DIR": t.TempDir()}
}

func TestAgentDefaults(t *testing.T) {
	a, err := ParseAgent(nil, envOf(agentEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if a.TmuxSession != "claude" || !a.AlarmEnabled || a.Heartbeat != 30*time.Second || a.Handshake != 10*time.Second ||
		a.AlarmCooldown != time.Minute || a.AlarmPoll != time.Second || a.ReceiptTimeout != 30*time.Second || a.AlarmSecret != nil {
		t.Fatalf("%+v", a)
	}
	if filepath.Dir(a.SpoolDir) != a.StateDir || filepath.Base(a.SpoolDir) != "spool" || filepath.Base(a.LockFile) != "locked" || filepath.Base(a.AlarmState) != "alarm-armed" {
		t.Fatalf("state paths: %+v", a)
	}
}

func TestAgentStateDirFallsBackToXDG(t *testing.T) {
	e := agentEnv(t)
	delete(e, "CB_STATE_DIR")
	e["XDG_STATE_HOME"] = "/xdg/state"
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || a.StateDir != "/xdg/state/claude-bridge" {
		t.Fatalf("%v %q", err, a.StateDir)
	}
}

func TestAgentNameIsValidated(t *testing.T) {
	for _, bad := range []string{"", "Laptop", "a b", "../x"} {
		e := agentEnv(t)
		e["CB_NAME"] = bad
		if _, err := ParseAgent(nil, envOf(e)); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}

func TestAlarmSecretFile(t *testing.T) {
	secret, b32, _ := approval.GenerateSecret()
	f := filepath.Join(t.TempDir(), "alarm-secret")
	os.WriteFile(f, []byte(b32+"\n"), 0o600)
	e := agentEnv(t)
	e["CB_ALARM_SECRET_FILE"] = f
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || string(a.AlarmSecret) != string(secret) {
		t.Fatalf("%v", err)
	}
	os.Chmod(f, 0o644)
	if _, err := ParseAgent(nil, envOf(e)); err == nil {
		t.Fatal("a world-readable secret file was accepted")
	}
	os.Chmod(f, 0o600)
	os.WriteFile(f, []byte("not a secret"), 0o600)
	if _, err := ParseAgent(nil, envOf(e)); err == nil {
		t.Fatal("a malformed secret was accepted")
	}
}

func TestAgentAlarmCanBeSwitchedOff(t *testing.T) {
	e := agentEnv(t)
	e["CB_ALARM"] = "off"
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || a.AlarmEnabled {
		t.Fatalf("%v %+v", err, a)
	}
}
