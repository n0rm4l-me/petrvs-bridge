package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/approval"
	"github.com/n0rm4l-me/petrvs-bridge/internal/slack/slackid"
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
	_, stderr := capture(t)
	if _, err := ParseHub([]string{"-token", "1:secret"}, envOf(e)); err == nil {
		t.Fatal("a token flag was accepted")
	}
	if _, err := ParseHub([]string{"-telegram-bot-token=1:x"}, envOf(e)); err == nil {
		t.Fatal("a token flag was accepted")
	}
	if strings.Contains(stderr.String(), "1:secret") || strings.Contains(stderr.String(), "1:x") {
		t.Fatalf("the usage echoes the token given on the command line:\n%s", stderr)
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
	e["PB_PULL"] = "push"
	_, err := ParseHub(nil, envOf(e))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"PB_OFFLINE_AFTER", "PB_KEY_TTL", "PB_CHAT_ID", "PB_CREATE_RESOURCES", "log-level", "prefix", "pull"} {
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
	if err != nil || h.ReceivedReaction != "" || h.DeliveredReaction != "👀" || h.FailedReaction != "👎" {
		t.Fatalf("%v received=%q delivered=%q failed=%q: the agent's mark is the one that is on by default", err, h.ReceivedReaction, h.DeliveredReaction, h.FailedReaction)
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
	if err != nil || h.DeliveredReaction != "👀" {
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

// The Pub/Sub client refuses an empty project even for the emulator, so configuration must never hand it one.
func TestTheEmulatorGetsAPlaceholderProjectWhenNoneIsSet(t *testing.T) {
	e := hubEnv()
	delete(e, "PB_PROJECT")
	e["PUBSUB_EMULATOR_HOST"] = "127.0.0.1:8085"
	h, err := ParseHub(nil, envOf(e))
	if err != nil || h.Project != "emulator" {
		t.Fatalf("hub: %v %q", err, h.Project)
	}
	ae := agentEnv(t)
	delete(ae, "PB_PROJECT")
	ae["PUBSUB_EMULATOR_HOST"] = "127.0.0.1:8085"
	a, err := ParseAgent(nil, envOf(ae))
	if err != nil || a.Project != "emulator" {
		t.Fatalf("agent: %v %q", err, a.Project)
	}
	// A project that is set is kept, from the variable, from GOOGLE_CLOUD_PROJECT and from the flag.
	e["PB_PROJECT"] = "demo"
	if h, err = ParseHub(nil, envOf(e)); err != nil || h.Project != "demo" {
		t.Fatalf("%v %q", err, h.Project)
	}
	delete(e, "PB_PROJECT")
	e["GOOGLE_CLOUD_PROJECT"] = "fromgoogle"
	if h, err = ParseHub(nil, envOf(e)); err != nil || h.Project != "fromgoogle" {
		t.Fatalf("%v %q", err, h.Project)
	}
	if a, err = ParseAgent([]string{"-project", "flagged"}, envOf(ae)); err != nil || a.Project != "flagged" {
		t.Fatalf("%v %q", err, a.Project)
	}
	// Without the emulator a missing project is still a configuration problem.
	delete(e, "GOOGLE_CLOUD_PROJECT")
	delete(e, "PUBSUB_EMULATOR_HOST")
	if _, err = ParseHub(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("%v", err)
	}
}

// capture replaces the places the usage is written to for the length of a test.
func capture(t *testing.T) (stdout, stderr *bytes.Buffer) {
	t.Helper()
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	oldOut, oldErr := usageOut, usageErr
	usageOut, usageErr = stdout, stderr
	t.Cleanup(func() { usageOut, usageErr = oldOut, oldErr })
	return stdout, stderr
}

func TestHelpPrintsTheUsageOnStandardOutput(t *testing.T) {
	for _, flagName := range []string{"-h", "--help", "-help"} {
		stdout, stderr := capture(t)
		// Even with nothing configured and a token in the environment: help needs no valid configuration and shows no secret.
		e := map[string]string{"TELEGRAM_BOT_TOKEN": "1:secret"}
		_, err := ParseHub([]string{flagName}, envOf(e))
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("hub %s: %v", flagName, err)
		}
		if strings.Contains(err.Error(), "configuration problems") {
			t.Errorf("hub %s: help reported as a configuration problem: %v", flagName, err)
		}
		for _, want := range []string{"Usage of hub", "-agents-dir", "-chat-id", "PB_CHAT_ID", "-telegram-api", `(default "https://api.telegram.org")`} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("hub %s: the usage lacks %q:\n%s", flagName, want, stdout)
			}
		}
		if stderr.Len() != 0 || strings.Contains(stdout.String(), "1:secret") {
			t.Errorf("hub %s: stderr %q, or the token in the usage:\n%s", flagName, stderr, stdout)
		}

		stdout, stderr = capture(t)
		_, err = ParseAgent([]string{flagName}, envOf(agentEnv(t)))
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("agent %s: %v", flagName, err)
		}
		for _, want := range []string{"Usage of agent", "-tmux-session", "-state-dir", "PB_STATE_DIR"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("agent %s: the usage lacks %q:\n%s", flagName, want, stdout)
			}
		}
		if stderr.Len() != 0 {
			t.Errorf("agent %s: help wrote to stderr: %q", flagName, stderr)
		}
	}
}

