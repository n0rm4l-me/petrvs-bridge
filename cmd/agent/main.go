// Command agent is the client: it runs next to the tmux session and talks only to Pub/Sub.
//
//	agent [run]                   serve (the default)
//	agent send [-type T] [TEXT]   queue a reply for the hub to show (TEXT, or standard input); for hooks
//	agent send-file [-caption T] PATH   queue a file for the hub to upload to the chat
//	agent prompt                  the Notification hook: queue a notice with the prompt on the screen and its buttons
//	agent unlock                  lift a lockdown; only this machine can do it
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/agentsvc"
	"github.com/n0rm4l-me/petrvs-bridge/internal/alarm"
	"github.com/n0rm4l-me/petrvs-bridge/internal/approval"
	"github.com/n0rm4l-me/petrvs-bridge/internal/config"
	"github.com/n0rm4l-me/petrvs-bridge/internal/identity"
	"github.com/n0rm4l-me/petrvs-bridge/internal/screen"
	"github.com/n0rm4l-me/petrvs-bridge/internal/spool"
	"github.com/n0rm4l-me/petrvs-bridge/internal/sysinfo"
	"github.com/n0rm4l-me/petrvs-bridge/internal/tmuxctl"
	"github.com/n0rm4l-me/petrvs-bridge/internal/transport"
	"github.com/n0rm4l-me/petrvs-bridge/internal/wire"
)

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stdin)) }

func level(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func run(args []string, getenv func(string) string, stdin io.Reader) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		return serve(args, getenv)
	case "send":
		return send(args, getenv, stdin)
	case "unlock":
		return unlock(args, getenv)
	case "send-file":
		return sfSendFile(args, getenv)
	case "prompt":
		return prompt(args, getenv, stdin)
	}
	fmt.Fprintln(os.Stderr, "usage: agent [run] | send [-type reply|notice|alert] [TEXT] | send-file [-caption TEXT] PATH | prompt | unlock")
	return 2
}

// parse reads the settings. When it returns no settings the caller stops with the returned exit code, which is 0 for -h.
func parse(args []string, getenv func(string) string) (*config.Agent, int) {
	cfg, err := config.ParseAgent(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil, 0 // the usage is printed by the configuration code
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 2
	}
	return cfg, 0
}

// localEnv is the environment for the commands that touch only the state directory (send, unlock). They run where the
// agent's full settings may be missing, for example in a Claude Code hook, so what they do not use is faked and the
// secret file is not read.
func localEnv(getenv func(string) string) func(string) string {
	return func(k string) string {
		switch k {
		case "PB_NAME":
			return "x"
		case "PB_IDENTITY", "PB_HUB_PUB", "PB_PROJECT":
			return "unused"
		case "PB_ALARM_SECRET_FILE":
			return ""
		}
		return getenv(k)
	}
}

