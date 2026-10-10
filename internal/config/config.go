// Package config reads the settings of the hub and the agent from flags and
// environment variables. Secrets (the bot token, the alarm code secret) are
// read from the environment or a file, never from a command-line flag, so they
// do not end up in the shell history or in the process list.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/slack"
)

// Common settings shared by hub and agent.
type Common struct {
	Project         string // Pub/Sub project id (ignored by the emulator)
	Prefix          string // topic and subscription name prefix
	Identity        string // path of this party's private identity file
	LogLevel        string // debug, info, warn, error
	CreateResources bool   // create topics and subscriptions (development and the emulator only)
	// AllowGroupRead lets private files (identity, secrets) be readable by their group, for example a Kubernetes
	// secret volume with fsGroup. Other users still may not read them.
	AllowGroupRead bool
	// Pull is how the subscription is read: "stream" (one long streaming call, the default of the hub) or "unary" (short calls,
	// each with a deadline; the default of the agent). A tunnel or a TLS-inspecting proxy between a laptop and Google can hold
	// a long stream back and let it out in lumps; short calls pass it.
	Pull string
}

// Hub holds the hub's settings.
type Hub struct {
	Common
	AgentsDir    string // directory of *.pub.json files: the agents that may connect
	Token        string // bot token
	TelegramBase string // Bot API address; the default is Telegram's, change it for a local Bot API server or a test double
	// Chat is the chat service the hub talks to: "telegram" (the default) or "slack".
	Chat string
	// The Slack settings, used when Chat is "slack". The tokens are secrets: they come from the environment or a private file, never a flag.
	SlackBotToken, SlackAppToken string
	SlackUserID                  string // the only Slack user that is obeyed
	SlackChannelID               string // the direct message with the bot; empty means the hub asks Slack for it (needs the im:write right)
	SlackAPIBase                 string // the Web API address; the default is Slack's, change it for a test double
	SlackCommandPrefix           string // put before the name of the app's slash commands (default "pb-")
	ChatID                       int64
	UserID                       int64
	OfflineAfter                 time.Duration
	TextTTL                      time.Duration
	KeyTTL                       time.Duration
	PollTimeout                  time.Duration
	MaxReplyBytes                int
	LockFile                     string // keeps a lockdown across restarts of the hub; empty means it does not
	// DefaultAgent is where plain text goes until the user picks an agent. Empty means: ask first (with one agent, that
	// agent is used).
	DefaultAgent string
	// ReceivedReaction is the emoji put on each message of the user once the hub has read it, before any agent has seen it;
	// empty means no mark, which is the default: the mark that matters is the agent's.
	ReceivedReaction string
	// DeliveredReaction is put on the message when the agent has got the command and typed it into the session; empty means
	// no mark from the agent.
	DeliveredReaction string
	// FailedReaction is put on the message when the command did not get through (the hub could not send it, or the agent
	// could not carry it out); empty means no mark.
	FailedReaction string
	// MaxFileBytes bounds a file in either direction; 0 was replaced by the default (20 MiB).
	MaxFileBytes int64
	// HealthDir, if set, is where the hub records that it is alive and ready, for `hub probe` to read (an absolute path; in a
	// container, a small in-memory volume). Empty means the hub records nothing.
	HealthDir string
	// ShutdownTimeout is how long the hub waits, once told to stop, for the work in flight (default 20s).
	ShutdownTimeout time.Duration
}

// Agent holds the agent's settings.
type Agent struct {
	Common
	Name           string
	HubPub         string // path of the hub's public bundle
	TmuxSession    string
	StateDir       string
	SpoolDir       string
	LockFile       string
	AlarmEnabled   bool
	AlarmState     string
	AlarmSecret    []byte // raw TOTP secret; nil means disarming needs no code
	AlarmCooldown  time.Duration
	AlarmPoll      time.Duration
	Heartbeat      time.Duration
	Handshake      time.Duration
	Silence        time.Duration
	ReceiptTimeout time.Duration
	ReorderWindow  time.Duration
	AlarmCodeState string // where used one-time codes are remembered across restarts

	// Workdir is the directory Claude Code runs in; empty when not set.
	Workdir string
	// InboxDir is where files from the chat are saved.
	InboxDir string
	// MaxFileBytes bounds a file in either direction; 0 was replaced by the default (20 MiB).
	MaxFileBytes int64
	// Heal turns on starting Claude Code again when it has stopped (needs Workdir); ClaudeCommand is what is run,
	// HealGrace how long the pane must be without it first, HealTries the starts allowed within HealWindow.
	Heal          bool
	ClaudeCommand string
	HealGrace     time.Duration
	HealTries     int
	HealWindow    time.Duration
	// StuckAfter is how long a request may go without a reply before the chat is told; 0 turns the check off.
	StuckAfter time.Duration
}

