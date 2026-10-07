package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/approval"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func hubEnv() map[string]string {
	return map[string]string{"PB_PROJECT": "p", "PB_IDENTITY": "/x/hub.identity.json", "PB_AGENTS_DIR": "/x/agents", "PB_CHAT_ID": "4242", "TELEGRAM_BOT_TOKEN": "1:secret"}
}

func TestHubDefaults(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if h.Prefix != "pb" || h.UserID != 4242 || h.OfflineAfter != 3*time.Minute || h.TextTTL != 10*time.Minute || h.KeyTTL != time.Minute ||
		h.PollTimeout != 25*time.Second || h.LogLevel != "info" || h.CreateResources || h.Token != "1:secret" {
		t.Fatalf("%+v", h)
	}
}

func TestHubFlagsOverrideTheEnvironment(t *testing.T) {
	e := hubEnv()
	e["PB_PREFIX"] = "fromenv"
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
	e["PB_TELEGRAM_TOKEN_FILE"] = f
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
	delete(e, "PB_PROJECT")
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
	e["PB_OFFLINE_AFTER"] = "soon"
	e["PB_KEY_TTL"] = "-5s"
	e["PB_CHAT_ID"] = "abc"
	e["PB_CREATE_RESOURCES"] = "maybe"
	e["PB_LOG_LEVEL"] = "loud"
	e["PB_PREFIX"] = "Bad Prefix"
	_, err := ParseHub(nil, envOf(e))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"PB_OFFLINE_AFTER", "PB_KEY_TTL", "PB_CHAT_ID", "PB_CREATE_RESOURCES", "log-level", "prefix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func agentEnv(t *testing.T) map[string]string {
	return map[string]string{"PB_PROJECT": "p", "PB_IDENTITY": "/x/laptop.identity.json", "PB_NAME": "laptop", "PB_HUB_PUB": "/x/hub.pub.json", "PB_STATE_DIR": t.TempDir()}
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
	delete(e, "PB_STATE_DIR")
	e["XDG_STATE_HOME"] = "/xdg/state"
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || a.StateDir != "/xdg/state/petrvs-bridge" {
		t.Fatalf("%v %q", err, a.StateDir)
	}
}

