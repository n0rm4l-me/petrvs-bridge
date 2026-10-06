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

	"claudebridge/internal/identity"
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
}

// Hub holds the hub's settings.
type Hub struct {
	Common
	AgentsDir     string // directory of *.pub.json files: the agents that may connect
	Token         string // bot token
	TelegramBase  string // Bot API address; the default is Telegram's, change it for a local Bot API server or a test double
	ChatID        int64
	UserID        int64
	OfflineAfter  time.Duration
	TextTTL       time.Duration
	KeyTTL        time.Duration
	PollTimeout   time.Duration
	MaxReplyBytes int
	LockFile      string // keeps a lockdown across restarts of the hub; empty means it does not
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
	if d := e.str("CB_STATE_DIR", ""); d != "" {
		return d
	}
	if d := e.get("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "claude-bridge")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude-bridge"
	}
	return filepath.Join(home, ".local", "state", "claude-bridge")
}

func common(fs *flag.FlagSet, e *env, c *Common) {
	fs.StringVar(&c.Project, "project", e.str("CB_PROJECT", e.str("GOOGLE_CLOUD_PROJECT", "")), "Pub/Sub project id (env CB_PROJECT)")
	fs.StringVar(&c.Prefix, "prefix", e.str("CB_PREFIX", "cb"), "topic/subscription name prefix (env CB_PREFIX)")
	fs.StringVar(&c.Identity, "identity", e.str("CB_IDENTITY", ""), "path of the private identity file (env CB_IDENTITY)")
	fs.StringVar(&c.LogLevel, "log-level", e.str("CB_LOG_LEVEL", "info"), "debug, info, warn or error (env CB_LOG_LEVEL)")
	fs.BoolVar(&c.CreateResources, "create-resources", e.boolean("CB_CREATE_RESOURCES", false), "create topics and subscriptions; development and the emulator only (env CB_CREATE_RESOURCES)")
	fs.BoolVar(&c.AllowGroupRead, "allow-group-read", e.boolean("CB_ALLOW_GROUP_READ", false), "accept private files that their group may read, for a Kubernetes secret volume with fsGroup (env CB_ALLOW_GROUP_READ)")
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
		e.err = append(e.err, "identity: set CB_IDENTITY or -identity")
	}
	if needProject && c.Project == "" {
		e.err = append(e.err, "project: set CB_PROJECT or -project (not needed with the emulator)")
	}
	if !identity.ValidName(c.Prefix) {
		e.err = append(e.err, "prefix: lower-case letters, digits, - and _")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		e.err = append(e.err, "log-level: debug, info, warn or error")
	}
}

func finish(e *env) error {
	if len(e.err) == 0 {
		return nil
	}
	return errors.New("configuration problems:\n  - " + strings.Join(e.err, "\n  - "))
}

func emulator(e *env) bool { return e.get("PUBSUB_EMULATOR_HOST") != "" }

// ParseHub reads the hub's settings.
func ParseHub(args []string, getenv func(string) string) (*Hub, error) {
	e := &env{get: getenv}
	h := &Hub{}
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	common(fs, e, &h.Common)
	fs.StringVar(&h.AgentsDir, "agents-dir", e.str("CB_AGENTS_DIR", ""), "directory holding one *.pub.json per agent that may connect (env CB_AGENTS_DIR)")
	fs.StringVar(&h.TelegramBase, "telegram-api", e.str("CB_TELEGRAM_API_BASE", "https://api.telegram.org"), "Bot API base URL (env CB_TELEGRAM_API_BASE)")
	fs.Int64Var(&h.ChatID, "chat-id", e.int64("CB_CHAT_ID"), "the only chat that is obeyed (env CB_CHAT_ID)")
	fs.Int64Var(&h.UserID, "user-id", e.int64("CB_USER_ID"), "the only user that is obeyed; defaults to the chat id (env CB_USER_ID)")
	fs.DurationVar(&h.OfflineAfter, "offline-after", e.dur("CB_OFFLINE_AFTER", 3*time.Minute), "silence after which an agent is called offline (env CB_OFFLINE_AFTER)")
	fs.DurationVar(&h.TextTTL, "text-ttl", e.dur("CB_TEXT_TTL", 10*time.Minute), "how long a text command stays valid (env CB_TEXT_TTL)")
	fs.DurationVar(&h.KeyTTL, "key-ttl", e.dur("CB_KEY_TTL", time.Minute), "how long a key press or alarm command stays valid (env CB_KEY_TTL)")
	fs.DurationVar(&h.PollTimeout, "poll-timeout", e.dur("CB_POLL_TIMEOUT", 25*time.Second), "Telegram long-poll timeout (env CB_POLL_TIMEOUT)")
	fs.IntVar(&h.MaxReplyBytes, "max-reply-bytes", int(e.int64("CB_MAX_REPLY_BYTES")), "longest reply shown in the chat; 0 means 20000 (env CB_MAX_REPLY_BYTES)")
	fs.StringVar(&h.LockFile, "lock-file", e.str("CB_LOCK_FILE", ""), "file that keeps a lockdown across restarts of the hub; empty means a restart ends it (env CB_LOCK_FILE)")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("hub: %w", err)
	}
	if h.UserID == 0 {
		h.UserID = h.ChatID
	}
	h.validate(e, !emulator(e))
	checkTelegramBase(e, h.TelegramBase)
	if h.AgentsDir == "" {
		e.err = append(e.err, "agents-dir: set CB_AGENTS_DIR or -agents-dir")
	}
	if h.ChatID == 0 {
		e.err = append(e.err, "chat-id: set CB_CHAT_ID or -chat-id")
	}
	switch {
	case getenv("TELEGRAM_BOT_TOKEN") != "":
		h.Token = strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN"))
	case getenv("CB_TELEGRAM_TOKEN_FILE") != "":
		tok, err := readSecretFile(getenv("CB_TELEGRAM_TOKEN_FILE"), h.AllowGroupRead)
		if err != nil {
			e.err = append(e.err, "CB_TELEGRAM_TOKEN_FILE: "+err.Error())
		}
		h.Token = tok
	default:
		e.err = append(e.err, "bot token: set TELEGRAM_BOT_TOKEN or CB_TELEGRAM_TOKEN_FILE (it is never accepted as a flag)")
	}
	return h, finish(e)
}