func TestAnUnknownFlagShowsTheUsageOnStandardError(t *testing.T) {
	stdout, stderr := capture(t)
	_, err := ParseHub([]string{"-frobnicate"}, envOf(hubEnv()))
	if err == nil || errors.Is(err, flag.ErrHelp) || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("%v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "-agents-dir") {
		t.Errorf("stdout %q, stderr:\n%s", stdout, stderr)
	}
	stdout, stderr = capture(t)
	if _, err = ParseAgent([]string{"-frobnicate"}, envOf(agentEnv(t))); err == nil || errors.Is(err, flag.ErrHelp) {
		t.Fatalf("%v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "-tmux-session") {
		t.Errorf("stdout %q, stderr:\n%s", stdout, stderr)
	}
}

func TestFileAndWatchSettingsHaveSafeDefaults(t *testing.T) {
	a, err := ParseAgent(nil, envOf(agentEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxFileBytes != DefaultMaxFileBytes || a.Heal || a.AssistantCommand != "claude --continue" || a.HealGrace != 20*time.Second ||
		a.HealTries != 3 || a.HealWindow != 10*time.Minute || a.StuckAfter != 10*time.Minute || a.Workdir != "" {
		t.Fatalf("%+v", a)
	}
	if a.InboxDir != filepath.Join(a.StateDir, "inbox") {
		t.Errorf("without a workdir the inbox is in the state directory, got %s", a.InboxDir)
	}
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil || h.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("hub: %v %+v", err, h)
	}
}

func TestTheInboxFollowsTheWorkdirAndEverythingCanBeSet(t *testing.T) {
	e := agentEnv(t)
	e["PB_WORKDIR"] = "/work/proj"
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || a.InboxDir != "/work/proj/.petrvs-inbox" || a.Workdir != "/work/proj" {
		t.Fatalf("%v %+v", err, a)
	}
	e["PB_INBOX_DIR"] = "/data/in"
	e["PB_MAX_FILE_BYTES"] = "1048576"
	e["PB_HEAL"] = "true"
	e["PB_CLAUDE_COMMAND"] = "claude --permission-mode auto --continue"
	e["PB_HEAL_GRACE"] = "5s"
	e["PB_HEAL_TRIES"] = "2"
	e["PB_HEAL_WINDOW"] = "1m"
	e["PB_STUCK_AFTER"] = "off"
	a, err = ParseAgent(nil, envOf(e))
	if err != nil || a.InboxDir != "/data/in" || a.MaxFileBytes != 1<<20 || !a.Heal || a.AssistantCommand != "claude --permission-mode auto --continue" ||
		a.HealGrace != 5*time.Second || a.HealTries != 2 || a.HealWindow != time.Minute || a.StuckAfter != 0 {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestBadFileAndWatchSettingsAreRefused(t *testing.T) {
	for name, set := range map[string]map[string]string{
		"heal without a workdir":  {"PB_HEAL": "true"},
		"relative workdir":        {"PB_WORKDIR": "proj"},
		"relative inbox":          {"PB_INBOX_DIR": "in"},
		"negative file limit":     {"PB_MAX_FILE_BYTES": "-1"},
		"file limit over 50 MiB":  {"PB_MAX_FILE_BYTES": "52428801"},
		"two-line claude command": {"PB_CLAUDE_COMMAND": "claude\nrm -rf /"},
		"negative stuck time":     {"PB_STUCK_AFTER": "-1m"},
		"stuck time nonsense":     {"PB_STUCK_AFTER": "soon"},
		"negative tries":          {"PB_HEAL_TRIES": "-1"},
	} {
		e := agentEnv(t)
		for k, v := range set {
			e[k] = v
		}
		if _, err := ParseAgent(nil, envOf(e)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	e := hubEnv()
	e["PB_MAX_FILE_BYTES"] = "52428801"
	if _, err := ParseHub(nil, envOf(e)); err == nil {
		t.Error("a hub file limit over 50 MiB was accepted")
	}
	e = agentEnv(t)
	e["PB_HEAL"], e["PB_WORKDIR"] = "true", "/work"
	if _, err := ParseAgent(nil, envOf(e)); err != nil {
		t.Errorf("heal with a workdir: %v", err)
	}
}

func TestHubHealthAndShutdownSettings(t *testing.T) {
	e := hubEnv()
	h, err := ParseHub(nil, envOf(e))
	if err != nil {
		t.Fatal(err)
	}
	if h.HealthDir != "" || h.ShutdownTimeout != 20*time.Second {
		t.Fatalf("defaults: %q %v", h.HealthDir, h.ShutdownTimeout)
	}
	e["PB_HEALTH_DIR"], e["PB_SHUTDOWN_TIMEOUT"] = "/health", "25s"
	h, err = ParseHub(nil, envOf(e))
	if err != nil || h.HealthDir != "/health" || h.ShutdownTimeout != 25*time.Second {
		t.Fatalf("%v %+v", err, h)
	}
	for k, v := range map[string]string{"PB_HEALTH_DIR": "relative/dir", "PB_SHUTDOWN_TIMEOUT": "0s"} {
		bad := hubEnv()
		bad[k] = v
		if _, err := ParseHub(nil, envOf(bad)); err == nil {
			t.Errorf("%s=%q was accepted", k, v)
		}
	}
	long := hubEnv()
	long["PB_SHUTDOWN_TIMEOUT"] = "6m"
	if _, err := ParseHub(nil, envOf(long)); err == nil {
		t.Error("a shutdown timeout above five minutes was accepted")
	}
}

func TestPullDefaultsToTheStreamForTheHubAndToShortCallsForTheAgent(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil || h.Pull != "stream" {
		t.Fatalf("hub: %v %+v", err, h)
	}
	a, err := ParseAgent(nil, envOf(agentEnv(t)))
	if err != nil || a.Pull != "unary" {
		t.Fatalf("agent: %v %+v", err, a)
	}
}

func TestPullCanBeChosenForEitherProgramByTheEnvironmentOrAFlag(t *testing.T) {
	e := agentEnv(t)
	e["PB_PULL"] = "stream"
	a, err := ParseAgent(nil, envOf(e))
	if err != nil || a.Pull != "stream" {
		t.Fatalf("agent, env: %v %+v", err, a)
	}
	a, err = ParseAgent([]string{"-pull", "unary"}, envOf(e))
	if err != nil || a.Pull != "unary" {
		t.Fatalf("agent, flag over env: %v %+v", err, a)
	}
	he := hubEnv()
	he["PB_PULL"] = "unary"
	h, err := ParseHub(nil, envOf(he))
	if err != nil || h.Pull != "unary" {
		t.Fatalf("hub, env: %v %+v", err, h)
	}
}

func TestAWrongPullIsAnErrorAndNotASilentFallback(t *testing.T) {
	for _, bad := range []string{"push", "Unary", "streaming", "stream,unary"} {
		e := agentEnv(t)
		e["PB_PULL"] = bad
		if a, err := ParseAgent(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "pull") {
			t.Errorf("agent accepted PB_PULL=%q: %v %+v", bad, err, a)
		}
		he := hubEnv()
		he["PB_PULL"] = bad
		if _, err := ParseHub(nil, envOf(he)); err == nil || !strings.Contains(err.Error(), "pull") {
			t.Errorf("hub accepted PB_PULL=%q: %v", bad, err)
		}
	}
	// An empty value is the same as unset: the default of the program.
	e := agentEnv(t)
	e["PB_PULL"] = ""
	if a, err := ParseAgent(nil, envOf(e)); err != nil || a.Pull != "unary" {
		t.Errorf("an empty PB_PULL: %v %+v", err, a)
	}
}

const (
	testBotToken = "xoxb-1234567890-abcdefghij"
	testAppToken = "xapp-1-A0123456-7890123-abcdefghij"
)

func slackEnv() map[string]string {
	return map[string]string{"PB_PROJECT": "p", "PB_IDENTITY": "/x/hub.identity.json", "PB_AGENTS_DIR": "/x/agents", "PB_CHAT": "slack",
		"SLACK_BOT_TOKEN": testBotToken, "SLACK_APP_TOKEN": testAppToken, "PB_SLACK_USER_ID": "U0123ABCD"}
}

func TestTheChatIsTelegramUnlessSlackIsChosen(t *testing.T) {
	h, err := ParseHub(nil, envOf(hubEnv()))
	if err != nil || h.Chat != "telegram" {
		t.Fatalf("%v %+v", err, h)
	}
	h, err = ParseHub(nil, envOf(slackEnv()))
	if err != nil || h.Chat != "slack" {
		t.Fatalf("%v %+v", err, h)
	}
	if h.SlackBotToken != testBotToken || h.SlackAppToken != testAppToken || h.SlackUserID != "U0123ABCD" || h.SlackCommandPrefix != "pb-" ||
		h.SlackChannelID != "" || h.SlackAPIBase != "https://slack.com/api" || h.ChatID == 0 || h.UserID == 0 {
		t.Fatalf("%+v", h)
	}
	e := hubEnv()
	e["PB_CHAT"] = "irc"
	if _, err := ParseHub(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "chat:") {
		t.Fatalf("%v", err)
	}
}

func TestSlackNeedsNoTelegramSettingsAndTelegramNeedsNoSlackOnes(t *testing.T) {
	e := slackEnv() // no PB_CHAT_ID, no Telegram token
	if _, err := ParseHub(nil, envOf(e)); err != nil {
		t.Fatal(err)
	}
	e["PB_CHAT_ID"] = "4242" // left over from Telegram: harmless, and not taken for the Slack user
	h, err := ParseHub(nil, envOf(e))
	if err != nil || h.UserID == 4242 {
		t.Fatalf("%v %+v", err, h)
	}
	if _, err := ParseHub(nil, envOf(hubEnv())); err != nil { // no Slack settings
		t.Fatal(err)
	}
}

func TestSlackSettingsAreCheckedAndNeverEchoTheTokens(t *testing.T) {
	for name, mut := range map[string]func(map[string]string){
		"no bot token":        func(e map[string]string) { delete(e, "SLACK_BOT_TOKEN") },
		"no app token":        func(e map[string]string) { delete(e, "SLACK_APP_TOKEN") },
		"tokens swapped":      func(e map[string]string) { e["SLACK_BOT_TOKEN"], e["SLACK_APP_TOKEN"] = testAppToken, testBotToken },
		"a user token":        func(e map[string]string) { e["SLACK_BOT_TOKEN"] = "xoxp-1234567890-abcdefghij" },
		"no user":             func(e map[string]string) { delete(e, "PB_SLACK_USER_ID") },
		"a channel as user":   func(e map[string]string) { e["PB_SLACK_USER_ID"] = "C0123ABCD" },
		"a public channel":    func(e map[string]string) { e["PB_SLACK_CHANNEL_ID"] = "C0123ABCD" },
		"plain http api":      func(e map[string]string) { e["PB_SLACK_API_BASE"] = "http://slack.example.com/api" },
		"a bad prefix":        func(e map[string]string) { e["PB_SLACK_COMMAND_PREFIX"] = "No Spaces" },
		"an unknown emoji":    func(e map[string]string) { e["PB_DELIVERED_REACTION"] = "🦄" },
		"a Telegram-only one": func(e map[string]string) { e["PB_FAILED_REACTION"] = "🤡" },
	} {
		e := slackEnv()
		mut(e)
		_, err := ParseHub(nil, envOf(e))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, s := range []string{testBotToken, testAppToken, "xoxp-1234567890-abcdefghij"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: the error holds a token: %v", name, err)
			}
		}
	}
	_, stderr := capture(t)
	e := slackEnv()
	delete(e, "SLACK_BOT_TOKEN")
	for _, args := range [][]string{{"-slack-bot-token", testBotToken}, {"-slack-app-token=" + testAppToken}, {"-bot-token", testBotToken}} {
		if _, err := ParseHub(args, envOf(e)); err == nil {
			t.Errorf("a token flag %v was accepted", args)
		}
	}
	if strings.Contains(stderr.String(), testBotToken) || strings.Contains(stderr.String(), testAppToken) {
		t.Fatalf("the usage echoes a token:\n%s", stderr)
	}
}

func TestSlackTokensFromFilesNeedPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	bot, app := filepath.Join(dir, "bot"), filepath.Join(dir, "app")
	os.WriteFile(bot, []byte(testBotToken+"\n"), 0o644)
	os.WriteFile(app, []byte(testAppToken+"\n"), 0o600)
	e := slackEnv()
	delete(e, "SLACK_BOT_TOKEN")
	delete(e, "SLACK_APP_TOKEN")
	e["PB_SLACK_BOT_TOKEN_FILE"], e["PB_SLACK_APP_TOKEN_FILE"] = bot, app
	if _, err := ParseHub(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("a world-readable token file was accepted: %v", err)
	}
	os.Chmod(bot, 0o600)
	h, err := ParseHub(nil, envOf(e))
	if err != nil || h.SlackBotToken != testBotToken || h.SlackAppToken != testAppToken {
		t.Fatalf("%v %+v", err, h)
	}
	// a file that holds the wrong kind of token is refused without printing it
	os.WriteFile(bot, []byte(testAppToken), 0o600)
	if _, err := ParseHub(nil, envOf(e)); err == nil || strings.Contains(err.Error(), testAppToken) {
		t.Fatalf("%v", err)
	}
}

func TestSlackReactionsOfTheTable(t *testing.T) {
	e := slackEnv()
	e["PB_RECEIVED_REACTION"], e["PB_DELIVERED_REACTION"], e["PB_FAILED_REACTION"] = "👍", "✅", "off"
	if h, err := ParseHub(nil, envOf(e)); err != nil || h.ReceivedReaction != "👍" || h.FailedReaction != "" {
		t.Fatalf("%v %+v", err, h)
	}
}

// The chart refuses what the hub would refuse, so that a pod does not start only to stop at once: the patterns of the schema
// and the checks of the hub must agree.
func TestTheChartSchemaAgreesWithTheChecksOfTheHub(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/petrvs-bridge-hub/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Slack struct {
				Properties map[string]struct {
					Pattern string `json:"pattern"`
				} `json:"properties"`
			} `json:"slack"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for field, check := range map[string]func(string) bool{
		"userId": slackid.ValidUserID, "channelId": slackid.ValidChannelID, "commandPrefix": slackid.ValidCommandPrefix, "apiBase": slackid.ValidAPIBase,
	} {
		pattern := schema.Properties.Slack.Properties[field].Pattern
		if pattern == "" {
			t.Fatalf("no pattern for slack.%s", field)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("slack.%s: %v", field, err)
		}
		for _, in := range []string{"U0123ABCD", "W0123ABCD", "U12", "u0123abcd", "C0123ABCD", "D0123ABCD", "D0123ABCD ", "pb-", "pb", "Pb-", "a b", "x_y-z",
			"https://slack.com/api", "https://eu.slack.com/api", "https://evil.example.com/api", "https://slack.com.evil.example/api", "https://evilslack.com/api",
			"http://localhost:8080/api", "http://127.0.0.1:9/api", "http://localhost.evil.example/api", "http://localhostevil.com/api", "ftp://slack.com/api"} {
			if in == "" {
				continue
			}
			if got, want := re.MatchString(in), check(in); got != want {
				t.Errorf("slack.%s %q: the chart says %v, the hub says %v", field, in, got, want)
			}
		}
	}
}

func TestAnInvalidReactionIsReportedOnceForSlackToo(t *testing.T) {
	e := slackEnv()
	e["PB_DELIVERED_REACTION"] = "yes"
	_, err := ParseHub(nil, envOf(e))
	if err == nil || strings.Count(err.Error(), "PB_DELIVERED_REACTION") != 1 {
		t.Fatalf("%v", err)
	}
	e = slackEnv()
	e["PB_DELIVERED_REACTION"] = "🦄"
	e["PB_FAILED_REACTION"] = "🤡"
	var first string
	for i := 0; i < 5; i++ { // the order of the complaints and of the choices is the same every time
		_, err := ParseHub(nil, envOf(e))
		if err == nil {
			t.Fatal("accepted")
		}
		if i == 0 {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("the report changes from run to run:\n%s\n%s", first, err)
		}
	}
}

func TestTheCommandThatStartsTheAssistantHasOneSettingAndAnOldName(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"nothing given", nil, nil, "claude --continue"},
		{"the new variable", map[string]string{"PB_ASSISTANT_COMMAND": "opencode"}, nil, "opencode"},
		{"the old variable", map[string]string{"PB_CLAUDE_COMMAND": "claude -c"}, nil, "claude -c"},
		{"both variables: the new one", map[string]string{"PB_ASSISTANT_COMMAND": "opencode", "PB_CLAUDE_COMMAND": "claude -c"}, nil, "opencode"},
		{"the new flag", nil, []string{"-assistant-command", "opencode run"}, "opencode run"},
		{"the old flag", nil, []string{"-claude-command", "claude x"}, "claude x"},
		{"a flag beats the environment", map[string]string{"PB_ASSISTANT_COMMAND": "opencode"}, []string{"-claude-command", "claude x"}, "claude x"},
		{"the new flag beats the old one", nil, []string{"-claude-command", "claude x", "-assistant-command", "opencode"}, "opencode"},
		{"a flag beats both variables", map[string]string{"PB_ASSISTANT_COMMAND": "a", "PB_CLAUDE_COMMAND": "b"}, []string{"-assistant-command", "opencode"}, "opencode"},
		{"a command with a path and options", map[string]string{"PB_ASSISTANT_COMMAND": "/opt/homebrew/bin/opencode --port 4096"}, nil, "/opt/homebrew/bin/opencode --port 4096"},
	}
	for _, c := range cases {
		e := agentEnv(t)
		for k, v := range c.env {
			e[k] = v
		}
		a, err := ParseAgent(c.args, envOf(e))
		if err != nil || a.AssistantCommand != c.want {
			t.Errorf("%s: %q (%v), want %q", c.name, a.AssistantCommand, err, c.want)
		}
	}
	// a command must be one line, whichever name it comes by
	for _, env := range []map[string]string{{"PB_ASSISTANT_COMMAND": "opencode\nrm -rf /"}, {"PB_CLAUDE_COMMAND": "claude\r\nx"}} {
		e := agentEnv(t)
		for k, v := range env {
			e[k] = v
		}
		if _, err := ParseAgent(nil, envOf(e)); err == nil || !strings.Contains(err.Error(), "assistant-command") {
			t.Errorf("%v: %v", env, err)
		}
	}
	// the name of the assistant, as the chat says it
	e := agentEnv(t)
	if a, _ := ParseAgent(nil, envOf(e)); a.AssistantName != "Claude Code" {
		t.Errorf("default name %q", a.AssistantName)
	}
	e["PB_ASSISTANT_NAME"] = "opencode"
	if a, _ := ParseAgent(nil, envOf(e)); a.AssistantName != "opencode" {
		t.Errorf("name %q", a.AssistantName)
	}
}