func send(args []string, getenv func(string) string, stdin io.Reader) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	typ := fs.String("type", "reply", "reply, notice or alert")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// Only the state directory is needed to queue a message, so this works without the full configuration.
	dir := getenv("PB_STATE_DIR")
	if dir == "" {
		cfg, code := parse(nil, localEnv(getenv))
		if cfg == nil {
			return code
		}
		dir = cfg.StateDir
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" || text == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, wire.MaxBody+1))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		text = string(b)
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		fmt.Fprintln(os.Stderr, "error: nothing to send")
		return 2
	}
	sp, err := spool.Open(dir + "/spool")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := sp.Add(wire.Type(*typ), text); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func unlock(args []string, getenv func(string) string) int {
	cfg, code := parse(args, localEnv(getenv))
	if cfg == nil {
		return code
	}
	err := os.Remove(cfg.LockFile)
	switch {
	case err == nil:
		fmt.Printf("unlocked: removed %s; the agent accepts text and keys again\n", cfg.LockFile)
		return 0
	case errors.Is(err, os.ErrNotExist):
		// Nothing was unlocked: this is not the agent's state directory, or its lock is in memory only.
		fmt.Fprintf(os.Stderr, "no lock file at %s (is PB_STATE_DIR the agent's?)\n", cfg.LockFile)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return 1
}

func serve(args []string, getenv func(string) string) int {
	cfg, code := parse(args, getenv)
	if cfg == nil {
		return code
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level(cfg.LogLevel)}))
	id, err := identity.LoadOpts(cfg.Identity, cfg.AllowGroupRead)
	if err != nil {
		log.Error("cannot load the identity", "err", err)
		return 1
	}
	if id.Name != cfg.Name {
		log.Error("the identity file belongs to another name", "identity", id.Name, "configured", cfg.Name)
		return 1
	}
	hub, err := identity.LoadPublic(cfg.HubPub)
	if err != nil {
		log.Error("cannot load the hub bundle", "err", err)
		return 1
	}
	log.Info("agent identity", "name", id.Name, "fingerprint", id.Public().Fingerprint())
	log.Info("pinned hub", "name", hub.Name, "fingerprint", hub.Fingerprint())

	ex, err := tmuxctl.New(cfg.TmuxSession, nil)
	if err != nil {
		log.Error("invalid tmux session name", "err", err)
		return 1
	}
	// Everything the agent keeps (spool, lock, alarm state, used codes) is private to its user.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err == nil {
		err = os.Chmod(cfg.StateDir, 0o700)
		if err != nil {
			log.Warn("cannot restrict the state directory to its owner", "dir", cfg.StateDir, "err", err)
		}
	} else {
		log.Error("cannot create the state directory", "dir", cfg.StateDir, "err", err)
		return 1
	}
	// The hash that binds a button to its prompt is keyed, and so is the one that `agent prompt` makes. A key that cannot be
	// had is not fatal: the hashes stay unkeyed, which the keyed buttons of the hooks do not match, so the agent refuses them.
	if key, err := screen.LoadKey(cfg.StateDir); err != nil {
		log.Warn("cannot read or create the prompt key; buttons made by the hooks will be refused until it can", "err", err)
	} else {
		screen.SetHashKey(key)
	}
	sp, err := spool.Open(cfg.SpoolDir)
	if err != nil {
		log.Error("cannot open the spool", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tr, err := transport.NewPubSub(ctx, cfg.Project)
	if err != nil {
		log.Error("cannot connect to Pub/Sub", "err", err)
		return 1
	}
	defer tr.Close()
	names := wire.Names{Prefix: cfg.Prefix}
	if cfg.CreateResources {
		for _, f := range []func() error{
			func() error { return tr.EnsureTopic(ctx, names.ToHub()) },
			func() error { return tr.EnsureSubscription(ctx, names.ToHub(), names.HubSub(), 30*time.Second) },
			func() error { return tr.EnsureTopic(ctx, names.ToAgent(id.Name)) },
			func() error {
				return tr.EnsureSubscription(ctx, names.ToAgent(id.Name), names.AgentSub(id.Name), 30*time.Second)
			},
		} {
			if err := f(); err != nil {
				log.Error("cannot create topics", "err", err)
				return 1
			}
		}
	}
	host, _ := os.Hostname()
	diskPath := cfg.Workdir // the disk the work is done on; the state directory is on the same one when there is no workdir
	if diskPath == "" {
		diskPath = cfg.StateDir
	}
	acfg := agentsvc.Config{
		Identity: id, Hub: hub, Transport: tr, Names: names, Exec: ex, Spool: sp, LockFile: cfg.LockFile, Log: log,
		HeartbeatEvery: cfg.Heartbeat, HandshakeEvery: cfg.Handshake, SilenceLimit: cfg.Silence, ReceiptTimeout: cfg.ReceiptTimeout,
		ReorderWindow: cfg.ReorderWindow,
		Info:          func() map[string]string { return map[string]string{"host": host, "os": runtime.GOOS} },
		Screen:        ex,
		Ext: agentsvc.Extras{
			Workdir: cfg.Workdir, InboxDir: cfg.InboxDir, StateDir: cfg.StateDir, MaxFileBytes: cfg.MaxFileBytes, StuckAfter: cfg.StuckAfter,
			Heal:  agentsvc.Heal{Enabled: cfg.Heal, Command: cfg.ClaudeCommand, Grace: cfg.HealGrace, MaxTries: cfg.HealTries, Window: cfg.HealWindow},
			Facts: sysinfo.FactsFunc(diskPath),
		},
	}
	if cfg.AlarmEnabled {
		if sensors := alarm.DefaultSensors(); len(sensors) > 0 {
			al, err := alarm.New(alarm.Config{Sensors: sensors, Poll: cfg.AlarmPoll, Cooldown: cfg.AlarmCooldown, StateFile: cfg.AlarmState})
			if err != nil {
				log.Error("cannot set up the alarm", "err", err)
				return 1
			}
			acfg.Alarm = al
			if cfg.AlarmSecret != nil {
				v := approval.NewVerifier(cfg.AlarmSecret)
				if err := v.Persist(cfg.AlarmCodeState, time.Now()); err != nil {
					log.Error("cannot read the record of used alarm codes", "err", err)
					return 1
				}
				acfg.AlarmCode = v
			} else {
				log.Warn("no PB_ALARM_SECRET_FILE: anyone who controls the chat can disarm the alarm")
			}
			log.Info("alarm available", "armed", al.Armed(), "code_to_disarm", cfg.AlarmSecret != nil)
		} else {
			log.Warn("the alarm has no sensors on this operating system")
		}
	}
	ag, err := agentsvc.New(acfg)
	if err != nil {
		log.Error("invalid agent configuration", "err", err)
		return 1
	}
	log.Info("agent running", "tmux_session", cfg.TmuxSession)
	if err := ag.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		return 1
	}
	log.Info("agent stopped")
	return 0
}