// MaxFileLimit is the most a file may be allowed to weigh: what a bot can upload to Telegram.
const MaxFileLimit = 50 << 20

// DefaultMaxFileBytes applies when no limit is set.
const DefaultMaxFileBytes = 20 << 20

// parseOffDuration reads a duration that may also be "0" or "off", meaning none.
func parseOffDuration(e *env, key, v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "0" || strings.EqualFold(v, "off") {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		e.err = append(e.err, key+": "+strconv.Quote(v)+" is not a positive duration (for example 10m), 0 or off")
		return 0
	}
	return d
}

// checkFileLimit returns the limit to use and reports a value that is negative or above what Telegram takes.
func checkFileLimit(e *env, n int64) int64 {
	switch {
	case n < 0 || n > MaxFileLimit:
		e.err = append(e.err, fmt.Sprintf("max-file-bytes: 0 (the default, %d) to %d", int64(DefaultMaxFileBytes), int64(MaxFileLimit)))
		return DefaultMaxFileBytes
	case n == 0:
		return DefaultMaxFileBytes
	}
	return n
}

type env struct {
	get func(string) string
	err []string
}

func (e *env) str(key, def string) string {
	if v := strings.TrimSpace(e.get(key)); v != "" {
		return v
	}
	return def
}

func (e *env) dur(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(e.get(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		e.err = append(e.err, fmt.Sprintf("%s: %q is not a positive duration (for example 30s)", key, v))
		return def
	}
	return d
}

func (e *env) int64(key string) int64 {
	v := strings.TrimSpace(e.get(key))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		e.err = append(e.err, fmt.Sprintf("%s: %q is not a number", key, v))
	}
	return n
}

func (e *env) boolean(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(e.get(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	e.err = append(e.err, fmt.Sprintf("%s: use true or false", key))
	return def
}

func readSecretFile(path string, allowGroupRead bool) (string, error) {
	if err := identity.CheckPrivateMode(path, allowGroupRead); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

func stateDir(e *env) string {
	if d := e.str("PB_STATE_DIR", ""); d != "" {
		return d
	}
	if d := e.get("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "petrvs-bridge")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".petrvs-bridge"
	}
	return filepath.Join(home, ".local", "state", "petrvs-bridge")
}

// common declares the settings of both programs. pull is the default way to read the subscription of the program: the hub runs
// in a data centre and keeps the stream, the agent runs on a laptop behind whatever network it is on and asks with short calls.
func common(fs *flag.FlagSet, e *env, c *Common, pull string) {
	fs.StringVar(&c.Project, "project", e.str("PB_PROJECT", e.str("GOOGLE_CLOUD_PROJECT", "")), "Pub/Sub project id (env PB_PROJECT)")
	fs.StringVar(&c.Prefix, "prefix", e.str("PB_PREFIX", "pb"), "topic/subscription name prefix (env PB_PREFIX)")
	fs.StringVar(&c.Identity, "identity", e.str("PB_IDENTITY", ""), "path of the private identity file (env PB_IDENTITY)")
	fs.StringVar(&c.LogLevel, "log-level", e.str("PB_LOG_LEVEL", "info"), "debug, info, warn or error (env PB_LOG_LEVEL)")
	fs.BoolVar(&c.CreateResources, "create-resources", e.boolean("PB_CREATE_RESOURCES", false), "create topics and subscriptions; development and the emulator only (env PB_CREATE_RESOURCES)")
	fs.StringVar(&c.Pull, "pull", e.str("PB_PULL", pull), "how the subscription is read: unary (short calls; the default of the agent) or stream (one long call; the default of the hub) (env PB_PULL)")
	fs.BoolVar(&c.AllowGroupRead, "allow-group-read", e.boolean("PB_ALLOW_GROUP_READ", false), "accept private files that their group may read, for a Kubernetes secret volume with fsGroup (env PB_ALLOW_GROUP_READ)")
}

// checkReaction returns the emoji to use, or "" when marking is switched off. Telegram accepts only a fixed set of emoji
// (the hub says so in its log if it refuses one), so this only catches what is clearly not an emoji: a plain word such as
// "on" or "yes" is a typo, not a reaction.
func checkReaction(e *env, key, v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "off") {
		return ""
	}
	ok := v != "" && len(v) <= 16 && utf8.ValidString(v)
	for _, r := range v {
		if r < 0x80 {
			ok = false
		}
	}
	if !ok {
		e.err = append(e.err, fmt.Sprintf("%s: %q is not an emoji; use one such as 👀 or 👍, or off", key, v))
	}
	return v
}

// checkTelegramBase refuses a Bot API address that would send the bot token over plain HTTP to another machine.
func checkTelegramBase(e *env, base string) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		e.err = append(e.err, "telegram-api: not a valid URL")
		return
	}
	switch u.Scheme {
	case "https":
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			e.err = append(e.err, "telegram-api: plain http is accepted only for localhost; the bot token would cross the network in the clear")
		}
	default:
		e.err = append(e.err, "telegram-api: use an https URL")
	}
}