// ParseAgent reads the agent's settings.
func ParseAgent(args []string, getenv func(string) string) (*Agent, error) {
	e := &env{get: getenv}
	a := &Agent{}
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	common(fs, e, &a.Common)
	sd := stateDir(e)
	fs.StringVar(&a.Name, "name", e.str("CB_NAME", ""), "this agent's name; must match its identity (env CB_NAME)")
	fs.StringVar(&a.HubPub, "hub-pub", e.str("CB_HUB_PUB", ""), "path of the hub's public bundle, pinned at enrolment (env CB_HUB_PUB)")
	fs.StringVar(&a.TmuxSession, "tmux-session", e.str("CB_TMUX_SESSION", "claude"), "tmux session that receives the text (env CB_TMUX_SESSION)")
	fs.StringVar(&a.StateDir, "state-dir", sd, "directory for the spool, lock and alarm state (env CB_STATE_DIR)")
	fs.BoolVar(&a.AlarmEnabled, "alarm", e.boolean("CB_ALARM", true), "offer the alarm mode (env CB_ALARM)")
	fs.DurationVar(&a.AlarmCooldown, "alarm-cooldown", e.dur("CB_ALARM_COOLDOWN", time.Minute), "at most one report per sensor in this time (env CB_ALARM_COOLDOWN)")
	fs.DurationVar(&a.AlarmPoll, "alarm-poll", e.dur("CB_ALARM_POLL", time.Second), "how often the sensors are read (env CB_ALARM_POLL)")
	fs.DurationVar(&a.Heartbeat, "heartbeat", e.dur("CB_HEARTBEAT", 30*time.Second), "heartbeat interval (env CB_HEARTBEAT)")
	fs.DurationVar(&a.Handshake, "handshake-retry", e.dur("CB_HANDSHAKE_RETRY", 10*time.Second), "how often a hello is repeated until answered (env CB_HANDSHAKE_RETRY)")
	fs.DurationVar(&a.Silence, "silence-limit", e.dur("CB_SILENCE_LIMIT", 0), "no answer from the hub for this long means a new handshake; 0 means 3 x heartbeat (env CB_SILENCE_LIMIT)")
	fs.DurationVar(&a.ReorderWindow, "reorder-window", e.dur("CB_REORDER_WINDOW", 2*time.Second), "how long a command waits for an earlier one that has not arrived before that one is reported lost (env CB_REORDER_WINDOW)")
	fs.DurationVar(&a.ReceiptTimeout, "receipt-timeout", e.dur("CB_RECEIPT_TIMEOUT", 30*time.Second), "a spooled message is sent again if not confirmed in this time (env CB_RECEIPT_TIMEOUT)")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	a.validate(e, !emulator(e))
	if !identity.ValidName(a.Name) {
		e.err = append(e.err, "name: set CB_NAME or -name (lower-case letters, digits, - and _)")
	}
	if a.HubPub == "" {
		e.err = append(e.err, "hub-pub: set CB_HUB_PUB or -hub-pub")
	}
	a.SpoolDir = filepath.Join(a.StateDir, "spool")
	a.LockFile = filepath.Join(a.StateDir, "locked")
	a.AlarmState = filepath.Join(a.StateDir, "alarm-armed")
	a.AlarmCodeState = filepath.Join(a.StateDir, "alarm-code-state")
	if f := getenv("CB_ALARM_SECRET_FILE"); f != "" {
		b32, err := readSecretFile(f, a.AllowGroupRead)
		if err != nil {
			e.err = append(e.err, "CB_ALARM_SECRET_FILE: "+err.Error())
		} else if secret, err := decodeSecret(b32); err != nil {
			e.err = append(e.err, "CB_ALARM_SECRET_FILE: "+err.Error())
		} else {
			a.AlarmSecret = secret
		}
	}
	return a, finish(e)
}