func TestAgentNameIsValidated(t *testing.T) {
	for _, bad := range []string{"", "Laptop", "a b", "../x"} {
		e := agentEnv(t)
		e["PB_NAME"] = bad
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
	e["PB_ALARM_SECRET_FILE"] = f
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
	e["PB_ALARM"] = "off"
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || a.AlarmEnabled {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestNewHubAndAgentSettingsHaveSafeDefaults(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if h.LockFile != "" || h.AllowGroupRead || h.TelegramBase != "https://api.telegram.org" {
		t.Fatalf("%+v", h)
	}
	a, err := ParseAgent(nil, envOf(agentEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if a.ReorderWindow != 2*time.Second || a.AllowGroupRead || !strings.HasSuffix(a.AlarmCodeState, "alarm-code-state") {
		t.Fatalf("%+v", a)
	}
	e := hubEnv()
	e["PB_LOCK_FILE"] = "/var/lib/bridge/lock"
	e["PB_ALLOW_GROUP_READ"] = "true"
	h, err = ParseHub(nil, envOf(e))
	if err != nil || h.LockFile != "/var/lib/bridge/lock" || !h.AllowGroupRead {
		t.Fatalf("%v %+v", err, h)
	}
	ae := agentEnv(t)
	ae["PB_REORDER_WINDOW"] = "5s"
	if a, err = ParseAgent(nil, envOf(ae)); err != nil || a.ReorderWindow != 5*time.Second {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestBotAPIAddressMustNotSendTheTokenInTheClear(t *testing.T) {
	for base, ok := range map[string]bool{
		"https://api.telegram.org":    true,
		"https://bot-api.example.com": true,
		"http://127.0.0.1:8081":       true,
		"http://localhost:8081":       true,
		"http://[::1]:8081":           true,
		"http://api.example.com":      false,
		"http://10.0.0.5:8081":        false,
		"ftp://api.telegram.org":      false,
		"api.telegram.org":            false,
		"https://":                    false,
	} {
		e := hubEnv()
		e["PB_TELEGRAM_API_BASE"] = base
		_, err := ParseHub(nil, envOf(e))
		if (err == nil) != ok {
			t.Errorf("%s: accepted=%v, want %v (%v)", base, err == nil, ok, err)
		}
	}
}

func TestGroupReadableSecretsNeedTheExplicitSetting(t *testing.T) {
	f := filepath.Join(t.TempDir(), "token")
	os.WriteFile(f, []byte("9:fromfile\n"), 0o600)
	os.Chmod(f, 0o640)
	e := hubEnv()
	delete(e, "TELEGRAM_BOT_TOKEN")
	e["PB_TELEGRAM_TOKEN_FILE"] = f
	if _, err := ParseHub(nil, envOf(e)); err == nil {
		t.Fatal("a group-readable token file was accepted without PB_ALLOW_GROUP_READ")
	}
	e["PB_ALLOW_GROUP_READ"] = "true"
	if h, err := ParseHub(nil, envOf(e)); err != nil || h.Token != "9:fromfile" {
		t.Fatalf("%v %+v", err, h)
	}
	os.Chmod(f, 0o604)
	if _, err := ParseHub(nil, envOf(e)); err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
}

func TestReceivedReactionDefaultsToEyesAndCanBeChangedOrSwitchedOff(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil || h.ReceivedReaction != "👀" {
		t.Fatalf("%v %q", err, h.ReceivedReaction)
	}
	for in, want := range map[string]string{"👍": "👍", " 🫡 ": "🫡", "off": "", "OFF": "", "👨‍💻": "👨‍💻"} {
		e := hubEnv()
		e["PB_RECEIVED_REACTION"] = in
		h, err := ParseHub(nil, envOf(e))
		if err != nil || h.ReceivedReaction != want {
			t.Errorf("%q: %v %q", in, err, h.ReceivedReaction)
		}
	}
	h, err = ParseHub([]string{"-received-reaction", "🔥"}, envOf(hubEnv()))
	if err != nil || h.ReceivedReaction != "🔥" {
		t.Fatalf("%v %q", err, h.ReceivedReaction)
	}
}

func TestAReactionThatIsNotAnEmojiIsRefused(t *testing.T) {
	for _, in := range []string{"on", "yes", "true", "ok", "👍 ok", "👀👀👀👀👀"} {
		e := hubEnv()
		e["PB_RECEIVED_REACTION"] = in
		_, err := ParseHub(nil, envOf(e))
		if err == nil || !strings.Contains(err.Error(), "PB_RECEIVED_REACTION") {
			t.Errorf("%q was accepted: %v", in, err)
		}
	}
}

func TestDefaultAgentIsOptionalAndMustBeAValidName(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil || h.DefaultAgent != "" {
		t.Fatalf("%v %q", err, h.DefaultAgent)
	}
	e := hubEnv()
	e["PB_DEFAULT_AGENT"] = "desk"
	if h, err = ParseHub(nil, envOf(e)); err != nil || h.DefaultAgent != "desk" {
		t.Fatalf("%v %q", err, h.DefaultAgent)
	}
	e["PB_DEFAULT_AGENT"] = "Desk Top"
	if _, err = ParseHub(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "default-agent") {
		t.Fatalf("%v", err)
	}
}

func TestDeliveredReactionDefaultsToAThumbAndIsCheckedLikeTheOther(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil || h.DeliveredReaction != "👍" {
		t.Fatalf("%v %q", err, h.DeliveredReaction)
	}
	e := hubEnv()
	e["PB_DELIVERED_REACTION"] = "off"
	if h, err = ParseHub(nil, envOf(e)); err != nil || h.DeliveredReaction != "" {
		t.Fatalf("%v %q", err, h.DeliveredReaction)
	}
	e["PB_DELIVERED_REACTION"] = "done"
	if _, err = ParseHub(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "PB_DELIVERED_REACTION") {
		t.Fatalf("%v", err)
	}
}