func (c *Common) validate(e *env, needProject bool) {
	if c.Identity == "" {
		e.err = append(e.err, "identity: set PB_IDENTITY or -identity")
	}
	if c.Project == "" {
		if needProject {
			e.err = append(e.err, "project: set PB_PROJECT or -project (not needed with the emulator)")
		} else {
			// The emulator accepts any project, but the Pub/Sub client refuses an empty one.
			c.Project = emulatorProject
		}
	}
	if !identity.ValidName(c.Prefix) {
		e.err = append(e.err, "prefix: lower-case letters, digits, - and _")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		e.err = append(e.err, "log-level: debug, info, warn or error")
	}
	switch c.Pull {
	case "stream", "unary":
	default:
		e.err = append(e.err, "pull: stream or unary")
	}
}

func finish(e *env) error {
	if len(e.err) == 0 {
		return nil
	}
	return errors.New("configuration problems:\n  - " + strings.Join(e.err, "\n  - "))
}

func emulator(e *env) bool { return e.get("PUBSUB_EMULATOR_HOST") != "" }

// emulatorProject is the project id used with the emulator when none is set. Hub and agent must use the same one, as
// the emulator keeps its topics per project.
const emulatorProject = "emulator"

// Where the usage goes. Tests replace them.
var usageOut, usageErr io.Writer = os.Stdout, os.Stderr

// parseFlags parses the command line. The flag package's own messages are discarded and replaced by these: a request for
// help (-h, --help) prints the usage with every flag's default on standard output and comes back as an error that
// satisfies errors.Is(err, flag.ErrHelp), which the program should answer with exit status 0; any other mistake prints
// the usage on standard error. The error names the program; the caller prints it.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	if err == nil {
		return nil
	}
	out := usageErr
	if errors.Is(err, flag.ErrHelp) {
		out = usageOut
	}
	fs.SetOutput(out)
	fmt.Fprintf(out, "Usage of %s (every flag can also be set by the environment variable named in its description):\n", fs.Name())
	fs.PrintDefaults()
	return fmt.Errorf("%s: %w", fs.Name(), err)
}

// ParseHub reads the hub's settings.
func ParseHub(args []string, getenv func(string) string) (*Hub, error) {
	e := &env{get: getenv}
	h := &Hub{}
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	common(fs, e, &h.Common, "stream")
	fs.StringVar(&h.AgentsDir, "agents-dir", e.str("PB_AGENTS_DIR", ""), "directory holding one *.pub.json per agent that may connect (env PB_AGENTS_DIR)")
	fs.StringVar(&h.Chat, "chat", e.str("PB_CHAT", "telegram"), "the chat service: telegram or slack (env PB_CHAT)")
	fs.StringVar(&h.SlackUserID, "slack-user", e.str("PB_SLACK_USER_ID", ""), "Slack: the only user that is obeyed, like U0123ABCD (env PB_SLACK_USER_ID)")
	fs.StringVar(&h.SlackChannelID, "slack-channel", e.str("PB_SLACK_CHANNEL_ID", ""), "Slack: the direct message of that user with the app, like D0123ABCD; unset means the hub asks Slack, which needs the im:write right (env PB_SLACK_CHANNEL_ID)")
	fs.StringVar(&h.SlackAPIBase, "slack-api", e.str("PB_SLACK_API_BASE", slack.DefaultAPIBase), "Slack: the Web API address; change it only for a test double (env PB_SLACK_API_BASE)")
	fs.StringVar(&h.SlackCommandPrefix, "slack-command-prefix", e.str("PB_SLACK_COMMAND_PREFIX", "pb-"), "Slack: the prefix of the app's slash commands, so that /pb-status reaches the hub as /status (env PB_SLACK_COMMAND_PREFIX)")
	fs.StringVar(&h.TelegramBase, "telegram-api", e.str("PB_TELEGRAM_API_BASE", "https://api.telegram.org"), "Bot API base URL (env PB_TELEGRAM_API_BASE)")
	fs.Int64Var(&h.ChatID, "chat-id", e.int64("PB_CHAT_ID"), "the only chat that is obeyed (env PB_CHAT_ID)")
	fs.Int64Var(&h.UserID, "user-id", e.int64("PB_USER_ID"), "the only user that is obeyed; defaults to the chat id (env PB_USER_ID)")
	fs.DurationVar(&h.OfflineAfter, "offline-after", e.dur("PB_OFFLINE_AFTER", 3*time.Minute), "silence after which an agent is called offline (env PB_OFFLINE_AFTER)")
	fs.DurationVar(&h.TextTTL, "text-ttl", e.dur("PB_TEXT_TTL", 10*time.Minute), "how long a text command stays valid (env PB_TEXT_TTL)")
	fs.DurationVar(&h.KeyTTL, "key-ttl", e.dur("PB_KEY_TTL", time.Minute), "how long a key press or alarm command stays valid (env PB_KEY_TTL)")
	fs.DurationVar(&h.PollTimeout, "poll-timeout", e.dur("PB_POLL_TIMEOUT", 25*time.Second), "Telegram long-poll timeout (env PB_POLL_TIMEOUT)")
	fs.IntVar(&h.MaxReplyBytes, "max-reply-bytes", int(e.int64("PB_MAX_REPLY_BYTES")), "longest reply shown in the chat; 0 means 20000 (env PB_MAX_REPLY_BYTES)")
	fs.Int64Var(&h.MaxFileBytes, "max-file-bytes", e.int64("PB_MAX_FILE_BYTES"), "largest file in either direction; 0 means 20 MiB, at most 50 MiB (env PB_MAX_FILE_BYTES)")
	fs.StringVar(&h.HealthDir, "health-dir", e.str("PB_HEALTH_DIR", ""), "directory where the hub records that it is alive and ready, for `hub probe live` and `hub probe ready` to read; empty means it records nothing (env PB_HEALTH_DIR)")
	fs.DurationVar(&h.ShutdownTimeout, "shutdown-timeout", e.dur("PB_SHUTDOWN_TIMEOUT", 20*time.Second), "how long the hub waits for the work in flight after it was told to stop; keep it shorter than the grace period of the container (env PB_SHUTDOWN_TIMEOUT)")
	fs.StringVar(&h.LockFile, "lock-file", e.str("PB_LOCK_FILE", ""), "file that keeps a lockdown across restarts of the hub; empty means a restart ends it (env PB_LOCK_FILE)")
	fs.StringVar(&h.DefaultAgent, "default-agent", e.str("PB_DEFAULT_AGENT", ""), "agent that gets plain text until you choose one; empty means the hub asks first when it has several (env PB_DEFAULT_AGENT)")
	reaction := fs.String("received-reaction", e.str("PB_RECEIVED_REACTION", "off"), "emoji put on each message of yours once the hub has read it, before the agent has, or off (env PB_RECEIVED_REACTION)")
	delivered := fs.String("delivered-reaction", e.str("PB_DELIVERED_REACTION", "👀"), "emoji the agent causes to be put on your message once it has got it and typed it into the session, or off (env PB_DELIVERED_REACTION)")
	failed := fs.String("failed-reaction", e.str("PB_FAILED_REACTION", "👎"), "emoji put on your message when it did not get through (agent offline, bus refused, agent could not type it), or off (env PB_FAILED_REACTION)")
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	h.MaxFileBytes = checkFileLimit(e, h.MaxFileBytes)
	h.ReceivedReaction = checkReaction(e, "PB_RECEIVED_REACTION", *reaction)
	h.DeliveredReaction = checkReaction(e, "PB_DELIVERED_REACTION", *delivered)
	h.FailedReaction = checkReaction(e, "PB_FAILED_REACTION", *failed)
	if h.Chat == "slack" {
		for key, v := range map[string]string{"PB_RECEIVED_REACTION": h.ReceivedReaction, "PB_DELIVERED_REACTION": h.DeliveredReaction, "PB_FAILED_REACTION": h.FailedReaction} {
			if _, ok := slack.ReactionName(v); v != "" && !ok {
				e.err = append(e.err, fmt.Sprintf("%s: %q has no name in Slack; use one of %s, or off", key, v, strings.Join(slack.Reactions(), " ")))
			}
		}
	}
	if h.DefaultAgent != "" && !identity.ValidName(h.DefaultAgent) {
		e.err = append(e.err, "default-agent: lower-case letters, digits, - and _")
	}
	if h.HealthDir != "" && !filepath.IsAbs(h.HealthDir) {
		e.err = append(e.err, "health-dir: an absolute path")
	}
	if h.ShutdownTimeout <= 0 || h.ShutdownTimeout > 5*time.Minute {
		e.err = append(e.err, "shutdown-timeout: more than 0 and at most 5m")
	}
	h.validate(e, !emulator(e))
	if h.AgentsDir == "" {
		e.err = append(e.err, "agents-dir: set PB_AGENTS_DIR or -agents-dir")
	}
	switch h.Chat {
	case "telegram":
		h.parseTelegram(e, getenv)
	case "slack":
		h.parseSlack(e, getenv)
	default:
		e.err = append(e.err, "chat: telegram or slack")
	}
	return h, finish(e)
}

// parseTelegram checks what the hub needs to talk to Telegram.
func (h *Hub) parseTelegram(e *env, getenv func(string) string) {
	if h.UserID == 0 {
		h.UserID = h.ChatID
	}
	checkTelegramBase(e, h.TelegramBase)
	if h.ChatID == 0 {
		e.err = append(e.err, "chat-id: set PB_CHAT_ID or -chat-id")
	}
	switch {
	case getenv("TELEGRAM_BOT_TOKEN") != "":
		h.Token = strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN"))
	case getenv("PB_TELEGRAM_TOKEN_FILE") != "":
		tok, err := readSecretFile(getenv("PB_TELEGRAM_TOKEN_FILE"), h.AllowGroupRead)
		if err != nil {
			e.err = append(e.err, "PB_TELEGRAM_TOKEN_FILE: "+err.Error())
		}
		h.Token = tok
	default:
		e.err = append(e.err, "bot token: set TELEGRAM_BOT_TOKEN or PB_TELEGRAM_TOKEN_FILE (it is never accepted as a flag)")
	}
}

// parseSlack checks what the hub needs to talk to Slack: two tokens (from the environment or from private files, never a flag),
// the one user that is obeyed and, optionally, the direct message with the app.
func (h *Hub) parseSlack(e *env, getenv func(string) string) {
	h.ChatID, h.UserID = slack.ChatID, slack.UserID // the client keeps the one chat and the one user under these numbers
	if !slack.ValidUserID(h.SlackUserID) {
		e.err = append(e.err, "slack-user: set PB_SLACK_USER_ID to the id of the one user that is obeyed, like U0123ABCD")
	}
	if h.SlackChannelID != "" && !slack.ValidChannelID(h.SlackChannelID) {
		e.err = append(e.err, "slack-channel: the id of the direct message, like D0123ABCD")
	}
	h.SlackBotToken = slackToken(e, getenv, "SLACK_BOT_TOKEN", "PB_SLACK_BOT_TOKEN_FILE", h.AllowGroupRead, slack.ValidBotToken, "xoxb-")
	h.SlackAppToken = slackToken(e, getenv, "SLACK_APP_TOKEN", "PB_SLACK_APP_TOKEN_FILE", h.AllowGroupRead, slack.ValidAppToken, "xapp-")
	if !slack.ValidAPIBase(h.SlackAPIBase) {
		e.err = append(e.err, "slack-api: an https URL (plain http only for localhost)")
	}
	if !slack.ValidCommandPrefix(h.SlackCommandPrefix) {
		e.err = append(e.err, "slack-command-prefix: lower-case letters, digits, - and _, at most 10 characters")
	}
}

// slackToken reads a Slack token from the environment or a private file and checks its shape. The token never appears in the
// error, only what is wrong with where it came from.
func slackToken(e *env, getenv func(string) string, envKey, fileKey string, groupRead bool, valid func(string) bool, prefix string) string {
	var tok string
	switch {
	case getenv(envKey) != "":
		tok = strings.TrimSpace(getenv(envKey))
	case getenv(fileKey) != "":
		t, err := readSecretFile(getenv(fileKey), groupRead)
		if err != nil {
			e.err = append(e.err, fileKey+": "+err.Error())
			return ""
		}
		tok = t
	default:
		e.err = append(e.err, fmt.Sprintf("slack token: set %s or %s (it is never accepted as a flag)", envKey, fileKey))
		return ""
	}
	if !valid(tok) {
		e.err = append(e.err, fmt.Sprintf("%s: not a Slack token of the right kind (it must start with %s)", envKey, prefix))
		return ""
	}
	return tok
}

// ParseAgent reads the agent's settings.
func ParseAgent(args []string, getenv func(string) string) (*Agent, error) {
	e := &env{get: getenv}
	a := &Agent{}
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	common(fs, e, &a.Common, "unary")
	sd := stateDir(e)
	fs.StringVar(&a.Name, "name", e.str("PB_NAME", ""), "this agent's name; must match its identity (env PB_NAME)")
	fs.StringVar(&a.HubPub, "hub-pub", e.str("PB_HUB_PUB", ""), "path of the hub's public bundle, pinned at enrolment (env PB_HUB_PUB)")
	fs.StringVar(&a.TmuxSession, "tmux-session", e.str("PB_TMUX_SESSION", "claude"), "tmux session that receives the text (env PB_TMUX_SESSION)")
	fs.StringVar(&a.StateDir, "state-dir", sd, "directory for the spool, lock and alarm state (env PB_STATE_DIR)")
	fs.BoolVar(&a.AlarmEnabled, "alarm", e.boolean("PB_ALARM", true), "offer the alarm mode (env PB_ALARM)")
	fs.DurationVar(&a.AlarmCooldown, "alarm-cooldown", e.dur("PB_ALARM_COOLDOWN", time.Minute), "at most one report per sensor in this time (env PB_ALARM_COOLDOWN)")
	fs.DurationVar(&a.AlarmPoll, "alarm-poll", e.dur("PB_ALARM_POLL", time.Second), "how often the sensors are read (env PB_ALARM_POLL)")
	fs.DurationVar(&a.Heartbeat, "heartbeat", e.dur("PB_HEARTBEAT", 30*time.Second), "heartbeat interval (env PB_HEARTBEAT)")
	fs.DurationVar(&a.Handshake, "handshake-retry", e.dur("PB_HANDSHAKE_RETRY", 10*time.Second), "how often a hello is repeated until answered (env PB_HANDSHAKE_RETRY)")
	fs.DurationVar(&a.Silence, "silence-limit", e.dur("PB_SILENCE_LIMIT", 0), "no answer from the hub for this long means a new handshake; 0 means 3 x heartbeat (env PB_SILENCE_LIMIT)")
	fs.DurationVar(&a.ReorderWindow, "reorder-window", e.dur("PB_REORDER_WINDOW", 2*time.Second), "how long a command waits for an earlier one that has not arrived before that one is reported lost (env PB_REORDER_WINDOW)")
	fs.DurationVar(&a.ReceiptTimeout, "receipt-timeout", e.dur("PB_RECEIPT_TIMEOUT", 30*time.Second), "a spooled message is sent again if not confirmed in this time (env PB_RECEIPT_TIMEOUT)")
	fs.StringVar(&a.Workdir, "workdir", e.str("PB_WORKDIR", ""), "directory Claude Code runs in; auto-heal starts it there (env PB_WORKDIR)")
	fs.StringVar(&a.InboxDir, "inbox-dir", e.str("PB_INBOX_DIR", ""), "where files from the chat are saved; default: .petrvs-inbox in the workdir, or inbox in the state directory (env PB_INBOX_DIR)")
	fs.Int64Var(&a.MaxFileBytes, "max-file-bytes", e.int64("PB_MAX_FILE_BYTES"), "largest file in either direction; 0 means 20 MiB, at most 50 MiB (env PB_MAX_FILE_BYTES)")
	fs.BoolVar(&a.Heal, "heal", e.boolean("PB_HEAL", false), "start Claude Code again when it has stopped or its tmux session is gone; needs the workdir (env PB_HEAL)")
	fs.StringVar(&a.ClaudeCommand, "claude-command", e.str("PB_CLAUDE_COMMAND", "claude --continue"), "what auto-heal and /continue run to start Claude Code (env PB_CLAUDE_COMMAND)")
	fs.DurationVar(&a.HealGrace, "heal-grace", e.dur("PB_HEAL_GRACE", 20*time.Second), "Claude Code must be gone this long before auto-heal acts (env PB_HEAL_GRACE)")
	healTries := fs.Int("heal-tries", int(e.int64("PB_HEAL_TRIES")), "starts allowed within the heal window before auto-heal gives up; 0 means 3 (env PB_HEAL_TRIES)")
	fs.DurationVar(&a.HealWindow, "heal-window", e.dur("PB_HEAL_WINDOW", 10*time.Minute), "the window the heal tries are counted in (env PB_HEAL_WINDOW)")
	stuck := fs.String("stuck-after", e.str("PB_STUCK_AFTER", "10m"), "tell the chat when a request has had no reply for this long; 0 or off turns it off (env PB_STUCK_AFTER)")
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	a.validate(e, !emulator(e))
	if !identity.ValidName(a.Name) {
		e.err = append(e.err, "name: set PB_NAME or -name (lower-case letters, digits, - and _)")
	}
	if a.HubPub == "" {
		e.err = append(e.err, "hub-pub: set PB_HUB_PUB or -hub-pub")
	}
	a.MaxFileBytes = checkFileLimit(e, a.MaxFileBytes)
	a.StuckAfter = parseOffDuration(e, "PB_STUCK_AFTER", *stuck)
	a.HealTries = *healTries
	if a.HealTries == 0 {
		a.HealTries = 3
	}
	if a.HealTries < 0 || a.HealGrace < 0 || a.HealWindow <= 0 {
		e.err = append(e.err, "heal-tries, heal-grace, heal-window: must not be negative, and the window must be positive")
	}
	if a.Workdir != "" && !filepath.IsAbs(a.Workdir) {
		e.err = append(e.err, "workdir: an absolute path")
	}
	if a.InboxDir != "" && !filepath.IsAbs(a.InboxDir) {
		e.err = append(e.err, "inbox-dir: an absolute path")
	}
	if a.InboxDir == "" {
		if a.Workdir != "" {
			a.InboxDir = filepath.Join(a.Workdir, ".petrvs-inbox")
		} else {
			a.InboxDir = filepath.Join(a.StateDir, "inbox")
		}
	}
	if a.Heal && a.Workdir == "" {
		e.err = append(e.err, "heal: PB_HEAL needs PB_WORKDIR, the directory to start Claude Code in")
	}
	if strings.TrimSpace(a.ClaudeCommand) == "" || strings.ContainsAny(a.ClaudeCommand, "\x00\n\r") {
		e.err = append(e.err, "claude-command: one line, not empty")
	}
	a.SpoolDir = filepath.Join(a.StateDir, "spool")
	a.LockFile = filepath.Join(a.StateDir, "locked")
	a.AlarmState = filepath.Join(a.StateDir, "alarm-armed")
	a.AlarmCodeState = filepath.Join(a.StateDir, "alarm-code-state")
	if f := getenv("PB_ALARM_SECRET_FILE"); f != "" {
		b32, err := readSecretFile(f, a.AllowGroupRead)
		if err != nil {
			e.err = append(e.err, "PB_ALARM_SECRET_FILE: "+err.Error())
		} else if secret, err := decodeSecret(b32); err != nil {
			e.err = append(e.err, "PB_ALARM_SECRET_FILE: "+err.Error())
		} else {
			a.AlarmSecret = secret
		}
	}
	return a, finish(e)
}
